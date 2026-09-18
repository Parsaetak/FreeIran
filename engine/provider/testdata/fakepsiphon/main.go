// fakepsiphon is a deterministic Psiphon console-client stand-in for
// provider tests. It parses -config <json>, opens the configured
// local SOCKS and HTTP proxy ports, emits tunnel-up style output and
// relays traffic — proving the engine's negotiation observation,
// proxy readiness, health and HTTP-through-Psiphon paths with no
// live Psiphon server dependency.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"
)

func main() {
	configPath := ""

	for i, arg := range os.Args {
		if arg == "-config" && i+1 < len(os.Args) {
			configPath = os.Args[i+1]
		}

		if arg == "-version" {
			fmt.Println("psiphon-tunnel-core 2.0.31")
			fmt.Println("Psiphon Inc.")

			return
		}
	}

	if configPath == "" {
		fmt.Fprintln(os.Stderr, "fakepsiphon: no -config")
		os.Exit(1)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakepsiphon: read config: %v\n", err)
		os.Exit(1)
	}

	var cfg struct {
		LocalSocksProxyPort int `json:"LocalSocksProxyPort"`
		LocalHttpProxyPort  int `json:"LocalHttpProxyPort"`
	}

	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "fakepsiphon: parse config: %v\n", err)
		os.Exit(1)
	}

	if cfg.LocalSocksProxyPort == 0 || cfg.LocalHttpProxyPort == 0 {
		fmt.Fprintln(os.Stderr, "fakepsiphon: missing proxy ports")
		os.Exit(1)
	}

	socks, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.LocalSocksProxyPort)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakepsiphon: socks listen: %v\n", err)
		os.Exit(1)
	}

	defer socks.Close()

	httpListener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.LocalHttpProxyPort)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakepsiphon: http listen: %v\n", err)
		os.Exit(1)
	}

	defer httpListener.Close()

	// Tunnel-core style output: connecting → tunnels up.
	go func() {
		time.Sleep(60 * time.Millisecond)
		fmt.Println(`notice: establishing tunnels`)
		time.Sleep(80 * time.Millisecond)
		fmt.Println(`notice: tunnels: 1`)
		fmt.Println(`notice: client regional 127.0.0.1`)
	}()

	go servePlain(httpListener)

	for {
		conn, err := socks.Accept()
		if err != nil {
			return
		}

		go serveSOCKS(conn)
	}
}

// servePlain accepts and closes (HTTP port accept probe).
func servePlain(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}

		go func(c net.Conn) {
			defer c.Close()

			_ = c.SetDeadline(time.Now().Add(10 * time.Second))

			buf := make([]byte, 4096)

			_, _ = c.Read(buf)

			_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
		}(conn)
	}
}

// serveSOCKS implements the no-auth CONNECT subset (same protocol
// shape as the faketor stand-in).
func serveSOCKS(conn net.Conn) {
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	reader := bufio.NewReader(conn)

	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return
	}

	if header[0] != 5 {
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

	var host string

	switch req[3] {
	case 0x01:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(reader, addr); err != nil {
			return
		}

		host = net.IP(addr).String()
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(reader, l); err != nil {
			return
		}

		name := make([]byte, int(l[0]))
		if _, err := io.ReadFull(reader, name); err != nil {
			return
		}

		host = string(name)
	default:
		return
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(reader, portBuf); err != nil {
		return
	}

	target := net.JoinHostPort(host, strconv.Itoa(int(portBuf[0])<<8|int(portBuf[1])))

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
