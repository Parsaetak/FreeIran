package shadowsocks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// defaultTimeout bounds one dial and the client handshake when
// ClientConfig.Timeout is unset.
const defaultTimeout = 15 * time.Second

// Dialer matches freecore's dial seam structurally (deliberately without
// importing the freecore package, so this package stays leaf-level).
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// ClientConfig configures the Shadowsocks AEAD TCP client.
type ClientConfig struct {
	// Host is the remote Shadowsocks server host (IP or domain).
	Host string
	// Port is the remote Shadowsocks server port (1..65535).
	Port int
	// Password is the shared secret; the master key is derived from it.
	Password string
	// Method selects the AEAD cipher suite.
	Method Method
	// Dialer dials the server connection; nil uses the plain net.Dialer
	// with a sane timeout.
	Dialer Dialer
	// Timeout bounds the dial and the handshake (salt + target write);
	// 0 means 15s.
	Timeout time.Duration
}

// DialTCP establishes the Shadowsocks AEAD tunnel to the configured
// server and requests targetAddress ("host:port", host may be domain or
// IP). The returned net.Conn carries plaintext after the framing layer:
// Writes are chunked and encrypted, Reads are authenticated and
// decrypted, nonce counters per direction are maintained internally.
//
// The tunnel performs the handshake eagerly (salt + first chunk carrying
// the target address), bounded by the configured timeout, and the
// returned conn propagates ctx cancellation: when ctx is done, pending
// and subsequent I/O on the tunnel fail with a transport error.
func (c ClientConfig) DialTCP(ctx context.Context, targetAddress string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("shadowsocks: dial: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}

	host, portStr, err := net.SplitHostPort(targetAddress)
	if err != nil {
		return nil, fmt.Errorf("shadowsocks: target %q: %w", targetAddress, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("shadowsocks: target port %q: %w", portStr, err)
	}
	target, err := AddrBytes(host, port)
	if err != nil {
		return nil, err
	}

	master, err := DeriveKey(c.Method, c.Password)
	if err != nil {
		return nil, err
	}

	raw, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}

	// Handshake: [salt][first chunk = target address], bounded by the
	// timeout so a stalled server or link cannot hold the dial hostage.
	// The deadline is cleared once the handshake is on the wire — the
	// caller owns steady-state timing.
	if err := raw.SetDeadline(time.Now().Add(c.timeout())); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("shadowsocks: handshake deadline: %w", err)
	}

	salt, err := newSalt(c.Method.SaltSize())
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	if err := writeAll(raw, salt); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("shadowsocks: send salt: %w", err)
	}

	subkey := deriveSubkey(master, salt, c.Method.KeySize())
	aead, err := newAEAD(c.Method, subkey)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}

	tunnel := &tunnelConn{
		Conn:   raw,
		writer: newChunkWriter(raw, aead),
		// The read direction derives its subkey from the server's salt,
		// which only arrives when the server first writes — mirror the
		// reference implementation's lazy writer with a lazy reader.
		reader: &saltedReader{conn: raw, method: c.Method, master: master},
	}
	if _, err := tunnel.writer.Write(target); err != nil {
		_ = tunnel.Close()
		return nil, fmt.Errorf("shadowsocks: send target: %w", err)
	}
	_ = raw.SetDeadline(time.Time{})

	tunnel.watch(ctx)
	return tunnel, nil
}

// validate rejects unusable configurations before any I/O happens.
func (c ClientConfig) validate() error {
	if _, err := ParseMethod(string(c.Method)); err != nil {
		return err
	}
	if c.Password == "" {
		return errEmptyPassword
	}
	if c.Host == "" {
		return fmt.Errorf("shadowsocks: server host must not be empty")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("shadowsocks: server port %d out of range", c.Port)
	}
	return nil
}

func (c ClientConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

// dial connects to the Shadowsocks server through the configured Dialer
// (or the plain net.Dialer with a sane timeout), with ctx propagation.
func (c ClientConfig) dial(ctx context.Context) (net.Conn, error) {
	address := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	dialer := c.Dialer
	if dialer == nil {
		dialer = &net.Dialer{Timeout: c.timeout(), KeepAlive: 30 * time.Second}
	}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("shadowsocks: dial %s: %w", address, err)
	}
	return conn, nil
}

// tunnelConn is the plaintext view of an AEAD tunnel. It embeds the
// raw net.Conn so Close and deadline handling reach the underlying
// transport, while Read/Write go through the framing layer.
//
// Concurrency contract (net.Conn permits concurrent method calls):
//
//   - concurrent Reads / concurrent Writes are serialized per direction
//     by the framing locks (chunkReader.mu / chunkWriter.mu); the two
//     directions keep fully separate state and stay parallel;
//   - Close racing Read/Write is safe: the close flag is atomic, and
//     closing the raw transport unblocks parked I/O with the real
//     transport error;
//   - cancellation (ctx done) marks the tunnel closed and closes the
//     raw transport, so blocked I/O is interrupted and new I/O fails
//     fast with a net.ErrClosed-wrapped error.
//
// Half-close: deliberately UNSUPPORTED. CloseWrite is not reachable on
// a tunnelConn, and no zero-length chunk is ever emitted implicitly —
// the reference implementation (go-shadowsocks2) cannot parse one
// (it would block waiting for a payload tag that never arrives).
// End-of-stream is signaled by closing the connection, exactly like
// the reference implementation; the read side still honors an explicit
// zero-length chunk per the spec.
type tunnelConn struct {
	net.Conn
	reader tunnelReader // client: lazy peer-salt init; server: concrete chunk reader
	writer *chunkWriter

	watchStop chan struct{}
	watchOnce sync.Once
	closeOnce sync.Once

	// watchDone is closed when the cancellation watcher returns —
	// the observable proof that the watcher never outlives the
	// connection it guards (the leak assertion in tests).
	watchDone chan struct{}

	// done is set before the raw transport is closed (Close and the
	// cancellation watcher both go through markClosed). Once set, new
	// Read/Write calls fail fast; in-flight I/O is interrupted by the
	// raw transport close.
	done atomic.Bool
}

// errTunnelClosed is the post-close/post-cancel fast-fail error. It
// wraps net.ErrClosed so callers can classify it with errors.Is like
// any other closed-transport failure.
var errTunnelClosed = fmt.Errorf("shadowsocks: tunnel closed: %w", net.ErrClosed)

// tunnelReader is the read half of the framing: the concrete chunkReader
// (server side, salt already consumed) or the lazy saltedReader (client
// side, peer salt arrives with the peer's first write).
type tunnelReader interface {
	Read(p []byte) (int, error)
}

func (c *tunnelConn) Read(p []byte) (int, error) {
	if c.done.Load() {
		return 0, errTunnelClosed
	}

	return c.reader.Read(p)
}

func (c *tunnelConn) Write(p []byte) (int, error) {
	if c.done.Load() {
		return 0, errTunnelClosed
	}

	return c.writer.Write(p)
}

// markClosed flips the fast-fail flag BEFORE the raw transport is
// closed: once any observer sees the raw close (a returned I/O error),
// new operations are already guaranteed to fail fast instead of racing
// the teardown.
func (c *tunnelConn) markClosed() { c.done.Store(true) }

// Close releases the tunnel and stops the context watcher. It is
// honest-idempotent: closing an ALREADY-closed transport (the ctx
// watcher races the caller's Close by design) reports nil, not the
// "use of closed connection" noise; any other close error is real and
// surfaces.
func (c *tunnelConn) Close() error {
	c.closeOnce.Do(func() {
		c.markClosed()
		c.stopWatch()
	})

	if err := c.Conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}

	return nil
}

// watch unblocks any pending tunnel I/O when ctx is done: the watcher
// marks the tunnel closed and closes the raw transport, which
// interrupts every blocked Read/Write with the real transport error.
// Closing (rather than a deadline) is deliberate: a canceled tunnel
// must also make the PEER's relay notice immediately, which only a
// real close propagates. The watcher stops itself when the tunnel is
// closed so it never outlives the connection it guards.
func (c *tunnelConn) watch(ctx context.Context) {
	if ctx == nil || ctx.Done() == nil {
		return // nothing to watch; Close still cleans up
	}
	c.watchStop = make(chan struct{})
	c.watchDone = make(chan struct{})
	go c.watchLoop(ctx)
}

// watchLoop is the watcher body. It exits when the tunnel closes OR
// ctx is done, closing watchDone on the way out — one goroutine per
// tunnel, never a leak.
func (c *tunnelConn) watchLoop(ctx context.Context) {
	defer close(c.watchDone)

	select {
	case <-ctx.Done():
		c.markClosed()
		_ = c.Conn.Close()
	case <-c.watchStop:
	}
}

func (c *tunnelConn) stopWatch() {
	if c.watchStop != nil {
		c.watchOnce.Do(func() { close(c.watchStop) })
	}
}

// saltedReader lazily reads the peer's salt and derives the read-direction
// subkey on first Read. Lazy because the peer only sends its salt when it
// first writes (both the reference implementation and our server fixture
// initialize their writer on first use), so blocking for it earlier would
// hang one-way tunnels. once guards the init even if Read were called
// concurrently, which the net.Conn contract does not require but allows
// for defensively.
type saltedReader struct {
	conn   io.Reader
	method Method
	master []byte

	once  sync.Once
	inner *chunkReader
	err   error
}

func (r *saltedReader) Read(p []byte) (int, error) {
	r.once.Do(r.init)
	if r.err != nil {
		return 0, r.err
	}
	return r.inner.Read(p)
}

func (r *saltedReader) init() {
	salt := make([]byte, r.method.SaltSize())
	if _, err := io.ReadFull(r.conn, salt); err != nil {
		r.err = fmt.Errorf("shadowsocks: read peer salt: %w", err)
		return
	}
	aead, err := newAEAD(r.method, deriveSubkey(r.master, salt, r.method.KeySize()))
	if err != nil {
		r.err = err
		return
	}
	r.inner = newChunkReader(r.conn, aead)
}
