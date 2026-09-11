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
package main

import (
	"encoding/json"
	"fmt"
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
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			_ = conn.Close()
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

	if value, ok := inbound["port"].(float64); ok {
		port = int(value)
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
