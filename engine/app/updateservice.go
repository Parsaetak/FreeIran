// updateservice.go surfaces the application-update CHECK stage
// (internal/appupdate) on the Wails service surface (v0.9.4 §16/§20).
//
// It is the same artifact pipeline the managed-core updater uses,
// pointed at the application's own trusted release feed: resolve the
// latest release → select the platform asset → compare versions →
// surface availability together with the checksum sidecar URL the
// downloader must verify before staging. No download, no staging, no
// side effects happen from this service — the UI shows an
// "update available" badge and the install flow (download → verify →
// stage → activate → rollback, reusing engine/coremgr primitives)
// lands on top of it in a later phase.
package app

import (
	"context"
	"runtime"
	"time"

	"github.com/Parsaetak/FreeIran/internal/appupdate"
	"github.com/Parsaetak/FreeIran/internal/httpx"
	"github.com/Parsaetak/FreeIran/internal/version"
)

// AppUpdateService is the Wails-facing application-update checker.
type AppUpdateService struct {
	app *App
}

// NewAppUpdateService binds the checker to the running application.
func NewAppUpdateService(app *App) *AppUpdateService {
	return &AppUpdateService{app: app}
}

// CheckApplicationUpdate queries the trusted release feed once and
// returns the availability outcome. It never blocks long (15 s
// budget, retries and Retry-After waits included) and never mutates
// anything: a failed check returns an error the UI can surface as
// "could not check for updates".
//
// v0.9.5: the ad-hoc per-call http.Client (with its own duplicated
// timeout policy) is gone — the ONE shared production client
// (httpx.Default) serves the application updater, the core manager
// and the source fetcher alike.
func (s *AppUpdateService) CheckApplicationUpdate() (appupdate.Info, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	return appupdate.Check(ctx, httpx.Default(), "", version.Version, runtime.GOOS)
}
