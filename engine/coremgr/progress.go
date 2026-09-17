package coremgr

import (
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/internal/httpx"
)

// InstallStage identifies one step of the install pipeline. The UI
// renders the unified lifecycle:
//
//	Resolving → Downloading → Verifying → Unpacking →
//	Validating → Activating → Complete
//
// (StageFailed terminates a failed run.) Every stage represents REAL
// work being performed — no fake progress.
type InstallStage string

const (
	// StageResolving covers release resolution and asset selection.
	StageResolving InstallStage = "resolving"

	// StageDownloading covers the streamed, resumable asset transfer.
	StageDownloading InstallStage = "downloading"

	// StageVerifying covers size + SHA-256 verification.
	StageVerifying InstallStage = "verifying"

	// StageUnpacking covers archive extraction.
	StageUnpacking InstallStage = "unpacking"

	// StageValidating covers version probe, config validation AND the
	// smoke test of the staged executable.
	StageValidating InstallStage = "validating"

	// StageActivating covers the atomic activation (rollback
	// retention + rename into bin/).
	StageActivating InstallStage = "activating"

	// StageComplete is the successful end state.
	StageComplete InstallStage = "complete"

	// StageFailed is the failed end state (Message carries the
	// actionable reason).
	StageFailed InstallStage = "failed"
)

// InstallStages lists the lifecycle in order (for UIs that render a
// step indicator).
var InstallStages = []InstallStage{
	StageResolving,
	StageDownloading,
	StageVerifying,
	StageUnpacking,
	StageValidating,
	StageActivating,
	StageComplete,
}

// InstallProgress is one progress event of a running install/update.
//
// Byte/speed/ETA telemetry is populated during StageDownloading from
// the downloader's real measurements; Retries counts resume attempts;
// ResumedBytes is the byte offset a resumed download continued from.
type InstallProgress struct {
	Core       CoreName     `json:"core"`
	Stage      InstallStage `json:"stage"`
	Message    string       `json:"message,omitempty"`
	BytesDone  int64        `json:"bytes_done,omitempty"`
	BytesTotal int64        `json:"bytes_total,omitempty"`

	// SpeedBPS is the measured download throughput (bytes/second).
	SpeedBPS float64 `json:"speed_bps,omitempty"`

	// ETASeconds estimates the remaining download time (-1 unknown).
	ETASeconds float64 `json:"eta_seconds,omitempty"`

	// Retries counts download resume attempts.
	Retries int `json:"retries,omitempty"`

	// ResumedBytes is the offset a resumed download continued from.
	ResumedBytes int64 `json:"resumed_bytes,omitempty"`

	At time.Time `json:"at"`
}

// progressListener is the process-wide install progress sink. The app
// service registers one callback and forwards events to the UI; the
// manager itself stays UI-agnostic.
var (
	progressMu       sync.Mutex
	progressListener func(InstallProgress)
)

// OnProgress registers the install progress listener. Passing nil
// removes the current listener. The callback must be cheap and
// non-blocking: it is invoked synchronously on the install path.
func OnProgress(fn func(InstallProgress)) {
	progressMu.Lock()
	defer progressMu.Unlock()

	progressListener = fn
}

// emitProgress delivers one event to the registered listener (if any).
func emitProgress(core CoreName, stage InstallStage, message string, done, total int64) {
	progressMu.Lock()
	fn := progressListener
	progressMu.Unlock()

	if fn == nil {
		return
	}

	event := InstallProgress{
		Core:       core,
		Stage:      stage,
		Message:    message,
		BytesDone:  done,
		BytesTotal: total,
		At:         time.Now().UTC(),
	}

	// A panicking listener must never take the install pipeline down.
	defer func() { _ = recover() }()

	fn(event)
}

// emitDownloadProgress translates one httpx download telemetry sample
// into an install progress event.
func emitDownloadProgress(core CoreName, p httpx.Progress) {
	progressMu.Lock()
	fn := progressListener
	progressMu.Unlock()

	if fn == nil {
		return
	}

	event := InstallProgress{
		Core:         core,
		Stage:        StageDownloading,
		BytesDone:    p.BytesDone,
		BytesTotal:   p.BytesTotal,
		SpeedBPS:     p.SpeedBPS,
		ETASeconds:   p.ETASeconds,
		Retries:      p.Retries,
		ResumedBytes: p.ResumedFrom,
		At:           time.Now().UTC(),
	}

	if p.ResumedFrom > 0 {
		event.Message = formatResumedMessage(p)
	}

	defer func() { _ = recover() }()

	fn(event)
}

// formatResumedMessage renders the "resumed from X MB" line the UI
// shows when a download continued from a durable prefix.
func formatResumedMessage(p httpx.Progress) string {
	return "resumed from " + formatSize(p.ResumedFrom)
}
