package freecore

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// HTTPConnectInbound serves the local HTTP proxy surface:
//
//   - CONNECT host:port — the tunneling form every browser/proxy
//     client uses for https:// targets;
//   - absolute-form requests (GET http://host/path) — the plain-HTTP
//     forwarding form WinINet and friends use for http:// targets.
//
// Both carry real bytes through the first-party pipeline.
type HTTPConnectInbound struct {
	// Engine is the serving engine (session registry + pipeline).
	Engine *Engine
}

// Name identifies the inbound in session metadata.
func (h *HTTPConnectInbound) Name() string { return "http-connect" }

// Serve handles one accepted connection.
func (h *HTTPConnectInbound) Serve(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(h.Engine.handshakeTimeout)); err != nil {
		return
	}

	// Bounded request-head read: a stalling client cannot pin the
	// engine's memory with an endless header.
	reader := bufio.NewReader(io.LimitReader(conn, DefaultHeaderLimit))

	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}

	switch {
	case request.Method == http.MethodConnect:
		h.serveConnect(ctx, conn, request)
	case request.URL.IsAbs():
		h.serveAbsolute(ctx, conn, reader, request)
	default:
		// Origin-form on a proxy port is not a proxy request: the
		// honest answer is 431/400-class, not a hang.
		writeSimpleResponse(conn, http.StatusBadRequest,
			"freecore: origin-form requests are not accepted on the proxy port")
	}
}

// serveConnect tunnels a CONNECT request.
func (h *HTTPConnectInbound) serveConnect(ctx context.Context, conn net.Conn, request *http.Request) {
	host := request.URL.Host
	if host == "" {
		host = request.Host
	}

	if _, _, err := net.SplitHostPort(host); err != nil {
		writeSimpleResponse(conn, http.StatusBadRequest, "freecore: CONNECT target missing port")

		return
	}

	// The handshake deadline no longer applies once the pipe opens.
	_ = conn.SetDeadline(time.Time{})

	h.Engine.pipe(ctx, conn, h.Name(), host,
		func(net.Addr) error {
			_, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

			return err
		},
		func(error) {
			writeSimpleResponse(conn, http.StatusBadGateway, "freecore: upstream unavailable")
		},
	)
}

// serveAbsolute forwards one absolute-form request through the
// first-party pipeline. Phase 1 serves one request per connection
// with Connection: close semantics: the forwarded request forces
// close so the relayed response ends the connection deterministically
// (documented; proxy clients re-open per request).
func (h *HTTPConnectInbound) serveAbsolute(ctx context.Context, conn net.Conn, reader *bufio.Reader, request *http.Request) {
	target := request.URL.Host
	if target == "" {
		writeSimpleResponse(conn, http.StatusBadRequest, "freecore: absolute-form request without authority")

		return
	}

	// Normalize the target into host:port (default scheme port).
	scheme := strings.ToLower(request.URL.Scheme)
	if scheme != "http" && scheme != "https" {
		writeSimpleResponse(conn, http.StatusBadRequest, "freecore: unsupported scheme")

		return
	}

	if _, _, err := net.SplitHostPort(target); err != nil {
		if scheme == "https" {
			target = net.JoinHostPort(target, "443")
		} else {
			target = net.JoinHostPort(target, "80")
		}
	}

	_ = conn.SetDeadline(time.Time{})

	session, sctx, end, err := h.Engine.beginSession(ctx)
	if err != nil {
		writeSimpleResponse(conn, http.StatusServiceUnavailable, "freecore: session limit reached")

		return
	}

	defer end()

	session.Inbound = h.Name()
	session.Target = target

	outbound, decision := h.Engine.outboundDecision()
	session.Outbound = decision.Outbound

	upstream, err := outbound.Dial(sctx, target)
	if err != nil {
		writeSimpleResponse(conn, http.StatusBadGateway, "freecore: upstream unavailable")

		return
	}

	defer func() { _ = upstream.Close() }()

	// Rewrite to origin-form, drop hop-by-hop headers, force close.
	request.URL.Scheme = ""
	request.URL.Host = ""
	request.Header.Del("Proxy-Authorization")
	request.Header.Del("Proxy-Connection")
	request.Header.Set("Connection", "close")

	if err := request.Write(upstream); err != nil {
		writeSimpleResponse(conn, http.StatusBadGateway, "freecore: upstream write failed")

		return
	}

	// Relay the exchange: any request-body bytes the client already
	// pipelined past the head (buffered in reader) plus everything
	// that follows flow upstream; the response flows back verbatim.
	// The forced Connection: close ends the response deterministically.
	relay(sctx, session, conn, io.MultiReader(reader, conn), upstream)
}

// writeSimpleResponse writes a minimal response and closes the
// exchange for this connection.
func writeSimpleResponse(conn net.Conn, status int, message string) {
	response := &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(message)),
		ContentLength: int64(len(message)),
	}

	// The connection is closing: say so explicitly.
	response.Header.Set("Connection", "close")

	_ = response.Write(conn)
}
