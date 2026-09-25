package app

// importservice_test.go — the v0.10.2 personal-import contract:
// paste/file → detect → parse → normalize → validate → capability
// preview → redacted preview → Save, all through the ONE parser /
// store / capability architecture (no second pipeline).
//
// Pinned here:
//
//   - URL lists, base64 subscriptions and WireGuard INI payloads are
//     all importable from the paste surface (no subscription source
//     required).
//   - The preview is REDACTED (no credentials) and honest about
//     executability (which installed backends accept each config).
//   - Saving persists through the existing store with the
//     personal-import source and user trust tier.
//   - Re-importing the same payload is an UPDATE (stable fingerprint
//     IDs), never a duplicate.
//   - The store count reflects the saved configs; connection can
//     target the exact imported ID.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
)

func TestImportPreviewRedactsAndReportsCapabilities(t *testing.T) {
	a := newTestApp(t)
	svc := NewImportService(a)

	payload := "vless://secret-uuid-1@srv1.example.com:443?security=tls&type=ws#node-one\n" +
		"socks://user:pass@127.0.0.1:1080#local-socks\n"

	preview, err := svc.PreviewImport(payload)
	if err != nil {
		t.Fatalf("PreviewImport: %v", err)
	}

	if preview.Format != "url-list" {
		t.Errorf("format = %q, want url-list", preview.Format)
	}

	if len(preview.Imported) != 2 {
		t.Fatalf("imported = %d rows, want 2 (got %+v)", len(preview.Imported), preview.Imported)
	}

	first := preview.Imported[0]

	if first.Protocol != "vless" || first.Address != "srv1.example.com" || first.Port != 443 {
		t.Errorf("row 0 = %+v, want the vless server facts", first)
	}

	// The redacted surface must not leak the credential.
	if containsString(first.Redacted, "secret-uuid-1") {
		t.Errorf("redacted display %q leaks the UUID", first.Redacted)
	}

	// VLESS over TLS+WS: sing-box declares it; the deep validation is
	// capability-truthful (xray supports it too when installed).
	if !first.Executable {
		t.Errorf("vless tls/ws must be executable by the installed backends (backends=%v)", first.Backends)
	}

	if len(first.Backends) == 0 {
		t.Error("executable config must list at least one backend")
	}

	if first.ConfigID == "" {
		t.Error("preview must carry the deterministic config id")
	}
}

func TestImportBase64Subscription(t *testing.T) {
	a := newTestApp(t)
	svc := NewImportService(a)

	raw := fmt.Sprintf(
		"trojan://pw-%d@trojan%d.example.com:443?security=tls#t%d\nss://YWVzLTI1Ni1nY206c2VjcmV0@ss%d.example.com:8388#s%d\n",
		1, 1, 1, 1, 1)

	encoded := base64Encode([]byte(raw))

	preview, err := svc.PreviewImport(encoded)
	if err != nil {
		t.Fatalf("PreviewImport(base64): %v", err)
	}

	if preview.Format != "base64-subscription" {
		t.Errorf("format = %q, want base64-subscription", preview.Format)
	}

	if len(preview.Imported) < 2 {
		t.Fatalf("imported = %d rows, want >= 2 from the subscription", len(preview.Imported))
	}
}

func TestImportWireGuardINIWithoutSubscription(t *testing.T) {
	a := newTestApp(t)
	svc := NewImportService(a)

	// v0.10.3: key material is DERIVED at runtime — committed
	// key-shaped literals trip the Gitleaks wireguard rule even when
	// synthetic. deterministicTestKey yields the same 44-char
	// base64 every run (deterministic coverage, no committed
	// secret-looking literal, never a real credential).
	ini := "[Interface]\n" +
		"PrivateKey = " + deterministicTestKey(0x21) + "\n" +
		"Address = 10.7.0.2/32\n" +
		"DNS = 1.1.1.1\n\n" +
		"[Peer]\n" +
		"PublicKey = " + deterministicTestKey(0x42) + "\n" +
		"Endpoint = 192.0.2.10:51820\n" +
		"AllowedIPs = 0.0.0.0/0\n"

	preview, err := svc.PreviewImport(ini)
	if err != nil {
		t.Fatalf("PreviewImport(wg ini): %v", err)
	}

	if len(preview.Imported) != 1 {
		t.Fatalf("imported = %d rows, want 1 (rejected=%+v)", len(preview.Imported), preview.Rejected)
	}

	row := preview.Imported[0]
	if row.Protocol != string(config.TypeWireGuard) {
		t.Errorf("protocol = %q, want wireguard", row.Protocol)
	}
}

func TestImportSavePersistsAndIsIdempotent(t *testing.T) {
	a := newTestApp(t)
	svc := NewImportService(a)

	payload := "vless://uuid-imported@srv-import.example.com:443?security=tls&type=tcp#imported-node\n"

	result, err := svc.SaveImportedConfigs(payload)
	if err != nil {
		t.Fatalf("SaveImportedConfigs: %v", err)
	}

	if result.SavedCount != 1 || len(result.ConfigIDs) != 1 {
		t.Fatalf("result = %+v, want exactly one saved config", result)
	}

	if len(result.ExecutableIDs) != 1 {
		t.Errorf("executable accounting wrong: %+v", result)
	}

	if a.store.Count() == 0 {
		t.Fatal("store count is zero after import")
	}

	// The saved record carries the personal-import source + user
	// trust tier.
	raw, err := a.store.Get(result.ConfigIDs[0])
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}

	var saved config.Config

	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("unmarshal saved config: %v", err)
	}

	if saved.Source != "personal-import" {
		t.Errorf("source = %q, want personal-import", saved.Source)
	}

	if saved.SourceTrust != config.SourceTrustUser {
		t.Errorf("trust = %q, want user", saved.SourceTrust)
	}

	// Re-import: same fingerprint → update, not duplicate.
	second, err := svc.SaveImportedConfigs(payload)
	if err != nil {
		t.Fatalf("second SaveImportedConfigs: %v", err)
	}

	if second.SavedCount != 1 || second.UpdatedCount != 1 {
		t.Fatalf("second result = %+v, want 1 saved / 1 updated", second)
	}

	if second.ConfigIDs[0] != result.ConfigIDs[0] {
		t.Fatalf("re-import changed the config id: %s vs %s", second.ConfigIDs[0], result.ConfigIDs[0])
	}
}

func TestImportReportsRejectionsHonesty(t *testing.T) {
	a := newTestApp(t)
	svc := NewImportService(a)

	payload := "vless://ok-uuid@ok.example.com:443?security=tls#ok\n" +
		"not-a-config-line\n" +
		"ftp://wrong-scheme.example.com:21#bad\n"

	preview, err := svc.PreviewImport(payload)
	if err != nil {
		t.Fatalf("PreviewImport: %v", err)
	}

	if len(preview.Imported) != 1 {
		t.Fatalf("imported = %d, want 1", len(preview.Imported))
	}

	if len(preview.Rejected) == 0 {
		t.Fatal("rejections must be reported, not swallowed")
	}

	for _, row := range preview.Rejected {
		if row.Reason == "" {
			t.Error("every rejection must carry a reason")
		}
	}
}

func TestImportEmptyPayloadFails(t *testing.T) {
	a := newTestApp(t)
	svc := NewImportService(a)

	if _, err := svc.PreviewImport("   \n  "); err == nil {
		t.Fatal("an empty payload must fail with an explicit error")
	}

	if _, err := svc.SaveImportedConfigs(""); err == nil {
		t.Fatal("saving an empty payload must fail")
	}
}

// --- tiny helpers (avoid importing encoding/json/base64 per file) ---

func containsString(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}

func base64Encode(data []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

	var out []byte

	for i := 0; i < len(data); i += 3 {
		var chunk [3]byte

		n := copy(chunk[:], data[i:])
		b := uint32(chunk[0])<<16 | uint32(chunk[1])<<8 | uint32(chunk[2])

		out = append(out, alphabet[(b>>18)&0x3f], alphabet[(b>>12)&0x3f])

		if n > 1 {
			out = append(out, alphabet[(b>>6)&0x3f])
		} else {
			out = append(out, '=')
		}

		if n > 2 {
			out = append(out, alphabet[b&0x3f])
		} else {
			out = append(out, '=')
		}
	}

	return string(out)
}

// deterministicTestKey derives a syntactically valid 32-byte base64
// key from a seed byte — the v0.10.3 Gitleaks contract for test
// material: deterministic across runs, syntactically valid (32 bytes
// → 44-char padded base64), never a real credential, and never a
// committed secret-looking literal (the wireguard rule cannot match
// a value that only exists at runtime).
func deterministicTestKey(seed byte) string {
	key := make([]byte, 32)

	for i := range key {
		key[i] = seed + byte(i)
	}

	return base64.StdEncoding.EncodeToString(key)
}
