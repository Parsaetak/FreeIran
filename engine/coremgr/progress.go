package coremgr

import (
	"sync"
	"time"
)

// InstallStage identifies one step of the install pipeline. The UI
// renders it as the progress line of an installing core.
type InstallStage string

const (
	StageResolveRelease InstallStage = "resolve_release"
	StageDownload       InstallStage = "download"
	StageVerifyChecksum InstallStage = "verify_checksum"
	StageUnpack         InstallStage = "unpack"
	StageLocate         InstallStage = "locate_executable"
	StageValidate       InstallStage = "validate_executable"
	StageActivate       InstallStage = "activate"
	StageSmokeTest      InstallStage = "smoke_test"
	StageComplete       InstallStage = "complete"
	StageFailed         InstallStage = "failed"
)

// InstallProgress is one progress event of a running install/update.
type InstallProgress struct {
	Core       CoreName     `json:"core"`
	Stage      InstallStage `json:"stage"`
	Message    string       `json:"message,omitempty"`
	BytesDone  int64        `json:"bytes_done,omitempty"`
	BytesTotal int64        `json:"bytes_total,omitempty"`
	At         time.Time    `json:"at"`
}

// progressListener is the process-wide install progress sink. The
// app service registers one callback and forwards events to the UI;
// the manager itself stays UI-agnostic.
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
