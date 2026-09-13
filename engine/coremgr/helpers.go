package coremgr

import (
	"net"
	"time"
)

// reserveLocalPort allocates an ephemeral free port for the smoke
// test. The listen-close-reuse pattern has an inherent tiny race
// (another process could bind between close and the core's bind) but
// the smoke test tolerates that with a retry.
func reserveLocalPort(host string) (int, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

// dialTCP attempts a single TCP dial with the given timeout.
func dialTCP(endpoint string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	return d.Dial("tcp", endpoint)
}
