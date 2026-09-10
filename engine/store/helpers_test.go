package store

import "encoding/json"

// jsonMarshal is a small indirection so tests can build legacy files
// without importing encoding/json everywhere.
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
