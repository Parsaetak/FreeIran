package coremgr

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/internal/version"
)

// defaultHTTPClient is a real *http.Client with safe timeouts. It
// implements HTTPDoer through a thin adapter (httpAdapter).
var defaultHTTPClient = &http.Client{
	Timeout: 60 * time.Second,
}

// httpAdapter wraps *http.Client so it satisfies HTTPDoer.
type httpAdapter struct{ client *http.Client }

// Do fetches the URL and returns the body + headers. Network errors
// are wrapped as KindNetwork errors so callers can classify them.
func (h *httpAdapter) Do(url string) (*HTTPResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindConfiguration,
			Subsystem, "http", "build request %s", url)
	}

	req.Header.Set("User-Agent", version.UserAgent())
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindRetryable,
			Subsystem, "http", "GET %s", url)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindRetryable,
			Subsystem, "http", "read body %s", url)
	}

	headers := make(map[string]string, len(resp.Header))
	for k, vs := range resp.Header {
		if len(vs) > 0 {
			headers[strings.ToLower(k)] = vs[0]
		}
	}

	return &HTTPResponse{
		StatusCode: resp.StatusCode,
		Body:       body,
		Headers:    headers,
	}, nil
}

// resolveHTTPClient returns the configured client, or a default
// adapter if none was set.
func (m *Manager) resolveHTTPClient() HTTPDoer {
	if m.httpClient != nil {
		return m.httpClient
	}
	return &httpAdapter{client: defaultHTTPClient}
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
