package system

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// WriteFileAtomic writes data to a temp file in the destination
// directory, fsyncs, then renames over the destination. Readers never
// observe a partially written file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	temp, err := os.CreateTemp(dir, ".fir-sys-*")
	if err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "write", "create temp file")
	}

	tempPath := temp.Name()

	defer func() {
		_ = os.Remove(tempPath)
	}()

	if err := temp.Chmod(perm); err != nil {
		_ = temp.Close()

		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "write", "chmod")
	}

	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()

		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "write", "write")
	}

	if err := temp.Sync(); err != nil {
		_ = temp.Close()

		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "write", "sync")
	}

	if err := temp.Close(); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "write", "close")
	}

	if err := os.Rename(tempPath, path); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "write", "commit")
	}

	return nil
}

// NetworkDialer probes endpoint reachability with bounded time.
type NetworkDialer struct {
	// Timeout bounds one dial attempt.
	Timeout time.Duration
}

// DefaultDialTimeout is the reachability probe default.
const DefaultDialTimeout = 5 * time.Second

// Reachable reports whether a TCP endpoint accepts connections. It is
// a reachability check, not a protocol handshake.
func (d NetworkDialer) Reachable(ctx context.Context, host string, port int) (bool, time.Duration, error) {
	timeout := d.Timeout

	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()

	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "tcp",
		net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false, 0, nil // unreachable is a result, not a panic path
	}

	defer conn.Close()

	return true, time.Since(started), nil
}

// InterfaceSummary describes one local network interface.
type InterfaceSummary struct {
	Name     string   `json:"name"`
	Up       bool     `json:"up"`
	Loopback bool     `json:"loopback"`
	Addrs    []string `json:"addrs,omitempty"`
}

// Interfaces enumerates local network interfaces.
func Interfaces() []InterfaceSummary {
	listing, err := net.Interfaces()
	if err != nil {
		return nil
	}

	out := make([]InterfaceSummary, 0, len(listing))

	for _, iface := range listing {
		summary := InterfaceSummary{
			Name:     iface.Name,
			Up:       iface.Flags&net.FlagUp != 0,
			Loopback: iface.Flags&net.FlagLoopback != 0,
		}

		if addrs, err := iface.Addrs(); err == nil {
			summary.Addrs = make([]string, 0, len(addrs))

			for _, addr := range addrs {
				summary.Addrs = append(summary.Addrs, addr.String())
			}
		}

		out = append(out, summary)
	}

	return out
}
