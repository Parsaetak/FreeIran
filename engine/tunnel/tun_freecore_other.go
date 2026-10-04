//go:build !windows

package tunnel

// newFreecoreTUNBackend returns nil on platforms without the
// first-party TUN dataplane: the selector falls back to the managed
// sing-box dataplane (or the honest unavailable backend). The
// first-party TUN path is Windows-only in this release — the honest
// refusal lives here, not in a fake adapter.
func newFreecoreTUNBackend() TUNBackend {
	return nil
}
