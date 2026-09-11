// Command fakecore is a test helper that emulates a protocol-core
// binary for lifecycle tests: it accepts the standard `-c <file>`
// argument, extracts the local inbound port from the generated
// configuration (V2Ray/Xray V4 format or sing-box format), opens a
// TCP listener on that port, prints a "started" line, and exits
// cleanly on termination.
//
// Reading the port from the generated document — exactly like real
// cores do — makes the helper verify that adapters actually emit the
// inbound they claim to emit.
//
// Failure injection through environment variables:
//
//	FAKECORE_FAIL_FAST=1       exit(3) before listening
//	FAKECORE_CRASH_AFTER_START=1  exit(5) 300ms after startup
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	configPath := findConfigArg(os.Args[1:])

	if configPath == "" {
		fmt.Fprintln(os.Stderr, "fakecore: -c is required")
		os.Exit(2)
	}

	port, listenHost, err := inboundFromConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakecore: %v\n", err)
		os.Exit(1)
	}

	if os.Getenv("FAKECORE_FAIL_FAST") == "1" {
		fmt.Fprintln(os.Stderr, "fakecore: injected startup failure")
		os.Exit(3)
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
