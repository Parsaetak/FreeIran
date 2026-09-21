// faketorstall is a deterministic Tor stand-in that reproduces the
// v0.9.11 readiness-contract violation: it opens the local SOCKS
// listener IMMEDIATELY (like real Tor does) but NEVER emits
// "Bootstrapped 100%" — it prints one 50% notice and then stays
// alive. The engine's readiness supervisor must therefore refuse to
// publish READY (the endpoint accepting is evidence, not the
// verdict) and fail at the bootstrap deadline with honest evidence.
package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

func main() {
	torrc := ""

	for i, arg := range os.Args {
		if arg == "-f" && i+1 < len(os.Args) {
			torrc = os.Args[i+1]
		}

		if arg == "--version" {
			fmt.Println("Tor version 0.4.8.16 (git-abcdef1234567890123456789012345678901234).")

			return
		}
	}

	port := 0

	if data, err := os.ReadFile(torrc); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "SocksPort" {
				addr, err := net.ResolveTCPAddr("tcp", fields[1])
				if err == nil {
					port = addr.Port
				}
			}
		}
	}

	if port == 0 {
		fmt.Fprintln(os.Stderr, "faketorstall: no SocksPort in torrc")
		os.Exit(1)
	}

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "faketorstall: listen: %v\n", err)
		os.Exit(1)
	}

	defer listener.Close()

	// Half-way bootstrap notice, then silence forever: the endpoint
	// is accepting but bootstrap never completes.
	fmt.Println("Jan 01 00:00:00.000 [notice] Bootstrapped 50% (Connecting): Half way there")

	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}

		// Hold the connection: a TCP accept is the probe's evidence;
		// no SOCKS handshake is required by the engine.
		go func() {
			buf := make([]byte, 64)

			for {
				if _, err := conn.Read(buf); err != nil {
					break
				}
			}

			_ = conn.Close()
		}()
	}
}
