// importservice.go implements the v0.10.2 P0 feature "personal
// configuration import": a user must be able to use THEIR OWN
// configuration without first creating a subscription source.
//
// The flow (§ Import Configuration):
//
//	paste / file
//	→ detect format          (parser: URL list / base64 subscription / JSON)
//	→ parse                  (engine/parser — the ONE parser, no second pipeline)
//	→ normalize + validate   (config.Normalize/Validate + backend Validate)
//	→ capability preview     (which installed cores can EXECUTE each config)
//	→ redacted preview       (no credentials in the preview surface)
//	→ Save                   (existing store + trust model)
//	→ Test                   (existing TestQueue — user action)
//	→ Connect                (existing connection engine — user action)
//
// Guarantees:
//
//   - Reuse: parsing goes through engine/parser, persistence through
//     the engine/pipeline StoreSink + engine/store, capability
//     resolution through the live core registry. There is NO second
//     configuration pipeline, no second store, no duplicate trust
//     model.
//   - Trust: imported configs carry SourceTrustUser — the same tier a
//     user-configured source gets. They never bypass route trust,
//     testing or verification; connecting still runs the standard
//     gates.
//   - Honesty: the preview reports, per entry, whether an INSTALLED
//     core can actually execute it (Supports + Validate). A parsed
//     protocol no core can run is shown as such — never silently
//     saved-and-never-usable. (Since v0.10.2 the sing-box adapter
//     executes hysteria2/tuic/wireguard/hysteria; the preview is
//     still the authority because it consults the live registry.)
//   - Redaction: preview surfaces carry host:port + protocol +
//     capability facts, never secrets. Credentials stay in the saved
//     record and are subject to the existing redaction contract.
//   - Idempotence: saving re-parses the SAME payload and upserts by
//     fingerprint — re-importing the same config updates in place
//     (stable IDs), it never duplicates.

package app

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/engine/parser"
	"github.com/Parsaetak/FreeIran/engine/pipeline"
)

// personalImportSource is the durable source label for user-pasted
// configurations (visible in the config's import-source column).
const personalImportSource = "personal-import"

// ImportService implements first-class personal configuration import.
type ImportService struct {
	app *App
}

// NewImportService binds the import service to the app.
func NewImportService(a *App) *ImportService {
	return &ImportService{app: a}
}

// ImportedConfigView is the redacted preview row for one parsed
// configuration: identity + capability truth, no credentials.
type ImportedConfigView struct {
	Index     int      `json:"index"`
	Name      string   `json:"name"`
	Protocol  string   `json:"protocol"`
	Address   string   `json:"address"`
	Port      int      `json:"port"`
	Transport string   `json:"transport,omitempty"`
	Security  string   `json:"security,omitempty"`
	Redacted  string   `json:"redacted"`
	Backends  []string `json:"backends,omitempty"`
	// Executable is TRUE only when at least one INSTALLED backend
	// both declares the protocol and accepts the config (deep
	// validation). This is the "will it actually run" answer.
	Executable bool `json:"executable"`
	// ConfigID is the deterministic fingerprint ID the config will
	// carry after saving (connect-from-paste targets THIS id).
	ConfigID string `json:"config_id"`
	// Warnings carries honesty notes (e.g. insecure TLS requested).
	Warnings []string `json:"warnings,omitempty"`
}

// ImportRejected explains one entry that could not be parsed or
// validated. Snippet is a bounded, redacted fragment for recognition.
type ImportRejected struct {
	Index   int    `json:"index"`
	Reason  string `json:"reason"`
	Snippet string `json:"snippet,omitempty"`
}

// ImportPreview is the full preview result.
type ImportPreview struct {
	Format   string               `json:"format"`
	Total    int                  `json:"total"`
	Imported []ImportedConfigView `json:"imported"`
	Rejected []ImportRejected     `json:"rejected"`
}

// ImportResult reports what SaveImportedConfigs persisted.
type ImportResult struct {
	SavedCount    int      `json:"saved_count"`
	UpdatedCount  int      `json:"updated_count"`
	RejectedCount int      `json:"rejected_count"`
	ConfigIDs     []string `json:"config_ids"`
	ExecutableIDs []string `json:"executable_ids,omitempty"`
}

// PreviewImport parses, validates and capability-resolves the payload
// WITHOUT persisting anything. It is the read-only half of the import
// flow; SaveImportedConfigs is the durable half.
func (s *ImportService) PreviewImport(payload string) (ImportPreview, error) {
	preview, _, err := s.resolve(payload)
	if err != nil {
		return ImportPreview{}, err
	}

	return preview, nil
}

// SaveImportedConfigs re-parses the payload and persists every
// parseable configuration through the ONE store path. Re-parsing (vs
// caching preview state) keeps the service stateless: the saved truth
// is always derived from the same bytes the user pasted.
func (s *ImportService) SaveImportedConfigs(payload string) (ImportResult, error) {
	preview, parsed, err := s.resolve(payload)
	if err != nil {
		return ImportResult{}, err
	}

	if len(parsed) == 0 {
		return ImportResult{}, firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "import",
			"nothing importable found in the payload (%d rejected entries)",
			len(preview.Rejected))
	}

	// Update accounting BEFORE persist: the store's upsert is
	// idempotent per fingerprint, so a config that already exists is
	// an UPDATE, not a duplicate.
	alreadyPresent := 0
	for i := range parsed {
		if s.app.store.Has(parsed[i].Fingerprint()) {
			alreadyPresent++
		}
	}

	// Persist through the existing store sink (batch + flush).
	sink := pipeline.NewStoreSink(s.app.store, 0)

	if err := sink.Persist(s.app.ctx, parsed); err != nil {
		return ImportResult{}, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "import", "persist imported configurations")
	}

	if err := sink.Flush(); err != nil {
		return ImportResult{}, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "import", "flush imported configurations")
	}

	result := ImportResult{
		RejectedCount: len(preview.Rejected),
	}

	seen := map[string]bool{}

	for _, view := range preview.Imported {
		if seen[view.ConfigID] {
			continue
		}

		seen[view.ConfigID] = true
		result.ConfigIDs = append(result.ConfigIDs, view.ConfigID)

		if view.Executable {
			result.ExecutableIDs = append(result.ExecutableIDs, view.ConfigID)
		}
	}

	result.SavedCount = len(parsed)
	result.UpdatedCount = alreadyPresent

	s.app.logger.Info("import", "imported",
		"personal import saved %d configs (%d executable, %d rejected)",
		result.SavedCount, len(result.ExecutableIDs), result.RejectedCount)

	return result, nil
}

// resolve is the shared parse → normalize → validate →
// capability-resolve pipeline for Preview and Save. It returns the
// preview AND the persistable configs (in preview order).
func (s *ImportService) resolve(payload string) (ImportPreview, []config.Config, error) {
	if strings.TrimSpace(payload) == "" {
		return ImportPreview{}, nil, firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "import", "the payload is empty")
	}

	if len(payload) > maxImportPayloadBytes {
		return ImportPreview{}, nil, firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "import",
			"payload exceeds %d bytes", maxImportPayloadBytes)
	}

	// The ONE parser (URL lists, base64 subscriptions, JSON).
	parsed, stats, parseErr := parser.New().ParseDetailed([]byte(payload))
	if parseErr != nil && len(parsed) == 0 {
		return ImportPreview{}, nil, firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "import",
			"the payload is not a recognizable configuration format: %v", parseErr)
	}

	format := importFormat(payload)

	preview := ImportPreview{
		Format:   format,
		Total:    stats.Discovered,
		Imported: []ImportedConfigView{},
		Rejected: []ImportRejected{},
	}

	// The parser aggregates per-entry failures into one error; surface
	// it honestly as the rejection reason alongside the count.
	if parseErr != nil {
		preview.Rejected = append(preview.Rejected, ImportRejected{
			Reason: boundedReason(parseErr.Error()),
		})
	}

	if stats.Rejected > 0 {
		preview.Rejected = append(preview.Rejected, ImportRejected{
			Reason: fmt.Sprintf("%d entries failed structural validation", stats.Rejected),
		})
	}

	// Capability resolution against the LIVE registry: only installed
	// backends count.
	backends := s.installedBackends()

	for i := range parsed {
		// Personal import marks: the user's own configuration. Applied
		// to the SLICE element (the saved record must carry them).
		parsed[i].Source = personalImportSource
		parsed[i].SourceTrust = config.SourceTrustUser

		cfg := parsed[i]

		view := ImportedConfigView{
			Index:     i,
			Name:      cfg.Name,
			Protocol:  string(cfg.Type),
			Address:   cfg.Address,
			Port:      cfg.Port,
			Transport: cfg.Network,
			Security:  cfg.Security,
			Redacted:  cfg.DisplayURL(),
			ConfigID:  cfg.Fingerprint(),
		}

		if cfg.Insecure {
			view.Warnings = append(view.Warnings,
				"TLS verification disabled (insecure) — the server certificate is not validated")
		}

		if cfg.Type == config.TypeWireGuard && strings.TrimSpace(cfg.PrivateKey) == "" {
			view.Warnings = append(view.Warnings,
				"no private key in the payload — the configuration cannot execute")
		}

		for _, backend := range backends {
			if !backend.Supports(cfg) {
				continue
			}

			view.Backends = append(view.Backends, backend.Name())

			if err := backend.Validate(s.app.ctx, cfg); err == nil {
				view.Executable = true
			}
		}

		sort.Strings(view.Backends)

		preview.Imported = append(preview.Imported, view)
	}

	// The persistable list: every parsed config with the personal
	// source/trust marks applied (rejected entries were never parsed).
	return preview, parsed, nil
}

// installedBackends returns the registry's backends in deterministic
// order.
func (s *ImportService) installedBackends() []core.Core {
	names := []string{"xray", "v2ray", "sing-box"}

	out := make([]core.Core, 0, len(names))

	for _, name := range names {
		if backend, ok := s.app.coreRegistry.Get(name); ok {
			out = append(out, backend)
		}
	}

	return out
}

// importFormat classifies the payload for the preview header.
func importFormat(payload string) string {
	trimmed := strings.TrimSpace(payload)

	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return "json"
	}

	if decoded, ok := parser.DecodeBase64Subscription(trimmed); ok {
		_ = decoded

		return "base64-subscription"
	}

	return "url-list"
}

// maxImportPayloadBytes bounds one import payload (a subscription
// page can carry hundreds of nodes; this is generous but bounded).
const maxImportPayloadBytes = 4 << 20 // 4 MiB

// boundedReason trims parser error text for the preview surface.
func boundedReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) > 300 {
		reason = reason[:297] + "..."
	}

	return reason
}
