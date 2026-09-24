package app

// v0915_test.go — regression tests for the v0.9.15 deep fixes.
//
// 1. ONE authoritative testing path: DataService.TestConfig must
//    enqueue into the app's test queue (the SAME engine bulk testing
//    uses) and return promptly — never run a synchronous network test
//    on the caller's context. Repeated clicks collapse into the queued
//    task (no duplicate work).
//
// 2. The honest "timed_out" bulk-test scope: retry-timed-out filters
//    on the classified failure reason instead of silently reusing the
//    "failed" scope.
//
// The tester is replaced by a deterministic stub probe so the tests
// prove the ROUTING (queue convergence, dedup, scope filtering)
// without network access or core processes.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/tester"
)

// recordingProbe is a deterministic probe: every test passes, and the
// tested fingerprint is recorded for assertions.
type recordingProbe struct {
	tested chan<- string
}

func (p recordingProbe) Supports(config.Type) bool { return true }

func (p recordingProbe) Test(_ context.Context, cfg config.Config) (tester.Result, error) {
	select {
	case p.tested <- cfg.ID:
	case <-time.After(5 * time.Second):
	}

	return tester.Result{
		Working:  true,
		Measured: true,
		TestedAt: time.Now().UTC(),
	}, nil
}

// TestTestConfigEnqueuesIntoTheQueue proves the convergence: the
// single-test entry point shares the queue execution path (the queue's
// testerAdapter is the executor), returns promptly, and repeated
// clicks collapse into the queued task.
func TestTestConfigEnqueuesIntoTheQueue(t *testing.T) {
	a := newTestApp(t)

	tested := make(chan string, 8)

	a.tester = tester.New(recordingProbe{tested: tested})

	cfg := config.Config{ID: "3269ad33c4063654fb3c8935610c5a3756ab833e283d09d63ab6993f5a1a31ef", Type: "vless", Source: "src"}
	cfg.Name = "queued through the queue"
	cfg.Address = "198.51.100.10"
	cfg.Port = 443
	cfg.UUID = "b831381d-6324-4d53-ad4f-8cda48b30811"

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := a.store.Upsert(cfg.ID, raw); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	data := NewDataService(a)

	done := make(chan struct{})
	var result *config.Config
	var serviceErr error

	go func() {
		defer close(done)

		result, serviceErr = data.TestConfig(cfg.ID)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("TestConfig blocked: single tests must enqueue and return promptly")
	}

	if serviceErr != nil {
		t.Fatalf("TestConfig: %v", serviceErr)
	}

	if result == nil || result.ID != cfg.ID {
		t.Fatalf("result = %+v", result)
	}

	q, err := a.ensureTestQueue()
	if err != nil {
		t.Fatalf("ensureTestQueue: %v", err)
	}

	snapshot := q.Snapshot(10)

	if len(snapshot) != 1 {
		t.Fatalf("queue snapshot = %d tasks, want exactly 1", len(snapshot))
	}

	if snapshot[0].Fingerprint != cfg.ID {
		t.Fatalf("queued fingerprint = %q, want %q", snapshot[0].Fingerprint, cfg.ID)
	}

	if snapshot[0].Priority != TestPrioritySingle {
		t.Fatalf("priority = %d, want %d (a user is waiting)", snapshot[0].Priority, TestPrioritySingle)
	}

	// A repeated click must NOT create a second task.
	if _, err := data.TestConfig(cfg.ID); err != nil {
		t.Fatalf("repeated TestConfig: %v", err)
	}

	if again := q.Snapshot(10); len(again) != 1 {
		t.Fatalf("repeated click created %d tasks, want 1 (duplicate suppression)", len(again))
	}

	// The queue worker (the ONE execution path) actually runs the test
	// through the app's tester and persists the outcome.
	select {
	case fp := <-tested:
		if fp != cfg.ID {
			t.Fatalf("executed fingerprint = %q, want %q", fp, cfg.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the queue never executed the enqueued single test")
	}

	waitFor(t, 5*time.Second, func() bool {
		stored, err := a.storeGetConfig(cfg.ID)
		return err == nil && stored.TestedAt > 0
	})
}

func waitFor(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(within)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(25 * time.Millisecond)
	}

	t.Fatal("condition not met in time")
}

// TestTestConfigUnknownConfigFailsFast keeps the old contract: an
// unknown id is an error, not a queued mystery.
func TestTestConfigUnknownConfigFailsFast(t *testing.T) {
	a := newTestApp(t)

	data := NewDataService(a)

	if _, err := data.TestConfig("does-not-exist"); err == nil {
		t.Fatal("TestConfig for an unknown config must fail")
	}
}

// TestEnqueueByFilterTimedOutScope pins the honest scope: only
// working=false configs whose classified failure reason is a timeout
// match "timed_out"; plain failures do not.
func TestEnqueueByFilterTimedOutScope(t *testing.T) {
	a := newTestApp(t)

	failed := config.Config{ID: "d44e45c9ad41f1c3b763c13ddd27ea4272d4982bebd78e95405adae8b7e14e56", Type: "vless"}
	failed.Working = false
	failed.TestedAt = time.Now().Unix()
	failed.LastFailureReason = "connection refused"

	timedOut := config.Config{ID: "9d85345e2e5ef0793a0a541418783f0662926d5207091434925d88a332727bd1", Type: "vless"}
	timedOut.Working = false
	timedOut.TestedAt = time.Now().Unix()
	timedOut.LastFailureReason = "timeout"

	for _, cfg := range []config.Config{failed, timedOut} {
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		if err := a.store.Upsert(cfg.ID, raw); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	q := &TestQueueService{app: a}

	result, err := q.EnqueueByFilter(TestFilter{Scope: "timed_out"})
	if err != nil {
		t.Fatalf("EnqueueByFilter: %v", err)
	}

	if result.Enqueued != 1 {
		t.Fatalf("enqueued = %d, want exactly the timed-out config", result.Enqueued)
	}

	queue, err := a.ensureTestQueue()
	if err != nil {
		t.Fatalf("ensureTestQueue: %v", err)
	}

	snapshot := queue.Snapshot(10)

	if len(snapshot) != 1 || snapshot[0].Fingerprint != timedOut.ID {
		t.Fatalf("queued tasks = %+v, want only %q", snapshot, timedOut.ID)
	}
}
