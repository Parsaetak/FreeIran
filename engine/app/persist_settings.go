// persistSettings applies a mutation to the settings atomically:
// validate → persist → apply. Shared by the provider service so all
// settings writes follow ONE code path.
package app

import (
	"encoding/json"
	"fmt"

	"github.com/Parsaetak/FreeIran/system"
)

func (a *App) persistSettings(mutate func(*Settings)) error {
	// v0.9.12: the whole read→mutate→validate→write→memory-update
	// cycle holds the shared settings write mutex — concurrent
	// settings writers (SettingsService.Save) serialize on the same
	// lock, so last-writer-wins is consistent in memory AND on disk.
	a.settingsWrite.Lock()
	defer a.settingsWrite.Unlock()

	settings := a.currentSettings()

	mutate(&settings)

	if err := validateSettings(settings); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(&settings, "", "  ")
	if err != nil {
		return fmt.Errorf("app: encode settings: %w", err)
	}

	if err := system.WriteFileAtomic(a.settingsPath(), raw, 0o600); err != nil {
		return fmt.Errorf("app: persist settings: %w", err)
	}

	a.mu.Lock()
	a.settings = settings
	a.mu.Unlock()

	a.applySettings(settings)

	return nil
}
