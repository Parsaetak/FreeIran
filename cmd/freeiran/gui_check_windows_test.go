//go:build windows

package main

import (
	"debug/pe"
	"os"
	"testing"
)

// TestWindowsGUISubsystem is the automated regression check for the
// "double-click FreeIran.exe → GUI opens → NO CMD WINDOW" requirement
// (§v0.9.2 console audit).
//
// A console-subsystem executable makes Windows allocate a console at
// startup — exactly the black CMD flash this project forbids. The
// release pipeline builds with `-H=windowsgui`; this test verifies the
// RESULTING binary (not the flag) by reading the PE header's
// subsystem field.
//
// The check needs the path of a BUILT application binary (a `go test`
// binary is console-subsystem by design): set FREEIRAN_GUI_EXE to the
// executable to verify. Without the variable the test skips, so
// regular `go test ./...` runs are unaffected. The release workflow
// sets it after linking and fails the release on mismatch.
func TestWindowsGUISubsystem(t *testing.T) {
	target := os.Getenv("FREEIRAN_GUI_EXE")
	if target == "" {
		t.Skip("FREEIRAN_GUI_EXE not set; skipping PE subsystem check " +
			"(the release workflow sets it to the built FreeIran.exe)")
	}

	file, err := pe.Open(target)
	if err != nil {
		t.Fatalf("open %s: %v", target, err)
	}

	defer file.Close()

	// 2 = IMAGE_SUBSYSTEM_WINDOWS_GUI, 3 = IMAGE_SUBSYSTEM_WINDOWS_CUI.
	const (
		subsystemWindowsGUI     = 2
		subsystemWindowsConsole = 3
	)

	var subsystem uint16

	switch header := file.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		subsystem = header.Subsystem

	case *pe.OptionalHeader32:
		subsystem = header.Subsystem

	default:
		t.Fatalf("%s: unsupported optional header %T", target, file.OptionalHeader)
	}

	switch subsystem {
	case subsystemWindowsGUI:
		// correct: no console window can appear

	case subsystemWindowsConsole:
		t.Fatalf("%s is a CONSOLE-subsystem executable: Windows will open a "+
			"CMD window on startup. Build with -ldflags \"-H=windowsgui\" "+
			"(see .github/workflows/release.yml)", target)

	default:
		t.Fatalf("%s has unexpected PE subsystem %d", target, subsystem)
	}
}
