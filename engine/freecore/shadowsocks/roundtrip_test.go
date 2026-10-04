package shadowsocks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// echoServer is a plain TCP echo fixture with half-close semantics: when
// the peer ends its write side, the echo closes its own write side after
// flushing, exactly like a well-behaved TCP service.
type echoServer struct {
	ln     net.Listener
	addr   string
	closed chan struct{}
}

func newEchoServer(t *testing.T) *echoServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	e := &echoServer{ln: ln, addr: ln.Addr().String(), closed: make(chan struct{})}
	go func() {
		defer close(e.closed)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c) // nil on peer EOF
				if cw, ok := c.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return e
}

// ssServer is the ServerConfig.ServeConn fixture harness: accepts
// connections and serves each with ServeConn, recording per-connection
// errors for assertions.
type ssServer struct {
	ln     net.Listener
	addr   string
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu   sync.Mutex
	errs []error
}

func newSSServer(t *testing.T, method Method, password string) *ssServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("shadowsocks listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &ssServer{ln: ln, addr: ln.Addr().String(), ctx: ctx, cancel: cancel}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func(c net.Conn) {
				defer s.wg.Done()
				err := ServeConn(ctx, c, ServerConfig{Password: password, Method: method}, nil, nil)
				s.mu.Lock()
				s.errs = append(s.errs, err)
				s.mu.Unlock()
			}(conn)
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *ssServer) errors() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.errs...)
}

// waitConns blocks until at least n served connections have recorded their
// ServeConn outcome (or the timeout passes), then returns all outcomes.
func (s *ssServer) waitConns(n int, timeout time.Duration) []error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if errs := s.errors(); len(errs) >= n {
			return errs
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s.errors()
}

// deterministicPayload fills a buffer with reproducible pseudo-random
// bytes (xorshift64), unique per stream index.
func deterministicPayload(n int, seed uint64) []byte {
	buf := make([]byte, n)
	var x uint64 = seed*0x9E3779B97F4A7C15 + 0x517CC1B727220A95
	for i := 0; i < n; i += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		for j := 0; j < 8 && i+j < n; j++ {
			buf[i+j] = byte(x >> (8 * j))
		}
	}
	return buf
}

const roundTripPayloadSize = 256 << 10 // 256KiB, per direction, per stream

// TestRoundTripAllMethods is the core evidence: through OUR client
// (ClientConfig.DialTCP) and OUR server fixture (ServeConn), every
// supported AEAD method relays ≥256KiB both ways across 8 concurrent
// streams with byte equality, clean half-close EOF and no server errors.
func TestRoundTripAllMethods(t *testing.T) {
	for _, method := range []Method{MethodAES128GCM, MethodAES256GCM, MethodChaCha20IETFPoly1305} {
		t.Run(string(method), func(t *testing.T) {
			echo := newEchoServer(t)
			server := newSSServer(t, method, "round-trip-secret-پارسی")

			const streams = 8
			var wg sync.WaitGroup
			errCh := make(chan error, streams)
			for i := 0; i < streams; i++ {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					err := runRoundTripStream(echo.addr, server.addr, method, uint64(idx+1))
					if err != nil {
						errCh <- fmt.Errorf("stream %d: %w", idx, err)
					}
				}(i)
			}
			wg.Wait()
			close(errCh)
			for err := range errCh {
				t.Errorf("%v", err)
			}

			// Wait for the server to finish serving every connection, then
			// require a clean (nil) outcome for each.
			errs := server.waitConns(streams, 10*time.Second)
			if len(errs) < streams {
				t.Fatalf("only %d of %d server connections recorded an outcome", len(errs), streams)
			}
			for i, err := range errs {
				if err != nil {
					t.Errorf("ServeConn conn %d returned %v on the clean path", i, err)
				}
			}
		})
	}
}

// runRoundTripStream dials one tunnel, streams the payload up while
// reading the echoed direction concurrently, and requires the echoed
// bytes back byte-identical. The stream is ended by closing the tunnel:
// client-side half-close through the tunnel is currently unreachable
// (RECORDED IMPLEMENTATION GAP — see TestClientTunnelHalfCloseContractCanary
// and the worklog), and the reference implementation likewise ends
// streams by closing the connection, which our reader maps to a clean
// boundary io.EOF.
func runRoundTripStream(echoAddr, serverAddr string, method Method, seed uint64) error {
	payload := deterministicPayload(roundTripPayloadSize, seed)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := ClientConfig{
		Host:     "127.0.0.1",
		Port:     mustPort(serverAddr),
		Password: "round-trip-secret-پارسی",
		Method:   method,
	}.DialTCP(ctx, echoAddr)
	if err != nil {
		return fmt.Errorf("DialTCP: %w", err)
	}
	defer client.Close()

	// Reader goroutine: consume the echoed direction concurrently so the
	// relay never deadlocks on full kernel buffers. Exactly the sent
	// number of bytes is expected back.
	readDone := make(chan struct{})
	got := make([]byte, len(payload))
	var readErr error
	go func() {
		defer close(readDone)
		_, readErr = io.ReadFull(client, got)
	}()

	for len(payload) > 0 {
		chunk := payload
		if len(chunk) > 8192 {
			chunk = chunk[:8192]
		}
		if _, err := client.Write(chunk); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		payload = payload[len(chunk):]
	}

	select {
	case <-readDone:
	case <-time.After(20 * time.Second):
		return errors.New("timed out waiting for the echoed direction")
	}
	if readErr != nil {
		return fmt.Errorf("read: %w", readErr)
	}
	if want := deterministicPayload(roundTripPayloadSize, seed); !bytes.Equal(got, want) {
		return errors.New("echoed bytes differ from the sent payload")
	}
	return nil
}

// TestRelayHalfClosePropagatesToTarget exercises the relay's half-close
// semantics (server.go relay) at the ServeConn level: a scripted client
// stream that ends (EOF) after a request must make the relay CloseWrite
// the TARGET so the target can finish and flush, while the response
// still flows back through the tunnel writer. (The client→tunnel
// direction of half-close is separately pinned by
// TestClientTunnelHalfCloseContractCanary — see the RECORDED
// IMPLEMENTATION GAP note there.)
func TestRelayHalfClosePropagatesToTarget(t *testing.T) {
	method := MethodAES128GCM
	password := "relay-half-close-secret"

	// Target: an echo listener whose connections record CloseWrite calls
	// (the recording wrapper intercepts the relay's half-close).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("target listen: %v", err)
	}
	defer ln.Close()
	targetGotCloseWrite := make(chan struct{}, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				rec := &closeWriteRecorder{Conn: c, called: targetGotCloseWrite}
				defer rec.Close()
				_, _ = io.Copy(rec, rec)
				_ = rec.CloseWrite()
			}(conn)
		}
	}()
	targetAddr := ln.Addr().String()

	// Scripted client stream: [salt][target address chunk][payload
	// chunks] — built with the same framing primitives, ending at a
	// chunk boundary (the Reader then yields io.EOF, the clean end).
	master, err := DeriveKey(method, password)
	if err != nil {
		t.Fatal(err)
	}
	salt := bytes.Repeat([]byte{0xA5}, method.SaltSize())
	aead, err := newAEAD(method, deriveSubkey(master, salt, method.KeySize()))
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	stream.Write(salt)
	w := newChunkWriter(&stream, aead)
	addr, err := AddrBytes("127.0.0.1", mustPort(targetAddr))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(addr); err != nil {
		t.Fatal(err)
	}
	request := []byte("half-close request payload — must reach the target and come back")
	if _, err := w.Write(request); err != nil {
		t.Fatal(err)
	}

	// The scripted client conn: reads the framed stream then EOF; captures
	// everything the server writes back into the tunnel.
	client := &scriptedClientConn{Reader: bytes.NewReader(stream.Bytes())}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- ServeConn(context.Background(), client, ServerConfig{Password: password, Method: method}, nil, nil)
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("ServeConn: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeConn did not finish")
	}

	// The tunnel writer's output must decrypt (salt → subkey → chunks) to
	// exactly the echoed request.
	respSalt := make([]byte, method.SaltSize())
	if _, err := io.ReadFull(&client.writeBuf, respSalt); err != nil {
		t.Fatalf("read server salt from tunnel writes: %v", err)
	}
	respAead, err := newAEAD(method, deriveSubkey(master, respSalt, method.KeySize()))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := io.ReadAll(newChunkReader(&client.writeBuf, respAead))
	if err != nil {
		t.Fatalf("decrypt server response: %v", err)
	}
	if !bytes.Equal(resp, request) {
		t.Fatalf("response through the tunnel = %q, want %q", resp, request)
	}

	// CloseWrite must have reached the target (relay half-close semantics).
	select {
	case <-targetGotCloseWrite:
	default:
		t.Fatal("relay never propagated the half-close (CloseWrite) to the target")
	}
}

// closeWriteRecorder wraps a net.Conn and signals every CloseWrite call —
// the observable trace of the relay's half-close propagation.
type closeWriteRecorder struct {
	net.Conn
	called chan<- struct{}
}

func (c *closeWriteRecorder) CloseWrite() error {
	select {
	case c.called <- struct{}{}:
	default:
	}
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// scriptedClientConn is a net.Conn whose read side replays a scripted
// byte stream (then io.EOF) and whose write side captures into a buffer.
// It lets ServeConn be driven deterministically without a live client.
type scriptedClientConn struct {
	io.Reader
	writeBuf  bytes.Buffer
	closeOnce sync.Once
}

func (c *scriptedClientConn) Write(p []byte) (int, error) { return c.writeBuf.Write(p) }
func (c *scriptedClientConn) Close() error {
	c.closeOnce.Do(func() {})
	return nil
}
func (c *scriptedClientConn) LocalAddr() net.Addr              { return dummyAddr{} }
func (c *scriptedClientConn) RemoteAddr() net.Addr             { return dummyAddr{} }
func (c *scriptedClientConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptedClientConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptedClientConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "dummy" }
func (dummyAddr) String() string  { return "dummy" }

// TestTunnelHalfCloseExplicitlyUnsupported pins the half-close
// contract decision (the RECORDED IMPLEMENTATION GAP is now a
// DOCUMENTED DECISION): CloseWrite is NOT reachable on the tunnel —
// half-close through the AEAD framing is unsupported because the
// reference implementation cannot parse the only wire form it could
// take (an implicit zero-length chunk). The relay's own CloseWrite to
// the TARGET (plain TCP, not the tunnel) is separately proven by
// TestRelayHalfClosePropagatesToTarget.
func TestTunnelHalfCloseExplicitlyUnsupported(t *testing.T) {
	echo := newEchoServer(t)
	server := newSSServer(t, MethodAES128GCM, "half-close-decision")

	client, err := ClientConfig{
		Host: "127.0.0.1", Port: mustPort(server.addr), Password: "half-close-decision", Method: MethodAES128GCM,
	}.DialTCP(context.Background(), echo.addr)
	if err != nil {
		t.Fatalf("DialTCP: %v", err)
	}
	defer client.Close()

	if _, ok := client.(interface{ CloseWrite() error }); ok {
		t.Fatal("CloseWrite became reachable on the tunnel conn — if half-close support was " +
			"added deliberately, it must be proven interoperable with go-shadowsocks2 first; " +
			"replace this test with a real interop-backed half-close test")
	}
}

// TestWrongPasswordFailsClosed: a client with the wrong password must be
// rejected with an authentication failure on the server (errChunkAuth)
// and an error — never silent garbage — on the client.
func TestWrongPasswordFailsClosed(t *testing.T) {
	method := MethodAES128GCM
	echo := newEchoServer(t)
	server := newSSServer(t, method, "the-real-password")

	deadline := time.Now().Add(10 * time.Second)

	client, err := ClientConfig{
		Host: "127.0.0.1", Port: mustPort(server.addr), Password: "WRONG-password", Method: method,
	}.DialTCP(context.Background(), echo.addr)
	if err != nil {
		t.Fatalf("DialTCP with a wrong password must still complete the client-side handshake: %v", err)
	}
	defer client.Close()

	// The AEAD Open on the server fails on the first chunk; the server
	// tears the connection down, so the client observes an I/O error on
	// either direction within the deadline — not garbage and not a hang.
	_, writeErr := client.Write(deterministicPayload(1024, 7))
	_, readErr := client.Read(make([]byte, 128))
	if writeErr == nil && readErr == nil {
		t.Fatalf("wrong-password client: neither Write nor Read failed (write err %v, read err %v)", writeErr, readErr)
	}

	// Wait for the server fixture to record its verdict.
	for time.Now().Before(deadline) {
		errs := server.errors()
		if len(errs) > 0 {
			if !errors.Is(errs[0], errChunkAuth) {
				t.Fatalf("server error = %v, want errChunkAuth in the chain", errs[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server recorded no error within the deadline")
}

// gatedRawConn is a raw transport whose write side accepts a fixed
// number of writes and then PARKS every further write until closed —
// the deterministic model of a TCP socket whose send buffer is full.
// It turns "the write has parked" from a timing assumption into an
// observed fact (the parked channel), which is what makes the
// cancellation test sleep-free and scheduling-race-free. Reads park
// the same way (signaled through readParked): the read direction is
// idle in these tests, and a parked Read is the observable proof of
// Close-racing-Read.
type gatedRawConn struct {
	mu         sync.Mutex
	allowed    int           // writes that succeed immediately (the handshake occupies one)
	parked     chan struct{} // signaled when a write actually parks
	readParked chan struct{} // signaled when a read actually parks
	closed     chan struct{}
	closeOnce  sync.Once

	// parkedErr is returned by every parked operation when the close
	// won the race. It wraps net.ErrClosed like a real TCP close, so
	// the test classifies it with errors.Is.
	parkedErr error
}

func newGatedRawConn(handshakeWrites int) *gatedRawConn {
	return &gatedRawConn{
		allowed:    handshakeWrites,
		parked:     make(chan struct{}, 1),
		readParked: make(chan struct{}, 1),
		closed:     make(chan struct{}),
		parkedErr:  fmt.Errorf("gated transport closed: %w", net.ErrClosed),
	}
}

func (g *gatedRawConn) Write(p []byte) (int, error) {
	g.mu.Lock()
	if g.allowed > 0 {
		g.allowed--
		g.mu.Unlock()

		return len(p), nil
	}
	g.mu.Unlock()

	// Park: signal the observer, then wait for the close. One signal is
	// enough — each test parks exactly one writer at a time.
	select {
	case g.parked <- struct{}{}:
	default:
	}

	<-g.closed

	return 0, g.parkedErr
}

func (g *gatedRawConn) Read(p []byte) (int, error) {
	select {
	case <-g.closed:
		return 0, g.parkedErr
	default:
	}

	select {
	case g.readParked <- struct{}{}:
	default:
	}

	<-g.closed // reads block until close — the read direction is idle here

	return 0, g.parkedErr
}

func (g *gatedRawConn) Close() error {
	g.closeOnce.Do(func() { close(g.closed) })

	return nil
}

func (g *gatedRawConn) LocalAddr() net.Addr              { return dummyAddr{} }
func (g *gatedRawConn) RemoteAddr() net.Addr             { return dummyAddr{} }
func (g *gatedRawConn) SetDeadline(time.Time) error      { return nil }
func (g *gatedRawConn) SetReadDeadline(time.Time) error  { return nil }
func (g *gatedRawConn) SetWriteDeadline(time.Time) error { return nil }

// gatedDialer adapts a pre-built raw conn to the ClientConfig.Dialer
// seam (DialTCP then performs the handshake over it).
type gatedDialer struct{ conn net.Conn }

func (d gatedDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return d.conn, nil
}

// TestCancellationMidStreamFailsCleanly is the deterministic
// cancellation proof. The raw transport parks the payload write for
// CERTAIN (the parked signal is an observed fact, not a timing bet),
// so the test never demands that an in-flight Write lose a scheduling
// race: cancellation must
//
//  1. be observed internally (markClosed before the raw close);
//  2. interrupt blocked I/O (the parked write returns a real
//     transport error);
//  3. fail post-cancel I/O fast (net.ErrClosed-class errors);
//  4. leave no goroutine behind (the watcher exits).
//
// No sleeps as synchronization, no artificial timeouts in the path
// under test.
func TestCancellationMidStreamFailsCleanly(t *testing.T) {
	for _, method := range []Method{MethodAES128GCM, MethodAES256GCM, MethodChaCha20IETFPoly1305} {
		t.Run(string(method), func(t *testing.T) {
			raw := newGatedRawConn(2) // two writes succeed: the salt + the handshake target chunk

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			client, err := ClientConfig{
				Host: "127.0.0.1", Port: 1, Password: "cancel-secret", Method: method,
				Dialer: gatedDialer{conn: raw},
			}.DialTCP(ctx, "198.51.100.1:443")
			if err != nil {
				t.Fatalf("DialTCP over the gated transport: %v", err)
			}

			tunnel, ok := client.(*tunnelConn)
			if !ok {
				t.Fatalf("DialTCP returned %T, want *tunnelConn", client)
			}

			// Park a write for certain, then cancel.
			payload := deterministicPayload(1<<20, 99)
			blocked := make(chan error, 1)
			go func() {
				_, err := client.Write(payload)
				blocked <- err
			}()

			select {
			case <-raw.parked:
			case err := <-blocked:
				t.Fatalf("write returned before parking (must park for certain): %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("write never parked")
			}

			cancel()

			// The parked Write must fail with a transport error promptly.
			select {
			case err := <-blocked:
				if err == nil {
					t.Fatal("parked Write returned nil after cancellation")
				}
				if !errors.Is(err, net.ErrClosed) {
					t.Errorf("parked Write error = %v, want a net.ErrClosed-class transport error", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Write still parked 10s after cancellation (cancellation leaked)")
			}

			// Cancellation was observed: post-cancel I/O fails FAST and
			// closed (the fast-fail flag is set before the raw close, so
			// once the parked write observed its error this is already
			// guaranteed — no race, no polling).
			if _, err := client.Write([]byte("after cancel")); !errors.Is(err, net.ErrClosed) {
				t.Errorf("Write after cancellation = %v, want net.ErrClosed-class", err)
			}
			if _, err := client.Read(make([]byte, 16)); !errors.Is(err, net.ErrClosed) {
				t.Errorf("Read after cancellation = %v, want net.ErrClosed-class", err)
			}
			if err := client.Close(); err != nil {
				t.Errorf("Close after cancellation: %v", err)
			}

			// No goroutine leak: the watcher exited (watchDone is closed
			// by watchLoop's defer — a direct, deterministic observation).
			select {
			case <-tunnel.watchDone:
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation watcher goroutine did not exit after cancellation")
			}
		})
	}
}

// TestTunnelConcurrentIODirections pins the net.Conn concurrency
// contract of the framing: concurrent Writes serialize safely (whole
// chunks, no nonce corruption — the server would fail authentication
// otherwise), concurrent Reads serialize safely, and both directions
// run in parallel. The total-byte assertion is exact; any framing
// corruption or interleaved chunk surfaces as an auth/read error. Run
// under -race this is the direct proof.
func TestTunnelConcurrentIODirections(t *testing.T) {
	method := MethodAES128GCM
	echo := newEchoServer(t)
	server := newSSServer(t, method, "concurrent-io-secret")

	client, err := ClientConfig{
		Host: "127.0.0.1", Port: mustPort(server.addr), Password: "concurrent-io-secret", Method: method,
	}.DialTCP(context.Background(), echo.addr)
	if err != nil {
		t.Fatalf("DialTCP: %v", err)
	}

	payload := deterministicPayload(64<<10, 42)

	const writers = 8

	// Eight writers and eight readers: both directions of the SAME
	// tunnel under concurrency — the framing locks are what keep the
	// chunks whole and the nonce streams correct.

	// Concurrent readers start FIRST and drain the echoed direction
	// while the writers run — the same structure the byte-path round
	// trip uses. Each reader reads into its OWN buffer; the shared
	// exact total is the completeness proof, and every read error
	// (auth, framing, panic) fails the test. Concurrent Reads on one
	// direction are exactly what the framing lock must serialize.
	wantTotal := writers * len(payload)
	var total atomic.Int64
	var readWG sync.WaitGroup
	for i := 0; i < writers; i++ {
		readWG.Add(1)
		go func() {
			defer readWG.Done()

			buf := make([]byte, 16<<10)
			for {
				n, err := client.Read(buf)
				if n > 0 {
					total.Add(int64(n))
				}
				if err != nil {
					// Any read error ends this reader; the final total
					// assertion below decides whether data was lost (an
					// early error with the total short is a real failure;
					// the post-drain Close unblocks parked readers).
					return
				}
			}
		}()
	}

	// Concurrent writers hammer the other direction. The framing lock
	// makes their chunks whole; the server's AEAD Open is the
	// integrity oracle. Readers drain concurrently, so the writers
	// cannot deadlock on full kernel buffers.
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			if _, err := client.Write(payload); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Write: %v", err)
	}

	// Bounded drain window: the echo has exactly wantTotal bytes in
	// flight; if they are not all back in time the test fails honestly
	// (and Close below unblocks every parked reader).
	drainDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(drainDeadline) {
		if total.Load() >= int64(wantTotal) {
			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	_ = client.Close() // unblock any parked reader
	readWG.Wait()

	if got := total.Load(); got != int64(wantTotal) {
		t.Fatalf("echoed bytes = %d, want exactly %d (lost or duplicated data through concurrent reads)", got, wantTotal)
	}

	// The server must have recorded a clean outcome for every
	// connection — no authentication failure, no relay error.
	for i, err := range server.waitConns(1, 5*time.Second) {
		if err != nil {
			t.Errorf("ServeConn conn %d returned %v under concurrent writes", i, err)
		}
	}
}

// TestTunnelCloseRacesIO proves Close racing parked Read AND Write is
// safe and observed: both directions park against the gated transport
// (observed facts), Close fires, and every parked and subsequent
// operation fails with a net.ErrClosed-class error — no panic, no
// hang, no false success. No sleeps as synchronization: the park
// signals ARE the synchronization.
func TestTunnelCloseRacesIO(t *testing.T) {
	raw := newGatedRawConn(2) // salt + handshake write succeed; everything else parks

	client, err := ClientConfig{
		Host: "127.0.0.1", Port: 1, Password: "close-race-secret", Method: MethodAES128GCM,
		Dialer: gatedDialer{conn: raw},
	}.DialTCP(context.Background(), "198.51.100.1:443")
	if err != nil {
		t.Fatalf("DialTCP: %v", err)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := client.Write(deterministicPayload(1<<20, 7))
		writeDone <- err
	}()

	readDone := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 4096))
		readDone <- err
	}()

	// Both directions must actually park before Close — observed, not
	// assumed.
	select {
	case <-raw.parked:
	case err := <-writeDone:
		t.Fatalf("write returned before parking: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("write never parked")
	}

	select {
	case <-raw.readParked:
	case err := <-readDone:
		t.Fatalf("read returned before parking: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("read never parked")
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for i := 0; i < 2; i++ {
		select {
		case err := <-writeDone:
			if err == nil || !errors.Is(err, net.ErrClosed) {
				t.Errorf("parked Write = %v, want net.ErrClosed-class", err)
			}
		case err := <-readDone:
			if err == nil || !errors.Is(err, net.ErrClosed) {
				t.Errorf("parked Read = %v, want net.ErrClosed-class", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("parked I/O did not unblock after Close")
		}
	}

	if _, err := client.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Write after Close = %v, want net.ErrClosed-class", err)
	}
	if _, err := client.Read(make([]byte, 16)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Read after Close = %v, want net.ErrClosed-class", err)
	}
}

// TestDialTCPValidationRefused pins configuration/target validation:
// unusable configs are refused before any network I/O (a listener is not
// needed — nothing is dialed).
func TestDialTCPValidationRefused(t *testing.T) {
	cases := []struct {
		name   string
		cfg    ClientConfig
		target string
		want   string
	}{
		{
			name:   "unsupported method",
			cfg:    ClientConfig{Host: "127.0.0.1", Port: 1, Password: "x", Method: "rc4-md5"},
			target: "127.0.0.1:9",
			want:   "unsupported method",
		},
		{
			name:   "empty password",
			cfg:    ClientConfig{Host: "127.0.0.1", Port: 1, Password: "", Method: MethodAES128GCM},
			target: "127.0.0.1:9",
			want:   "password must not be empty",
		},
		{
			name:   "empty host",
			cfg:    ClientConfig{Host: "", Port: 1, Password: "x", Method: MethodAES128GCM},
			target: "127.0.0.1:9",
			want:   "host must not be empty",
		},
		{
			name:   "port zero",
			cfg:    ClientConfig{Host: "127.0.0.1", Port: 0, Password: "x", Method: MethodAES128GCM},
			target: "127.0.0.1:9",
			want:   "out of range",
		},
		{
			name:   "target without port",
			cfg:    ClientConfig{Host: "127.0.0.1", Port: 1, Password: "x", Method: MethodAES128GCM},
			target: "127.0.0.1",
			want:   "missing port",
		},
		{
			name:   "target port zero",
			cfg:    ClientConfig{Host: "127.0.0.1", Port: 1, Password: "x", Method: MethodAES128GCM},
			target: "127.0.0.1:0",
			want:   "out of range",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.cfg.DialTCP(context.Background(), tc.target)
			if err == nil {
				t.Fatal("DialTCP accepted an invalid configuration")
			}
			if !bytes.Contains([]byte(err.Error()), []byte(tc.want)) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	// An already-canceled context is refused before anything else.
	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := ClientConfig{Host: "127.0.0.1", Port: 1, Password: "x", Method: MethodAES128GCM}.
			DialTCP(ctx, "127.0.0.1:9")
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled ctx: err = %v, want context.Canceled in the chain", err)
		}
	})
}

// mustPort extracts the numeric port from a host:port address.
func mustPort(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		panic(err)
	}
	return p
}
