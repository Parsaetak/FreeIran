//go:build linux

package mempressure

import (
	"os"
	"strconv"
	"strings"
)

// rssBytes returns the process RSS in bytes on Linux by reading
// /proc/self/statm (field 2, resident pages × page size).
func rssBytes() uint64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}
