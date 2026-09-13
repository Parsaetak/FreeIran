// Package appicon embeds the FreeIran application icon so the
// runtime window icon never depends on external files at user
// machines (v0.9.1).
//
// The embedded PNG is generated from the canonical vector source
// (assets/freeiran-icon.svg) by scripts/genicon.py, which also writes
// assets/freeiran-icon.ico for the Windows executable resource.
// Regenerating the icon always refreshes both copies.
package appicon

import _ "embed"

// PNG is the 256x256 application icon. Wails v3 consumes it directly:
//   - Linux: application.LinuxWindow.Icon (GTK window icon);
//   - Windows: the titlebar/taskbar icon comes from the executable
//     resource instead (cmd/freeiran/*.syso, built from
//     assets/freeiran-icon.ico) — this PNG is not used there.
//
//go:embed freeiran-icon.png
var PNG []byte
