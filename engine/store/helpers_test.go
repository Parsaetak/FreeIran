package store

import (
	"encoding/json"
	"hash/crc32"
)

// jsonMarshal is a small indirection so tests can build legacy files
// without importing encoding/json everywhere.
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// crc32Table returns the IEEE table used by journal CRC computation.
func crc32Table() *crc32.Table {
	return crc32.MakeTable(crc32.IEEE)
}
