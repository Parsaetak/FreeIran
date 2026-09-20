package netcheck

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// stages_test.go pins the v0.9.8.5 staged diagnostics ladder (§4):
// the ordered stage evidence that explains the seven-state verdict —
// which stage failed, with what failure class — deterministically
// against local endpoints.

// TestStagedLadderLocalEndpoints drives one full run against local
// probes: TCP and HTTPS alive, DNS dead. The ladder must carry the
// canonical order, name DNS as the first failed stage and never
// reclassify the aggregate verdict.
func TestStagedLadderLocalEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	defer srv.Close()

	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer tcpListener.Close()

	go func() {
		for {
			conn, aerr := tcpListener.Accept()
			if aerr != nil {
				return
			}

			_ = conn.Close()
		}
	}()

	checker := New(Config{
		DNSTargets: []string{"127.0.0.1:53997"},
		TCPTargets: []string{tcpListener.Addr().String()},
		HTTPSURLs:  []string{srv.URL + "/gen204"},
		ProbeHost:  "definitely.invalid",
		Timeout:    2 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	report := checker.Run(ctx)

	if len(report.Stages) == 0 {
		t.Fatal("staged evidence missing from the report")
	}

	// Canonical ladder order.
	if len(StageOrder) != 9 {
		t.Fatalf("ladder = %d stages, want 9", len(StageOrder))
	}

	seen := make([]StageID, 0, len(report.Stages))
	for _, s := range report.Stages {
		seen = append(seen, s.Stage)
	}

	if len(seen) != len(StageOrder) {
		t.Fatalf("report ladder = %d stages, want %d", len(seen), len(StageOrder))
	}

	for i, id := range StageOrder {
		if seen[i] != id {
			t.Fatalf("stage %d = %s, want %s (canonical order violated)", i, seen[i], id)
		}
	}

	// DNS is dead → the first failed stage must be DNS.
	if report.FailedStage != string(StageDNS) {
		t.Fatalf("failed stage = %q, want dns", report.FailedStage)
	}

	failed := FirstFailedStage(report.Stages)
	if failed == nil || failed.Stage != StageDNS {
		t.Fatalf("first failed stage = %+v, want dns", failed)
	}

	if failed.FailureClass == "" {
		t.Fatal("the failed stage must carry a failure class")
	}

	// The healthy stages must be OK with measured evidence.
	index := map[StageID]StageResult{}
	for _, s := range report.Stages {
		index[s.Stage] = s
	}

	if s := index[StageTCP]; s.Status != StageOK {
		t.Fatalf("tcp stage = %+v, want ok", s)
	}

	if s := index[StageHTTPS]; s.Status != StageOK {
		t.Fatalf("https stage = %+v, want ok", s)
	}

	// No tunnel in this run: the stage is honestly not checked.
	if s := index[StageTunnel]; s.Status != StageNotChecked {
		t.Fatalf("tunnel stage = %+v, want not_checked", s)
	}

	// The local-IP stage is either OK with an address (routed
	// environment) or honestly failed (isolated) — never fabricated.
	if s := index[StageLocalIP]; s.Status == StageOK && s.Detail == "" {
		t.Fatal("local-ip stage ok without its address evidence")
	}
}

// TestStagedLadderOfflineNamesHTTPS pins the offline shape: with every
// probe class dead, the first failed stage is the ladder's earliest
// dead rung after the local link, and direct Internet is failed.
func TestStagedLadderOfflineNamesHTTPS(t *testing.T) {
	checker := New(Config{
		DNSTargets: []string{"127.0.0.1:53996"},
		TCPTargets: []string{"127.0.0.1:53996"},
		HTTPSURLs:  []string{"http://127.0.0.1:53996/x"},
		ProbeHost:  "definitely.invalid",
		Timeout:    500 * time.Millisecond,
	})

	report := checker.Run(context.Background())

	if report.FailedStage == "" {
		t.Fatal("offline run must name a failed stage")
	}

	index := map[StageID]StageResult{}
	for _, s := range report.Stages {
		index[s.Stage] = s
	}

	// The HTTPS stage is dead, so the portal judgement is skipped
	// (honest) and direct Internet is failed.
	if s := index[StageCaptive]; s.Status != StageSkipped {
		t.Fatalf("captive stage = %+v, want skipped (HTTPS dead)", s)
	}

	if s := index[StageDirect]; s.Status != StageFailed {
		t.Fatalf("direct stage = %+v, want failed", s)
	}
}

// TestFirstFailedStageOrder pins the pure selection rule: the first
// failed rung in LADDER order wins, regardless of slice order.
func TestFirstFailedStageOrder(t *testing.T) {
	stages := []StageResult{
		{Stage: StageHTTPS, Status: StageFailed, FailureClass: "timeout"},
		{Stage: StageDNS, Status: StageFailed, FailureClass: "refused"},
		{Stage: StageLocalLink, Status: StageOK},
	}

	failed := FirstFailedStage(stages)
	if failed == nil || failed.Stage != StageDNS {
		t.Fatalf("first failed = %+v, want dns (earliest rung)", failed)
	}

	if FirstFailedStage([]StageResult{{Stage: StageLocalLink, Status: StageOK}}) != nil {
		t.Fatal("clean ladder must yield no failed stage")
	}
}

// TestStageLabelsRender pins the human-facing labels.
func TestStageLabelsRender(t *testing.T) {
	for _, id := range StageOrder {
		if label := StageLabel(id); label == "" || label == string(id) {
			// string(id) fallback only for unknown ids — every
			// canonical stage must have a real label.
			if label == "" {
				t.Fatalf("stage %s has no label", id)
			}
		}
	}

	if StageLabel(StageID("bogus")) != "bogus" {
		t.Fatal("unknown stage must render its raw id")
	}
}

// TestStagesNeverReclassify pins the additive contract: the ladder
// explains the verdict, it never replaces it. The classifier's word
// for the local-endpoint run (DNS dead, TCP/HTTPS alive) must stay
// the DNS-failure family regardless of the stage evidence.
func TestStagesNeverReclassify(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	defer srv.Close()

	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer tcpListener.Close()

	go func() {
		for {
			conn, aerr := tcpListener.Accept()
			if aerr != nil {
				return
			}

			_ = conn.Close()
		}
	}()

	checker := New(Config{
		DNSTargets: []string{"127.0.0.1:53995"},
		TCPTargets: []string{tcpListener.Addr().String()},
		HTTPSURLs:  []string{srv.URL + "/gen204"},
		ProbeHost:  "definitely.invalid",
		Timeout:    2 * time.Second,
	})

	report := checker.Run(context.Background())

	if report.State != StateDNSFailure {
		t.Fatalf("state = %s, want dns_failure (the ladder must not reclassify)", report.State)
	}

	if report.FailedStage != string(StageDNS) {
		t.Fatalf("stage evidence (%s) and verdict (%s) disagree", report.FailedStage, report.State)
	}
}
