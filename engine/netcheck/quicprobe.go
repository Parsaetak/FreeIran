// quicprobe.go implements the v0.12.1 QUIC diagnostic: a bounded,
// REAL QUIC v1 handshake probe built on quic-go — the minimal
// dedicated QUIC-diagnostic dependency.
//
// Scope discipline (the hard rule):
//
//   - quic-go is a MEASUREMENT dependency only. It lives inside this
//     file, is never used as a dataplane, transport, tunnel or
//     proxy path, and never carries user traffic.
//   - A successful probe is a completed QUIC v1 TLS 1.3 handshake
//     with the HTTP/3 ALPN offered. UDP merely being answerable is
//     NOT "QUIC works" — the UDP tool already measures datagram
//     reachability separately.
//   - The probe is bounded (context deadline + handshake idle
//     timeout), cancellable, has no retries and clears its sockets.
//
// Target resolution: explicit user target → the verified public
// default (www.cloudflare.com:443, a stable HTTP/3 endpoint) →
// invalid_target if the safety layer rejects it. The probe runs on
// the DIRECT path only: the SOCKS tunnel does not relay UDP
// datagrams, and silently swapping the path would fake evidence.
package netcheck

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
)

// quicHandshakeIdleTimeout bounds the QUIC handshake after the last
// packet is sent (independent of, and capped by, the caller context).
const quicHandshakeIdleTimeout = 8 * time.Second

// quicServerAnswered reports whether an error means "the server (or
// some middlebox) actively answered our QUIC Initial" rather than
// silence. A version-negotiation reply, for example, proves the path
// delivers QUIC Initials but that no mutually supported version
// exists — an honest failed, never a silent timeout.
func quicServerAnswered(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()

	return strings.Contains(msg, "version negotiation") ||
		strings.Contains(msg, "no compatible QUIC version")
}

// quicSilence reports whether an error means the peer never answered
// at all ("no recent network activity" is quic-go's handshake-idle
// signature): the tool ran, the path went silent — unreachable.
func quicSilence(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "no recent network activity") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "i/o timeout")
}

// runQUIC executes one bounded QUIC v1 handshake probe.
func (r *ToolRunner) runQUIC(ctx context.Context, req ToolRequest, result *ToolResult) {
	result.Transport = "quic"

	if req.Path == PathTunneled {
		result.Status = ToolStatusUnsupported
		result.Error = "QUIC operates on the direct path only (the SOCKS tunnel does not relay UDP datagrams); run the QUIC probe direct"

		return
	}

	host, port := targetHostPort(req, 443)

	// Resolve → validate → pin (the same rebinding guard the TCP
	// tools get from safeDialer, applied to the UDP socket): every
	// answer is checked against the private-range policy before the
	// socket is created. quic-go never resolves a hostname itself —
	// the probe dials the PINNED address.
	ip := net.ParseIP(host)

	if ip == nil {
		addrs, err := resolveIPAddrs(ctx, host)
		if err != nil {
			setStatusFromError(result, fmt.Errorf("resolve %s: %w", host, err))

			return
		}

		for _, a := range addrs {
			if !IsPrivateIP(a.IP) {
				ip = a.IP

				break
			}
		}

		if ip == nil {
			result.Status = ToolStatusInvalid
			result.Error = fmt.Sprintf("hostname %s resolves only to blocked private addresses", host)

			return
		}
	} else if IsPrivateIP(ip) && !r.Safety.privateTargetsAllowed(req.Tool) {
		result.Status = ToolStatusInvalid
		result.Error = fmt.Sprintf("private destination %s is blocked for this tool", host)

		return
	}

	pinned := &net.UDPAddr{IP: ip, Port: port}

	// TLS identity: hostname gets SNI + verification; an IP-literal
	// target cannot carry SNI (verification then relies on the
	// certificate's IP SANs — a failure is honest evidence, never
	// downgraded).
	serverName := host
	if ip != nil && net.ParseIP(host) != nil {
		serverName = ""
	}

	tlsConf := &tls.Config{
		ServerName:         serverName,
		NextProtos:         []string{"h3"}, // the HTTP/3 ALPN of a real client
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: r.Safety.AllowInsecureTLS, // explicit user opt-in only
	}

	idle := quicHandshakeIdleTimeout
	if d, ok := ctx.Deadline(); ok {
		if remaining := time.Until(d); remaining > 0 && remaining < idle {
			idle = remaining
		}
	}

	qcfg := &quic.Config{
		HandshakeIdleTimeout: idle,
		MaxIdleTimeout:       idle,
	}

	// One local UDP socket for the probe; closed on every path out.
	udpConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		setStatusFromError(result, err)

		return
	}

	defer udpConn.Close()

	started := time.Now()

	qconn, err := quic.Dial(ctx, udpConn, pinned, tlsConf, qcfg)
	elapsed := time.Since(started)

	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			result.Status = ToolStatusCancelled
			result.Error = "cancelled"
		case errors.Is(err, context.DeadlineExceeded):
			result.Status = ToolStatusTimeout
			result.Error = "handshake deadline exceeded"
		case quicSilence(err):
			// The Initial packet went out; nothing verifiable came
			// back. The probe ran — the path is silent. That is
			// unreachable evidence, not a tool crash and not "QUIC
			// unsupported".
			result.Status = ToolStatusUnreachable
			result.Error = "no QUIC answer from " + net.JoinHostPort(host, strconv.Itoa(port)) + ": " + err.Error()
		case quicServerAnswered(err):
			result.Status = ToolStatusFailed
			result.Error = "QUIC path answered but the handshake failed: " + err.Error()
		default:
			setStatusFromError(result, err)
		}

		result.Measurement.Probes = 1

		return
	}

	defer qConnClose(qconn)

	state := qconn.ConnectionState()

	result.Status = ToolStatusOK
	result.Measurement = measurementFor(elapsed)
	result.Measurement.Probes = 1
	result.Details = map[string]string{
		"alpn":        firstNonEmpty(state.TLS.NegotiatedProtocol, "none"),
		"tls_version": tlsVersionLabel(state.TLS.Version),
		"cipher":      tls.CipherSuiteName(state.TLS.CipherSuite),
		"quic":        "v1 handshake completed (HTTP/3 ALPN offered)",
	}
}

// qConnClose closes a completed QUIC connection with the short,
// bounded application error code the diagnostic uses.
func qConnClose(conn *quic.Conn) {
	if conn == nil {
		return
	}

	_ = conn.CloseWithError(quicApplicationDone, "")
}

// quicApplicationDone is the diagnostic's application close code
// (0 = clean, no error implied).
const quicApplicationDone quic.ApplicationErrorCode = 0
