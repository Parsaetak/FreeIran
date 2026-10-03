//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestWindowsGUIIcon is the automated regression check for the
// "titlebar / taskbar / Explorer show the canonical FreeIran icon"
// requirement (v0.13.0 icon root-cause fix).
//
// The executable icon comes from the linked Windows resource
// (cmd/freeiran/rsrc_windows_amd64.syso, compiled from
// build/winres.json → assets/freeiran-icon.ico). A regression in that
// chain — a stale .syso, a winres.json pointing at the bare 256px PNG,
// or a resource set with the icon entries dropped — cannot be seen by
// the subsystem check (TestWindowsGUISubsystem) and only shows up as a
// wrong/blurry icon in the Windows shell.
//
// This test verifies the RESULTING binary:
//
//  1. the PE carries a .rsrc resource table with exactly ONE
//     RT_GROUP_ICON and the RT_ICON entries behind it;
//  2. every RT_ICON payload byte-identically matches one image of the
//     CANONICAL icon family (assets/freeiran-icon.ico) — the one icon
//     asset authority — so the shell cannot be showing a foreign or
//     silently-resized single-image icon;
//  3. the single RT_GROUP_ICON is registered under NUMERIC resource
//     ID 3 (v0.13.1): the pinned Wails v3.0.0-beta.19 window path
//     assigns the window icon through LoadIconW(exe, MAKEINTRESOURCE(3))
//     - NewIconFromResource(GetModuleHandle(""), 3) in
//     webview_window_windows.go - and only falls back to
//     application.Options.Icon bytes when that lookup fails. The
//     v0.13.0 build shipped its only icon group under the resource
//     NAME "APP" (a go-winres string-name key), so the ID-3 lookup
//     failed on every launch, no WM_SETICON was ever sent and the
//     desktop showed the generic default icon while this test stayed
//     green - exactly the regression this third assertion pins.
//
// Like TestWindowsGUISubsystem it needs the path of a BUILT
// application binary via FREEIRAN_GUI_EXE and skips otherwise. The
// release workflow sets it after linking and fails the release on
// mismatch. No live-window claim is made here: this proves the
// resource is LINKED and discoverable through the exact lookup the
// pinned Wails performs, which is the input the Windows shell
// consumes for the titlebar/taskbar/executable icon; live shell
// rendering is not CI-executable and is not claimed.
func TestWindowsGUIIcon(t *testing.T) {
	target := os.Getenv("FREEIRAN_GUI_EXE")
	if target == "" {
		t.Skip("FREEIRAN_GUI_EXE not set; skipping PE icon check " +
			"(the release workflow sets it to the built FreeIran.exe)")
	}

	// Locate the canonical icon family through the repository root
	// (the test binary runs from cmd/freeiran; walk up exactly like
	// the version-consistency test does).
	root, ok := findRepoRootForIcon(t)
	if !ok {
		t.Skip("repository root not found; skipping PE icon check")
	}

	canonical, err := os.ReadFile(filepath.Join(root, "assets", "freeiran-icon.ico"))
	if err != nil {
		t.Fatalf("read canonical icon: %v", err)
	}

	want := canonicalICOImageBlobs(canonical)
	if len(want) < 5 {
		t.Fatalf("assets/freeiran-icon.ico carries %d images, want the multi-size family (>=5)", len(want))
	}

	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}

	groups, icons, err := peIconResources(raw)
	if err != nil {
		t.Fatalf("%s: inspect PE resources: %v", target, err)
	}

	if len(groups) != 1 {
		t.Fatalf("%s carries %d RT_GROUP_ICON resources, want exactly 1", target, len(groups))
	}

	// The group must be reachable through the numeric-ID lookup the
	// pinned Wails performs (LoadIconW(exe, MAKEINTRESOURCE(3))). A
	// NAMED group (any string name) is invisible to that lookup even
	// though Explorer still shows the file icon - the v0.13.0 defect.
	if g := groups[0]; g.named || g.id != 3 {
		t.Fatalf("%s registers its RT_GROUP_ICON as %s, want numeric ID 3 "+
			"(Wails beta.19 loads LoadIconW(exe, MAKEINTRESOURCE(3))); "+
			"regenerate the .syso from build/winres.json with the \"#3\" key",
			target, groupRefLabel(g))
	}

	if len(icons) < 5 {
		t.Fatalf("%s carries %d RT_ICON images, want the multi-size family (>=5); "+
			"the committed .syso is stale — regenerate it with go-winres (docs/development.md)",
			target, len(icons))
	}

	for _, blob := range icons {
		if !want[icoBlobKey(blob)] {
			t.Fatalf("%s carries an RT_ICON image that is NOT part of assets/freeiran-icon.ico "+
				"(len=%d): the executable ships a foreign icon", target, len(blob))
		}
	}
}

// icoBlobKey keys one icon image payload by length + leading bytes:
// exact-content identity without hashing machinery in a test.
func icoBlobKey(b []byte) string {
	head := b
	if len(head) > 16 {
		head = head[:16]
	}

	return string(rune(len(b))) + "|" + string(head)
}

// canonicalICOImageBlobs returns the image payload of every directory
// entry of an ICO file, keyed by icoBlobKey.
func canonicalICOImageBlobs(ico []byte) map[string]bool {
	if len(ico) < 6 {
		return nil
	}

	count := int(binary.LittleEndian.Uint16(ico[4:6]))
	out := make(map[string]bool, count)

	for i := range count {
		entry := 6 + i*16
		if entry+16 > len(ico) {
			return out
		}

		size := int(binary.LittleEndian.Uint32(ico[entry+8 : entry+12]))
		offset := int(binary.LittleEndian.Uint32(ico[entry+12 : entry+16]))

		if size <= 0 || offset < 0 || offset+size > len(ico) {
			continue
		}

		out[icoBlobKey(ico[offset:offset+size])] = true
	}

	return out
}

// resource-table constants (IMAGE_RESOURCE_DIRECTORY layout).
const (
	icoResourceTableRVAIndex = 2 // optional-header data directory slot
	icoRTGroupIcon           = 14
	icoRTIcon                = 3

	// icoResNameFlag marks a resource-directory Name field that holds
	// an offset to a Unicode string instead of a numeric ID.
	icoResNameFlag = 0x80000000
)

// icoGroupRef describes one RT_GROUP_ICON directory entry: whether it
// is registered by NAME (true) or by numeric ID, and the numeric ID
// when applicable.
type icoGroupRef struct {
	named bool
	id    uint32
}

// groupRefLabel renders a group reference for failure messages.
func groupRefLabel(g icoGroupRef) string {
	if g.named {
		return "a NAMED resource"
	}

	return fmt.Sprintf("numeric ID %d", g.id)
}

// peIconResources walks the PE resource table and returns
// (RT_GROUP_ICON references, RT_ICON image payloads).
func peIconResources(pe []byte) ([]icoGroupRef, [][]byte, error) {
	if len(pe) < 0x40 || binary.LittleEndian.Uint16(pe[:2]) != 0x5a4d {
		return nil, nil, os.ErrInvalid
	}

	peOff := int(binary.LittleEndian.Uint32(pe[0x3c:0x40]))
	if peOff <= 0 || peOff+24 > len(pe) || binary.LittleEndian.Uint32(pe[peOff:peOff+4]) != 0x00004550 {
		return nil, nil, os.ErrInvalid
	}

	nSections := int(binary.LittleEndian.Uint16(pe[peOff+6 : peOff+8]))
	optSize := int(binary.LittleEndian.Uint16(pe[peOff+20 : peOff+22]))
	opt := peOff + 24

	if optSize < 128 || opt+optSize+nSections*40 > len(pe) {
		return nil, nil, os.ErrInvalid
	}

	// The optional header magic selects the data-directory layout:
	// PE32 (0x10b) directories start at +96, PE32+ (0x20b) at +112.
	var dataDirOff int

	switch magic := binary.LittleEndian.Uint16(pe[opt : opt+2]); magic {
	case 0x10b:
		dataDirOff = 96
	case 0x20b:
		dataDirOff = 112
	default:
		return nil, nil, os.ErrInvalid
	}

	if optSize < dataDirOff+16*8 {
		return nil, nil, os.ErrInvalid
	}

	// Resource-table data directory (index 2): VirtualAddress + Size.
	resSlot := opt + dataDirOff + icoResourceTableRVAIndex*8
	resRVA := binary.LittleEndian.Uint32(pe[resSlot : resSlot+4])
	resSize := binary.LittleEndian.Uint32(pe[resSlot+4 : resSlot+8])

	if resRVA == 0 || resSize == 0 {
		return nil, nil, nil
	}

	for s := range nSections {
		sec := opt + optSize + s*40
		vaddr := binary.LittleEndian.Uint32(pe[sec+12 : sec+16])
		rawSize := binary.LittleEndian.Uint32(pe[sec+16 : sec+20])
		rawPtr := binary.LittleEndian.Uint32(pe[sec+20 : sec+24])

		if !(resRVA >= vaddr && resRVA+resSize <= vaddr+rawSize) || rawSize == 0 {
			continue
		}

		// A section matches only when its VIRTUAL address range covers
		// the table: the raw containment above is necessary because the
		// mapped table must be file-backed, but the virtual check is
		// what excludes earlier sections spanning the RVA by raw size
		// alone (e.g. .text).
		vsize := binary.LittleEndian.Uint32(pe[sec+8 : sec+12])
		virtualCover := vsize
		if rawSize < virtualCover {
			virtualCover = rawSize
		}

		if !(resRVA >= vaddr && resRVA+resSize <= vaddr+virtualCover) {
			continue
		}

		base := int(resRVA - vaddr)

		if int(rawPtr)+int(rawSize) > len(pe) {
			return nil, nil, os.ErrInvalid
		}

		section := pe[rawPtr : rawPtr+rawSize]

		if base+int(resSize) > len(section) {
			return nil, nil, os.ErrInvalid
		}

		rsrc := section[base : base+int(resSize)]

		groups := []icoGroupRef{}
		var icons [][]byte

		// Level 1: type entries.
		for _, ty := range icoDirEntries(rsrc, 0) {
			if ty.id != icoRTGroupIcon && ty.id != icoRTIcon {
				continue
			}

			// Level 2: name/ID entries → level 3: language leaves.
			for _, nm := range icoDirEntries(rsrc, ty.sub) {
				// The level-2 Name field selects HOW the resource is
				// registered: high bit set → an offset to a Unicode
				// NAME; clear → a numeric resource ID. LoadIconW can
				// only find numeric IDs.
				named := nm.id&icoResNameFlag != 0
				id := nm.id & 0x7fffffff

				for _, leaf := range icoDirEntries(rsrc, nm.sub) {
					if int(leaf.sub)+16 > len(rsrc) {
						continue
					}

					dataRVA := binary.LittleEndian.Uint32(rsrc[leaf.sub : leaf.sub+4])
					size := binary.LittleEndian.Uint32(rsrc[leaf.sub+4 : leaf.sub+8])

					// Leaf OffsetToData is a full RVA; the resource table
					// starts at resRVA, which rsrc[0] maps to.
					start := int(dataRVA - resRVA)

					if size == 0 || start < 0 || start+int(size) > len(rsrc) {
						continue
					}

					blob := make([]byte, size)
					copy(blob, rsrc[start:start+int(size)])

					if ty.id == icoRTGroupIcon {
						groups = append(groups, icoGroupRef{named: named, id: id})
					} else {
						icons = append(icons, blob)
					}
				}
			}
		}

		return groups, icons, nil
	}

	return nil, nil, nil
}

// icoResEntry is one IMAGE_RESOURCE_DIRECTORY_ENTRY.
type icoResEntry struct {
	id  uint32
	sub uint32 // subdirectory offset (already masked) or data-entry offset
}

// icoDirEntries parses the directory at off into its entries,
// resolving subdirectory offsets (high bit set) to section-relative
// offsets.
func icoDirEntries(rsrc []byte, off uint32) []icoResEntry {
	if int(off)+16 > len(rsrc) {
		return nil
	}

	nName := binary.LittleEndian.Uint16(rsrc[off+12 : off+14])
	nID := binary.LittleEndian.Uint16(rsrc[off+14 : off+16])

	out := make([]icoResEntry, 0, int(nName)+int(nID))

	for i := range int(nName) + int(nID) {
		entry := off + uint32(16+i*8)
		if int(entry)+8 > len(rsrc) {
			break
		}

		id := binary.LittleEndian.Uint32(rsrc[entry : entry+4])
		raw := binary.LittleEndian.Uint32(rsrc[entry+4 : entry+8])

		e := icoResEntry{id: id}

		if raw&0x80000000 != 0 {
			e.sub = raw & 0x7fffffff
		} else {
			e.sub = raw
		}

		out = append(out, e)
	}

	return out
}

// findRepoRootForIcon walks up from the working directory to the
// repository root (same markers the version-consistency test uses).
func findRepoRootForIcon(t *testing.T) (string, bool) {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}

	for range 8 {
		if fileExistsAtIcon(filepath.Join(dir, "go.mod")) &&
			fileExistsAtIcon(filepath.Join(dir, "build", "winres.json")) &&
			fileExistsAtIcon(filepath.Join(dir, "assets", "freeiran-icon.ico")) {
			return dir, true
		}

		parent := filepath.Dir(dir)

		if parent == dir {
			return "", false
		}

		dir = parent
	}

	return "", false
}

func fileExistsAtIcon(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir()
}
