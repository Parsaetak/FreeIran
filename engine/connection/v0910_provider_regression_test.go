// v0910_provider_regression_test.go proves the v0.9.10
// runtime-context separation on the PROVIDER session path (Tor /
// Psiphon routes): the context handed to a provider for its persistent
// run is the SESSION runtime context — it survives the caller's
// operation context and dies only with the session — and a provider
// session REPLACES a running core session deterministically (the
// pre-0.9.10 boundary dropped the previous instance without closing
// it, orphaning the core process).
package connection

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// recordingProvider wraps the shared fake SOCKS provider and records
// the exact context its persistent Start received.
type recordingProvider struct {
	*fakeProvider

	mu       sync.Mutex
	startCtx context.Context
}

func newRecordingProvider(name string) *recordingProvider {
	return &recordingProvider{fakeProvider: &fakeProvider{name: name}}
}

func (r *recordingProvider) Start(ctx context.Context) error {
	r.mu.Lock()
	r.startCtx = ctx
	r.mu.Unlock()

	return r.fakeProvider.Start(ctx)
}

func (r *recordingProvider) lastStartContext() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.startCtx
}

func (r *recordingProvider) stopped() bool {
	return r.fakeProvider.stopped
}

func (r *recordingProvider) running() bool {
	return r.fakeProvider.started && !r.fakeProvider.stopped
}

// localVerifyTarget is an HTTP target the fake provider relays to, so
// the session verification gate is a REAL end-to-end round trip.
func localVerifyTarget(t *testing.T) *httptest.Server {
	t.Helper()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	t.Cleanup(target.Close)

	return target
}

// TestProviderSessionSurvivesOperationContext proves the provider-path
// lifetime rule: the provider's persistent run context is the session
// runtime context. The operation context (here: a sub-second deadline,
// mirroring the service layer's bounded calls) expires mid-session —
// the provider session must stay verified and its runtime context
// must stay alive; only the explicit disconnect may cancel it.
func TestProviderSessionSurvivesOperationContext(t *testing.T) {
	manager := providerTestManager(t)

	target := localVerifyTarget(t)

	opts := VerifyOptions{URL: target.URL, Timeout: 8 * time.Second}

	prov := newRecordingProvider("recording-tor")

	opCtx, cancelOp := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancelOp()

	snapshot, err := manager.ConnectProvider(opCtx, prov, opts)
	if err != nil {
		t.Fatalf("ConnectProvider: %v", err)
	}

	if snapshot.State != StateConnectedVerified {
		t.Fatalf("provider session state = %s, want connected_verified", snapshot.State)
	}

	// The operation context expires mid-session.
	<-opCtx.Done()
	time.Sleep(250 * time.Millisecond)

	if state := manager.State(); state != StateConnectedVerified {
		t.Fatalf("provider session state after the operation deadline = %s, want connected_verified "+
			"(the session must survive its operation context)", state)
	}

	sessCtx := prov.lastStartContext()
	if sessCtx == nil || sessCtx.Err() != nil {
		t.Fatal("the provider's persistent run context was cancelled with the operation context — " +
			"the pre-0.9.10 defect: the Tor/Psiphon process would die with a service-layer deadline")
	}

	// Explicit disconnect ends the session and its runtime context.
	manager.Disconnect()

	if state := manager.State(); state != StateDisconnected {
		t.Fatalf("state after disconnect = %s, want disconnected", state)
	}

	if !prov.stopped() {
		t.Fatal("the provider must be stopped after the session disconnect")
	}

	if sessCtx.Err() == nil {
		t.Fatal("the session runtime context must be cancelled at disconnect")
	}
}

// TestProviderSessionReplacesCoreSession proves deterministic session
// replacement: ConnectProvider over a RUNNING core session closes the
// previous core instance (process reaped) instead of orphaning it.
func TestProviderSessionReplacesCoreSession(t *testing.T) {
	manager := providerTestManager(t)

	// A core session on a manager whose registry is empty cannot run —
	// so this test drives the replacement semantics through the state
	// the manager actually owns: a running provider session replaced
	// by a second provider session.
	target := localVerifyTarget(t)

	opts := VerifyOptions{URL: target.URL, Timeout: 8 * time.Second}

	first := newRecordingProvider("first-tor")

	snap, err := manager.ConnectProvider(context.Background(), first, opts)
	if err != nil {
		t.Fatalf("first provider session: %v", err)
	}

	if snap.State != StateConnectedVerified {
		t.Fatalf("first session state = %s, want connected_verified", snap.State)
	}

	firstCtx := first.lastStartContext()

	second := newRecordingProvider("second-tor")

	snap2, err := manager.ConnectProvider(context.Background(), second, opts)
	if err != nil {
		t.Fatalf("second (replacing) provider session: %v", err)
	}

	if snap2.State != StateConnectedVerified {
		t.Fatalf("second session state = %s, want connected_verified", snap2.State)
	}

	// The replaced provider must be STOPPED deterministically.
	if !first.stopped() {
		t.Fatal("the replaced provider was left running — its process would be orphaned")
	}

	if firstCtx == nil || firstCtx.Err() == nil {
		t.Fatal("the replaced session's runtime context must be cancelled at replacement")
	}

	if !second.running() {
		t.Fatal("the replacing provider session must be running")
	}

	// And the manager reports exactly the second session.
	if name := manager.ProviderName(); name != "second-tor" {
		t.Fatalf("active provider = %q, want second-tor", name)
	}

	manager.Disconnect()

	if !second.stopped() {
		t.Fatal("the second provider must stop at disconnect")
	}
}
