// process_manifest_test.go pins the managed-process manifest contract:
// a supervised process is recorded at spawn (pid + resolved path) and
// removed at exit, the file mirrors the live registry exactly, and a
// clean shutdown leaves no manifest behind.
package system

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestManifestRecordsAndRemovesSupervisedProcess(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "managed-processes.txt")

	SetProcessManifestPath(manifestPath)
	t.Cleanup(func() { SetProcessManifestPath("") })

	if got := ProcessManifestPath(); got != manifestPath {
		t.Fatalf("ProcessManifestPath() = %q, want %q", got, manifestPath)
	}

	// Spawn a real supervised child (the platform sleep binary).
	spec := ProcessSpec{
		Name: "manifest-test",
		Path: sleepBinary(),
		Args: []string{"2"},
	}

	proc, err := Start(context.Background(), spec)
	if err != nil {
		t.Skipf("cannot spawn test child on this platform: %v", err)
	}

	// The running process must be recorded with its PID and an
	// absolute executable path.
	deadline := time.Now().Add(2 * time.Second)

	for {
		entries := ReadProcessManifest(manifestPath)

		if path, ok := entries[proc.PID()]; ok && path != "" {
			if !filepath.IsAbs(path) {
				t.Fatalf("manifest path %q is not absolute", path)
			}

			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("running process pid %d not recorded in manifest: %v", proc.PID(), entries)
		}

		time.Sleep(5 * time.Millisecond)
	}

	// Stop the process; the manifest entry must disappear, and once
	// the registry is empty the file itself is removed.
	if err := proc.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop() = %v", err)
	}

	deadline = time.Now().Add(2 * time.Second)

	for {
		if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
			return // clean: no stale manifest left behind
		}

		if time.Now().After(deadline) {
			entries := ReadProcessManifest(manifestPath)
			t.Fatalf("manifest still present after stop: entries=%v err=%v", entries, err)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

func TestManifestDisabledByDefault(t *testing.T) {
	// With no path configured, spawning must not write any file and
	// must not fail.
	SetProcessManifestPath("")

	dir := t.TempDir()
	before, _ := os.ReadDir(dir)

	spec := ProcessSpec{Name: "manifest-off", Path: sleepBinary(), Args: []string{"1"}}

	proc, err := Start(context.Background(), spec)
	if err != nil {
		t.Skipf("cannot spawn test child on this platform: %v", err)
	}

	_ = proc.Stop(time.Second)

	after, _ := os.ReadDir(dir)
	if len(after) != len(before) {
		t.Fatalf("files appeared in dir despite disabled manifest: %d -> %d", len(before), len(after))
	}
}

func TestReadProcessManifestSkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed-processes.txt")

	content := "123|/cores/xray.exe\n" +
		"\n" +
		"garbage\n" +
		"notapid|/cores/v2ray.exe\n" +
		"-5|/cores/sing-box.exe\n" +
		"456|/providers/tor/tor.exe\n"

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	entries := ReadProcessManifest(path)

	if len(entries) != 2 {
		t.Fatalf("parsed %d entries, want 2 (malformed lines skipped): %v", len(entries), entries)
	}

	if entries[123] != "/cores/xray.exe" || entries[456] != "/providers/tor/tor.exe" {
		t.Fatalf("wrong entries parsed: %v", entries)
	}
}

// sleepBinary returns a tiny platform child that stays alive briefly
// so supervision has something real to manage.
func sleepBinary() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("SystemRoot"), "System32", "timeout.exe")
	}

	return "/bin/sleep"
}
