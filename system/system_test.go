package system

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnsureLayout(t *testing.T) {
	base := filepath.Join(t.TempDir(), "freeiran")

	layout, err := EnsureLayout(base)
	if err != nil {
		t.Fatalf("ensure layout: %v", err)
	}

	if layout.Data != filepath.Join(base, "data") {
		t.Fatalf("data dir = %s", layout.Data)
	}

	for _, dir := range []string{
		layout.Root, layout.Data, layout.Cache, layout.Logs, layout.Cores,
	} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("directory missing: %s", dir)
		}
	}

	if _, err := EnsureLayout(""); err == nil {
		t.Fatal("empty base must be rejected")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := WriteFileAtomic(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"a":1}` {
		t.Fatalf("read back: %v %q", err, data)
	}

	// Overwrite works and leaves no temp files.
	if err := WriteFileAtomic(path, []byte(`{"a":2}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	entries, _ := os.ReadDir(dir)

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".fir-sys-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestRedact(t *testing.T) {
	log := "uuid=SECRET-123 password=hunter2 nothing"

	cleaned := Redact(log, "SECRET-123", "hunter2")

	if strings.Contains(cleaned, "SECRET-123") ||
		strings.Contains(cleaned, "hunter2") {
		t.Fatalf("secrets survived redaction: %s", cleaned)
	}

	// Empty secrets are ignored.
	if Redact(log, "") != log {
		t.Fatal("empty secret must not alter text")
	}
}

func TestReachableLocal(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener available")
	}

	defer listener.Close()

	go func() {
		conn, _ := listener.Accept()

		if conn != nil {
			_ = conn.Close()
		}
	}()

	port := listener.Addr().(*net.TCPAddr).Port

	d := NetworkDialer{Timeout: 2 * time.Second}

	ok, latency, err := d.Reachable(context.Background(), "127.0.0.1", port)
	if err != nil {
		t.Fatalf("reachable: %v", err)
	}

	if !ok {
		t.Fatal("local listener should be reachable")
	}

	if latency <= 0 {
		t.Fatal("latency should be measured")
	}

	// An unreachable endpoint returns ok=false without error.
	ok, _, err = d.Reachable(context.Background(), "127.0.0.1", 1)
	if err != nil {
		t.Fatalf("unreachable probe returned error: %v", err)
	}

	if ok {
		t.Fatal("port 1 should not be reachable")
	}
}

func TestCoreLocatorMissing(t *testing.T) {
	loc := NewCoreLocator(t.TempDir())

	_, err := loc.Discover(context.Background(), "definitely-not-a-core-xyz")
	if err == nil {
		t.Fatal("missing core should error")
	}
}

func TestCoreLocatorFindsFile(t *testing.T) {
	coreDir := t.TempDir()
	corePath := filepath.Join(coreDir, executableName("fakecore"))

	// Create a fake core executable that answers --version via a
	// shell script on unix; on windows this test is skipped.
	if goosCheck() == "windows" {
		t.Skip("version probe test is unix-specific")
	}

	script := "#!/bin/sh\necho fakecore 1.2.3\n"

	if err := os.WriteFile(corePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	loc := NewCoreLocator(coreDir)

	binary, err := loc.Discover(context.Background(), "fakecore")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	if binary.Path != corePath {
		t.Fatalf("path = %s", binary.Path)
	}

	if !strings.Contains(binary.Version, "fakecore") {
		t.Fatalf("version = %q", binary.Version)
	}
}

func TestInterfaces(t *testing.T) {
	list := Interfaces()

	if len(list) == 0 {
		t.Fatal("expected at least one interface")
	}
}

func TestGetInfo(t *testing.T) {
	info := GetInfo()

	if info.OS == "" || info.Arch == "" || info.NumCPU == 0 {
		t.Fatalf("incomplete info: %+v", info)
	}
}
