package shadowsocks

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"
)

// serverHandshakeTimeout bounds the server fixture's handshake (salt,
// first chunk, target parse) when the caller's ctx carries no deadline.
// A client that stalls mid-handshake must not hold a goroutine and a
// socket forever.
const serverHandshakeTimeout = 15 * time.Second

// ServerConfig configures the in-process reference Shadowsocks server
// fixture. It implements the framing correctly (salt → subkey, per-
// direction nonce counters, chunked AEAD) and serves as the
// reference-server role inside tests and future integration points.
type ServerConfig struct {
	Password string
	Method   Method
}

// ServeConn accepts one framed client connection on conn, parses the
// target address from the first chunk, optionally validates it, dials
// the target through dialer (nil → plain net.Dialer with a sane timeout)
// and relays bidirectionally until either side ends or ctx is canceled.
//
// The connection is closed on return; ownership of conn passes to
// ServeConn. Failures fail closed: an authentication failure or an
// oversized chunk ends the connection and is returned as a real error
// (errors.Is-able against errChunkAuth for auth failures), never silently
// swallowed. The returned error is for diagnostics only — the wire-level
// consequence is always a closed connection.
func ServeConn(ctx context.Context, conn net.Conn, cfg ServerConfig, dialer Dialer, targetValidator func(target string) bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	defer conn.Close()

	if _, err := ParseMethod(string(cfg.Method)); err != nil {
		return err
	}
	master, err := DeriveKey(cfg.Method, cfg.Password)
	if err != nil {
		return err
	}

	// Bound the handshake: salt, first chunk and target parse must
	// complete in time. A caller-supplied earlier ctx deadline wins.
	deadline := time.Now().Add(serverHandshakeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("shadowsocks: handshake deadline: %w", err)
	}

	// Read direction: subkey derived from the client's salt.
	salt := make([]byte, cfg.Method.SaltSize())
	if _, err := io.ReadFull(conn, salt); err != nil {
		return fmt.Errorf("shadowsocks: read salt: %w", err)
	}
	aeadIn, err := newAEAD(cfg.Method, deriveSubkey(master, salt, cfg.Method.KeySize()))
	if err != nil {
		return err
	}
	reader := newChunkReader(conn, aeadIn)

	target, err := readTarget(reader)
	if err != nil {
		return err
	}
	if targetValidator != nil && !targetValidator(target) {
		return fmt.Errorf("shadowsocks: target %s rejected by validator", target)
	}

	// Write direction: our own salt and subkey, one counter pair per
	// direction. The salt must precede the first chunk to the client.
	saltOut, err := newSalt(cfg.Method.SaltSize())
	if err != nil {
		return err
	}
	if err := writeAll(conn, saltOut); err != nil {
		return fmt.Errorf("shadowsocks: send salt: %w", err)
	}
	aeadOut, err := newAEAD(cfg.Method, deriveSubkey(master, saltOut, cfg.Method.KeySize()))
	if err != nil {
		return err
	}

	targetConn, err := dialTarget(ctx, dialer, target)
	if err != nil {
		return fmt.Errorf("shadowsocks: dial target %s: %w", target, err)
	}
	defer targetConn.Close()

	// Handshake complete; steady-state timing belongs to the relay.
	_ = conn.SetDeadline(time.Time{})

	tunnel := &tunnelConn{
		Conn:   conn,
		reader: reader, // concrete chunk reader: the client salt is consumed already
		writer: newChunkWriter(conn, aeadOut),
	}

	return relay(ctx, tunnel, targetConn)
}

// dialTarget connects to the parsed target through the provided dialer.
func dialTarget(ctx context.Context, dialer Dialer, target string) (net.Conn, error) {
	if dialer == nil {
		dialer = &net.Dialer{Timeout: defaultTimeout, KeepAlive: 30 * time.Second}
	}
	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// relay pumps both tunnel directions concurrently until both end.
//
// Half-close semantics: when the tunnel (client) side of the stream ends,
// the target's write side is closed so the target can finish and flush —
// the target→client direction keeps pumping until it ends too, which is
// what lets a client that closed its write side still receive all pending
// data. When ctx is canceled both transports are closed to unblock any
// parked I/O immediately; the caller observes a clean error, never a
// panic or a hang.
func relay(ctx context.Context, client, target net.Conn) error {
	errc := make(chan error, 2)

	go func() {
		// client → target. io.Copy returns nil on EOF; a clean end of the
		// tunnel stream propagates as a half-close towards the target.
		_, err := io.Copy(target, client)
		if err == nil {
			if cw, ok := target.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
		}
		errc <- err
	}()

	go func() {
		// target → client.
		_, err := io.Copy(client, target)
		errc <- err
	}()

	relayDone := make(chan struct{})
	defer close(relayDone)
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				// Unblock both pumps; ServeConn's defers do the final close.
				_ = client.Close()
				_ = target.Close()
			case <-relayDone:
			}
		}()
	}

	err1 := <-errc
	err2 := <-errc
	if ctx.Err() != nil {
		return fmt.Errorf("shadowsocks: relay canceled: %w", ctx.Err())
	}
	for _, err := range []error{err1, err2} {
		if err != nil {
			return fmt.Errorf("shadowsocks: relay: %w", err)
		}
	}
	return nil
}
