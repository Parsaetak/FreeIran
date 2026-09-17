// Command fakecore is the deterministic test stand-in for the real
// protocol-core binaries (Xray, V2Ray, sing-box). It emulates every
// operation the registry, the process manager and the adapters rely
// on, so the full test matrix runs without installing real cores:
//
//	version | --version | -version
//	    print a version line and exit 0 (registry version probe)
//	check -c <file>
//	    validate the runtime configuration document and exit
//	(implicit run) -c <file>
//	    parse the document, bind the declared local inbound and stay
//	    alive until signalled (readiness = TCP listener accept loop)
//
// Reading the port from the generated document — exactly like real
// cores do — makes the helper verify that adapters actually emit the
// inbound they claim to emit.
//
// Failure injection through environment variables:
//
//	FAKECORE_FAIL_FAST=1          exit(3) before listening
//	FAKECORE_CRASH_AFTER_START=1  exit(5) 300ms after startup
//	FAKECORE_HANG=1               never listen, never exit (timeout paths)
//
// v0.9.6 verification mode (connection-engine VERIFY + racing tests):
//
//	FAKECORE_SOCKS_RELAY=h:p      speak minimal SOCKS5 on the inbound
//	                               listener and relay every CONNECT to
//	                               the given upstream address, so the
//	                               tunnel-verification path (a real
//	                               HTTP request through the tunnel)
//	                               can be exercised end-to-end.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	args := os.Args[1:]

	// Version probe forms used by system.CoreLocator. The fake core
	// answers all of them so discovery reports a version like the real
	// cores do.
	if len(args) == 1 && isVersionArg(args[0]) {
		fmt.Printf("fakecore %s (%s)\n", version(), invocationName())
		return
	}

	// "check" validates the document without starting (sing-box form).
	runMode := false

	if len(args) > 0 && args[0] == "check" {
		args = args[1:]
	} else {
		runMode = true
	}

	configPath := findConfigArg(args)

	if configPath == "" {
		fmt.Fprintln(os.Stderr, "fakecore: -c is required")
		os.Exit(2)
	}

	port, listenHost, err := inboundFromConfig(configPath)
	if err != nil {
		// Config validation failure: real cores exit non-zero here and
		// so does the stand-in (adapters test this path).
		fmt.Fprintf(os.Stderr, "fakecore: %v\n", err)
		os.Exit(1)
	}

	if !runMode {
		// "check": validation only, no listener.
		fmt.Printf("fakecore: configuration %s is valid\n", filepath.Base(configPath))
		return
	}

	if os.Getenv("FAKECORE_FAIL_FAST") == "1" {
		fmt.Fprintln(os.Stderr, "fakecore: injected startup failure")
		os.Exit(3)
	}

	if os.Getenv("FAKECORE_HANG") == "1" {
		// Never becomes ready: for startup-timeout paths.
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
		<-signals
		return
	}

	// Parallel test binaries share the ephemeral port range: a
	// reserved port can be stolen between close and bind. Retry the
	// bind briefly so lifecycle tests stay deterministic under
	// package-parallel execution.
	listener, err := listenWithRetry(listenHost, port, 2*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakecore: listen failed: %v\n", err)
		os.Exit(4)
	}

	defer listener.Close()

	fmt.Printf("fakecore %s started\n", version())

	if os.Getenv("FAKECORE_CRASH_AFTER_START") == "1" {
		go func() {
			time.Sleep(300 * time.Millisecond)
			fmt.Fprintln(os.Stderr, "fakecore: injected crash")
			os.Exit(5)
		}()
	}

	go func() {
		relayTarget := os.Getenv("FAKECORE_SOCKS_RELAY")

		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			if relayTarget == "" {
				_ = conn.Close()

				continue
			}

			go serveSocks5(conn, relayTarget)
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	<-signals

	fmt.Println("fakecore stopped")
}

// isVersionArg reports whether the argument is a version probe.
func isVersionArg(arg string) bool {
	return arg == "version" || arg == "--version" || arg == "-version"
}

// invocationName is the binary name it was invoked as, so a single
// fakecore binary staged as v2ray/xray/sing-box reports a matching
// identity.
func invocationName() string {
	base := filepath.Base(os.Args[0])

	return strings.TrimSuffix(base, ".exe")
}

// findConfigArg locates the -c/--config argument in a command line
// that may also carry a subcommand ("run -c file", "-c file",
// "run --config file").
func findConfigArg(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c", "--config", "-config":
			if i+1 < len(args) {
				return args[i+1]
			}

		default:
			if strings.HasPrefix(args[i], "-c=") {
				return args[i][3:]
			}

			if strings.HasPrefix(args[i], "--config=") {
				return args[i][len("--config="):]
			}
		}
	}

	return ""
}

// listenWithRetry binds an address, retrying while the port is
// briefly held by a concurrently terminating process.
func listenWithRetry(host string, port int, wait time.Duration) (net.Listener, error) {
	deadline := time.Now().Add(wait)

	for {
		listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err == nil {
			return listener, nil
		}

		if time.Now().After(deadline) {
			return nil, err
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// inboundFromConfig extracts the listen host and port from a
// generated runtime configuration. Supports both dialects:
//
//	V2Ray/Xray V4: {"inbounds":[{"listen":"127.0.0.1","port":10808}]}
//	sing-box:      {"inbounds":[{"listen":"127.0.0.1","listen_port":20808}]}
func inboundFromConfig(path string) (int, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, "", fmt.Errorf("read config %s: %w", path, err)
	}

	var doc struct {
		Inbounds []map[string]any `json:"inbounds"`
	}

	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, "", fmt.Errorf("parse config %s: %w", path, err)
	}

	if len(doc.Inbounds) == 0 {
		return 0, "", fmt.Errorf("config %s has no inbounds", path)
	}

	inbound := doc.Inbounds[0]

	host := "127.0.0.1"

	if value, ok := inbound["listen"].(string); ok && value != "" {
		host = value
	}

	port := 0

	// Real cores (Xray V4) accept the inbound port both as a JSON
	// number and as a string ("1080"); the fake core mirrors that so
	// minimal validation documents in either spelling parse.
	if value, ok := inbound["port"].(float64); ok {
		port = int(value)
	}

	if value, ok := inbound["port"].(string); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			port = parsed
		}
	}

	if value, ok := inbound["listen_port"].(float64); ok {
		port = int(value)
	}

	if port <= 0 || port > 65535 {
		return 0, "", fmt.Errorf("config %s has no valid inbound port", path)
	}

	return port, host, nil
}

func version() string {
	if v := os.Getenv("FAKECORE_VERSION"); v != "" {
		return v
	}

	return "v0-test"
}

// serveSocks5 implements just enough of RFC 1928 (no-auth greeting +
// CONNECT) for engine/socks5.Dialer to establish a tunnel, then
// relays bytes to the configured upstream. It exists so the
// connection-engine verification and racing tests can exercise the
// real tunnel-verification path against the deterministic fake core.
func serveSocks5(conn net.Conn, upstream string) {
	defer conn.Close()

	greet := make([]byte, 3)
	if _, err := io.ReadFull(conn, greet); err != nil {
		return
	}

	if greet[0] != 0x05 {
		return
	}

	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}

	if head[1] != 0x01 {
		_, _ = conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

		return
	}

	// Consume the request's DST.ADDR/DST.PORT fields; the relay
	// always dials its configured upstream regardless of the
	// requested destination (all the verification tests need).
	if _, err := readSocksAddr(conn, head[3]); err != nil {
		_, _ = conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

		return
	}

	up, err := net.Dial("tcp", upstream)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

		return
	}

	defer up.Close()

	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	relay(conn, up)
}

// readSocksAddr consumes the DST.ADDR/DST.PORT fields of a SOCKS5
// request (the relay ignores them and always dials its configured
// upstream, which is all the verification tests need).
func readSocksAddr(conn net.Conn, atyp byte) (string, error) {
	switch atyp {
	case 0x01:
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}

		port := make([]byte, 2)
		if _, err := io.ReadFull(conn, port); err != nil {
			return "", err
		}

		return net.JoinHostPort(net.IP(b).String(), strconv.Itoa(int(port[0])<<8|int(port[1]))), nil
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return "", err
		}

		host := make([]byte, l[0])
		if _, err := io.ReadFull(conn, host); err != nil {
			return "", err
		}

		port := make([]byte, 2)
		if _, err := io.ReadFull(conn, port); err != nil {
			return "", err
		}

		return string(host) + ":" + strconv.Itoa(int(port[0])<<8|int(port[1])), nil
	case 0x04:
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}

		port := make([]byte, 2)
		if _, err := io.ReadFull(conn, port); err != nil {
			return "", err
		}

		return net.JoinHostPort(net.IP(b).String(), strconv.Itoa(int(port[0])<<8|int(port[1]))), nil
	default:
		return "", fmt.Errorf("unsupported atyp %d", atyp)
	}
}

// relay copies bytes between the two connections until either side
// closes.
func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)

	go func() {
		_, _ = io.Copy(a, b)
		done <- struct{}{}
	}()

	go func() {
		_, _ = io.Copy(b, a)
		done <- struct{}{}
	}()

	<-done
}
