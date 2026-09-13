//go:build !linux

package mempressure

// rssBytes returns the process RSS in bytes. On non-Linux platforms
// this is a best-effort 0 (the heap fraction from runtime.MemStats is
// still tracked and is the primary pressure signal).
func rssBytes() uint64 {
	return 0
}
