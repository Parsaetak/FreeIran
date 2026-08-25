package pool

import (
	"errors"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
)

func testConfig(address string, port int) *config.Config {
	return &config.Config{
		Type:    config.TypeSOCKS,
		Address: address,
		Port:    port,
	}
}

func fingerprint(cfg *config.Config) string {
	cfg.Normalize()
	return cfg.Fingerprint()
}

func TestNew(t *testing.T) {
	p := New()

	if p == nil {
		t.Fatal("expected non-nil pool")
	}

	if p.Count() != 0 {
		t.Fatalf("expected empty pool, got %d entries", p.Count())
	}

	if p.WorkingCount() != 0 {
		t.Fatalf("expected zero working configurations, got %d", p.WorkingCount())
	}

	if p.TestedCount() != 0 {
		t.Fatalf("expected zero tested configurations, got %d", p.TestedCount())
	}
}

func TestAddAndGet(t *testing.T) {
	p := New()
	cfg := testConfig("example.com", 1080)

	if err := p.Add(cfg); err != nil {
		t.Fatalf("Add() failed: %v", err)
	}

	id := cfg.Fingerprint()

	got, ok := p.Get(id)
	if !ok {
		t.Fatal("expected configuration to exist")
	}

	if got == nil {
		t.Fatal("expected non-nil configuration")
	}

	if got.ID != id {
		t.Fatalf("expected ID %q, got %q", id, got.ID)
	}

	if got.Type != config.TypeSOCKS {
		t.Fatalf("expected SOCKS type, got %q", got.Type)
	}

	if got.Address != "example.com" {
		t.Fatalf("expected address example.com, got %q", got.Address)
	}

	if got.Port != 1080 {
		t.Fatalf("expected port 1080, got %d", got.Port)
	}
}

func TestAddRejectsDuplicate(t *testing.T) {
	p := New()

	cfg1 := testConfig("example.com", 1080)
	cfg2 := testConfig("example.com", 1080)

	if err := p.Add(cfg1); err != nil {
		t.Fatalf("first Add() failed: %v", err)
	}

	if err := p.Add(cfg2); err == nil {
		t.Fatal("expected duplicate configuration to be rejected")
	}

	if p.Count() != 1 {
		t.Fatalf("expected one entry, got %d", p.Count())
	}
}

func TestAddRejectsNilConfiguration(t *testing.T) {
	p := New()

	if err := p.Add(nil); err == nil {
		t.Fatal("expected nil configuration error")
	}

	if p.Count() != 0 {
		t.Fatalf("expected empty pool, got %d entries", p.Count())
	}
}

func TestUpsert(t *testing.T) {
	p := New()

	cfg := testConfig("example.com", 1080)

	if err := p.Upsert(cfg); err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}

	if p.Count() != 1 {
		t.Fatalf("expected one entry, got %d", p.Count())
	}

	got, ok := p.Get(cfg.Fingerprint())
	if !ok {
		t.Fatal("expected configuration after Upsert()")
	}

	if got == nil {
		t.Fatal("expected non-nil configuration")
	}
}

func TestUpsertReplacesExistingConfiguration(t *testing.T) {
	p := New()

	first := testConfig("example.com", 1080)

	if err := p.Upsert(first); err != nil {
		t.Fatalf("first Upsert() failed: %v", err)
	}

	// The second configuration has the same fingerprint but a different
	// runtime field. Upsert must replace the stored value rather than
	// creating another entry.
	second := testConfig("example.com", 1080)
	second.Working = true
	second.LatencyMS = 42
	second.TestedAt = 123456

	if err := p.Upsert(second); err != nil {
		t.Fatalf("second Upsert() failed: %v", err)
	}

	if p.Count() != 1 {
		t.Fatalf("expected one entry, got %d", p.Count())
	}

	id := second.Fingerprint()

	got, ok := p.Get(id)
	if !ok {
		t.Fatal("expected configuration after replacement")
	}

	if !got.Working {
		t.Fatal("expected replacement configuration to preserve Working")
	}

	if got.LatencyMS != 42 {
		t.Fatalf("expected latency 42, got %d", got.LatencyMS)
	}

	if got.TestedAt != 123456 {
		t.Fatalf("expected TestedAt 123456, got %d", got.TestedAt)
	}
}

func TestUpsertRejectsNilConfiguration(t *testing.T) {
	p := New()

	if err := p.Upsert(nil); err == nil {
		t.Fatal("expected nil configuration error")
	}
}

func TestRemove(t *testing.T) {
	p := New()
	cfg := testConfig("example.com", 1080)

	if err := p.Add(cfg); err != nil {
		t.Fatalf("Add() failed: %v", err)
	}

	id := cfg.Fingerprint()

	if !p.Remove(id) {
		t.Fatal("expected Remove() to return true")
	}

	if p.Has(id) {
		t.Fatal("configuration should no longer exist")
	}

	if p.Count() != 0 {
		t.Fatalf("expected empty pool, got %d entries", p.Count())
	}
}

func TestRemoveMissing(t *testing.T) {
	p := New()

	if p.Remove("missing") {
		t.Fatal("expected Remove() to return false for missing configuration")
	}
}

func TestHas(t *testing.T) {
	p := New()
	cfg := testConfig("example.com", 1080)
	id := fingerprint(cfg)

	if p.Has(id) {
		t.Fatal("configuration should not exist before Add()")
	}

	if err := p.Add(cfg); err != nil {
		t.Fatalf("Add() failed: %v", err)
	}

	if !p.Has(id) {
		t.Fatal("configuration should exist after Add()")
	}
}

func TestMarkTestedWorking(t *testing.T) {
	p := New()
	cfg := testConfig("example.com", 1080)

	if err := p.Add(cfg); err != nil {
		t.Fatalf("Add() failed: %v", err)
	}

	id := cfg.Fingerprint()

	if err := p.MarkTested(id, true, nil); err != nil {
		t.Fatalf("MarkTested() failed: %v", err)
	}

	if !p.IsTested(id) {
		t.Fatal("expected configuration to be marked tested")
	}

	if !p.IsWorking(id) {
		t.Fatal("expected configuration to be marked working")
	}

	if p.LastError(id) != "" {
		t.Fatalf("expected no last error, got %q", p.LastError(id))
	}

	if p.TestedCount() != 1 {
		t.Fatalf("expected TestedCount 1, got %d", p.TestedCount())
	}

	if p.WorkingCount() != 1 {
		t.Fatalf("expected WorkingCount 1, got %d", p.WorkingCount())
	}
}

func TestMarkTestedFailed(t *testing.T) {
	p := New()
	cfg := testConfig("example.com", 1080)

	if err := p.Add(cfg); err != nil {
		t.Fatalf("Add() failed: %v", err)
	}

	id := cfg.Fingerprint()
	expected := errors.New("connection failed")

	if err := p.MarkTested(id, false, expected); err != nil {
		t.Fatalf("MarkTested() failed: %v", err)
	}

	if !p.IsTested(id) {
		t.Fatal("expected configuration to be marked tested")
	}

	if p.IsWorking(id) {
		t.Fatal("expected configuration to be marked failed")
	}

	if p.LastError(id) != expected.Error() {
		t.Fatalf(
			"expected last error %q, got %q",
			expected.Error(),
			p.LastError(id),
		)
	}

	if p.TestedCount() != 1 {
		t.Fatalf("expected TestedCount 1, got %d", p.TestedCount())
	}

	if p.WorkingCount() != 0 {
		t.Fatalf("expected WorkingCount 0, got %d", p.WorkingCount())
	}
}

func TestMarkTestedRejectsMissingConfiguration(t *testing.T) {
	p := New()

	if err := p.MarkTested("missing", true, nil); err == nil {
		t.Fatal("expected missing configuration error")
	}
}

func TestMarkTestedClearsPreviousError(t *testing.T) {
	p := New()
	cfg := testConfig("example.com", 1080)

	if err := p.Add(cfg); err != nil {
		t.Fatalf("Add() failed: %v", err)
	}

	id := cfg.Fingerprint()
	expected := errors.New("temporary failure")

	if err := p.MarkTested(id, false, expected); err != nil {
		t.Fatalf("first MarkTested() failed: %v", err)
	}

	if p.LastError(id) != expected.Error() {
		t.Fatalf("expected stored error %q, got %q", expected, p.LastError(id))
	}

	if err := p.MarkTested(id, true, nil); err != nil {
		t.Fatalf("second MarkTested() failed: %v", err)
	}

	if !p.IsWorking(id) {
		t.Fatal("expected configuration to become working")
	}

	if p.LastError(id) != "" {
		t.Fatalf("expected previous error to be cleared, got %q", p.LastError(id))
	}
}

func TestListIsDeterministic(t *testing.T) {
	p := New()

	configs := []*config.Config{
		testConfig("z.example.com", 1080),
		testConfig("a.example.com", 1080),
		testConfig("m.example.com", 1080),
	}

	for _, cfg := range configs {
		if err := p.Add(cfg); err != nil {
			t.Fatalf("Add() failed: %v", err)
		}
	}

	first := p.List()
	second := p.List()

	if len(first) != len(second) {
		t.Fatalf("list lengths differ: %d vs %d", len(first), len(second))
	}

	for i := range first {
		if first[i].Fingerprint() != second[i].Fingerprint() {
			t.Fatalf(
				"list order is not deterministic at index %d: %s vs %s",
				i,
				first[i].Fingerprint(),
				second[i].Fingerprint(),
			)
		}
	}
}

func TestWorkingTestedAndFailed(t *testing.T) {
	p := New()

	working := testConfig("working.example.com", 1080)
	failed := testConfig("failed.example.com", 1080)
	untested := testConfig("untested.example.com", 1080)

	for _, cfg := range []*config.Config{working, failed, untested} {
		if err := p.Add(cfg); err != nil {
			t.Fatalf("Add() failed: %v", err)
		}
	}

	if err := p.MarkTested(working.Fingerprint(), true, nil); err != nil {
		t.Fatalf("MarkTested(working) failed: %v", err)
	}

	if err := p.MarkTested(
		failed.Fingerprint(),
		false,
		errors.New("probe failed"),
	); err != nil {
		t.Fatalf("MarkTested(failed) failed: %v", err)
	}

	workingList := p.Working()
	testedList := p.Tested()
	failedList := p.Failed()

	if len(workingList) != 1 {
		t.Fatalf("expected one working configuration, got %d", len(workingList))
	}

	if len(testedList) != 2 {
		t.Fatalf("expected two tested configurations, got %d", len(testedList))
	}

	if len(failedList) != 1 {
		t.Fatalf("expected one failed configuration, got %d", len(failedList))
	}

	if workingList[0].Fingerprint() != working.Fingerprint() {
		t.Fatal("working list contains the wrong configuration")
	}

	if failedList[0].Fingerprint() != failed.Fingerprint() {
		t.Fatal("failed list contains the wrong configuration")
	}
}

func TestRemoveClearsRuntimeState(t *testing.T) {
	p := New()
	cfg := testConfig("example.com", 1080)

	if err := p.Add(cfg); err != nil {
		t.Fatalf("Add() failed: %v", err)
	}

	id := cfg.Fingerprint()

	if err := p.MarkTested(id, false, errors.New("failed")); err != nil {
		t.Fatalf("MarkTested() failed: %v", err)
	}

	if !p.IsTested(id) {
		t.Fatal("expected configuration to be tested")
	}

	if err := p.MarkTested(id, true, nil); err != nil {
		t.Fatalf("MarkTested() second call failed: %v", err)
	}

	if !p.Remove(id) {
		t.Fatal("expected Remove() to succeed")
	}

	if p.IsTested(id) {
		t.Fatal("tested state should be removed")
	}

	if p.IsWorking(id) {
		t.Fatal("working state should be removed")
	}

	if p.LastError(id) != "" {
		t.Fatalf("last error should be removed, got %q", p.LastError(id))
	}
}

func TestNilPool(t *testing.T) {
	var p *Pool

	if err := p.Add(testConfig("example.com", 1080)); err == nil {
		t.Fatal("expected Add() on nil pool to fail")
	}

	if err := p.Upsert(testConfig("example.com", 1080)); err == nil {
		t.Fatal("expected Upsert() on nil pool to fail")
	}

	if _, ok := p.Get("missing"); ok {
		t.Fatal("expected Get() on nil pool to return false")
	}

	if p.Remove("missing") {
		t.Fatal("expected Remove() on nil pool to return false")
	}

	if p.Has("missing") {
		t.Fatal("expected Has() on nil pool to return false")
	}

	if err := p.MarkTested("missing", true, nil); err == nil {
		t.Fatal("expected MarkTested() on nil pool to fail")
	}

	if p.IsTested("missing") {
		t.Fatal("expected IsTested() on nil pool to return false")
	}

	if p.IsWorking("missing") {
		t.Fatal("expected IsWorking() on nil pool to return false")
	}

	if p.LastError("missing") != "" {
		t.Fatal("expected LastError() on nil pool to return empty string")
	}

	if p.List() != nil {
		t.Fatal("expected List() on nil pool to return nil")
	}

	if p.Count() != 0 {
		t.Fatalf("expected Count() on nil pool to return 0, got %d", p.Count())
	}

	if p.WorkingCount() != 0 {
		t.Fatalf(
			"expected WorkingCount() on nil pool to return 0, got %d",
			p.WorkingCount(),
		)
	}

	if p.TestedCount() != 0 {
		t.Fatalf(
			"expected TestedCount() on nil pool to return 0, got %d",
			p.TestedCount(),
		)
	}
}
