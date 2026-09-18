// faketor is a deterministic Tor stand-in for provider tests. It
// parses -f <torrc>, emits real "Bootstrapped" notice lines on
// stdout, and serves a minimal SOCKS5 endpoint on the configured
// SocksPort that actually relays CONNECT traffic — enough to prove
// the engine's bootstrap observation, health probing and end-to-end
// HTTP-through-Tor paths without any live Tor network dependency.
package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	torrc := ""

	for i, arg := range os.Args {
		if arg == "-f" && i+1 < len(os.Args) {
			torrc = os.Args[i+1]
		}

		if arg == "--version" {
			fmt.Println("Tor version 0.4.8.16 (git-abcdef1234567890123456789012345678901234).")
			fmt.Println("Libevent version 2.1.12-stable")

			return
		}

		if arg == "--verify-config" {
			fmt.Println("Jan 01 00:00:00.000 [notice] Read configuration file \"" + torrc + "\".")
			fmt.Println("Configuration was valid")

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
		fmt.Fprintln(os.Stderr, "faketor: no SocksPort in torrc")
		os.Exit(1)
	}

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "faketor: listen: %v\n", err)
		os.Exit(1)
	}

	defer listener.Close()

	// Real notice-style bootstrap lines, in Tor's own format.
	go func() {
		for _, step := range []struct {
			pct   int
			tag   string
			delay time.Duration
		}{
			{0, "Starting", 20 * time.Millisecond},
			{5, "Connecting to directory server", 30 * time.Millisecond},
			{45, "Connecting", 40 * time.Millisecond},
			{75, "Enabling", 40 * time.Millisecond},
			{90, "Establishing", 30 * time.Millisecond},
		} {
			time.Sleep(step.delay)
			fmt.Printf("Jan 01 00:00:00.000 [notice] Bootstrapped %d%% (%s): Doing an internet thing\n", step.pct, step.tag)
		}

		time.Sleep(50 * time.Millisecond)
		fmt.Println("Jan 01 00:00:00.000 [notice] Bootstrapped 100% (Done): Connected to the Tor network")
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}

		go serveSOCKS(conn)
	}
}

// serveSOCKS implements the no-auth CONNECT subset.
func serveSOCKS(conn net.Conn) {
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	reader := bufio.NewReader(conn)

	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return
	}

	if header[0] != 5 || header[1] == 0 {
		return
	}

	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return
	}

	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(reader, req); err != nil {
		return
	}

	if req[1] != 0x01 {
		return
	}

	var target string

	switch req[3] {
	case 0x01:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(reader, addr); err != nil {
			return
		}

		target = net.IP(addr).String()
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(reader, l); err != nil {
			return
		}

		name := make([]byte, int(l[0]))
		if _, err := io.ReadFull(reader, name); err != nil {
			return
		}

		target = string(name)
	case 0x04:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(reader, addr); err != nil {
			return
		}

		target = net.IP(addr).String()
	default:
		return
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(reader, portBuf); err != nil {
		return
	}

	target = net.JoinHostPort(target, strconv.Itoa(int(portBuf[0])<<8|int(portBuf[1])))

	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x04, 0x00, 0x01, 127, 0, 0, 1, 0, 0})

		return
	}

	defer upstream.Close()

	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0}); err != nil {
		return
	}

	_ = conn.SetDeadline(time.Time{})

	done := make(chan struct{}, 2)

	go func() { _, _ = io.Copy(upstream, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()

	<-done
}
