package coremgr

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/Parsaetak/FreeIran/internal/httpx"
)

// ProductionHTTPClient is the shared production HTTP engine for the
// core manager: control-plane release queries and data-plane archive
// downloads both route through the ONE process-wide httpx.Client
// (single policy, single connection pool) shared with the application
// updater and the configuration-source fetcher.
//
// Tests inject their own httpx.Interface fake; there is no second
// production client anywhere in the package.
func ProductionHTTPClient() *httpx.Client {
	return httpx.Default()
}

// jsonMarshalIndent is a small wrapper kept here so the manager file
// does not need to import encoding/json directly (keeps the install
// surface obvious).
func jsonMarshalIndent(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// json.Encoder appends a trailing newline; trim it so the
	// persisted file matches json.MarshalIndent's output exactly.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// jsonUnmarshal wraps json.Unmarshal.
func jsonUnmarshal(raw []byte, v interface{}) error {
	return json.Unmarshal(raw, v)
}

// withTimeout is the bounded per-operation context helper used by
// release resolution (the data plane deliberately has NO total
// timeout — httpx enforces stall detection instead).
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = 30 * time.Second
	}

	return context.WithTimeout(ctx, d)
}
