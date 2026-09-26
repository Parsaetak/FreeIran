package app

// bindings_contract_test.go pins the v0.9.8.6 Wails contract.
//
// The committed frontend bindings are of TWO generations:
//
//   - machine-generated files (appservice.js, connectionservice.js,
//     …) from the wails3 generator: they call $Call.ByID(<hash>) and
//     carry the "automatically generated, DO NOT EDIT" header;
//   - hand-written files (coreservice.js, discoveryservice.js,
//     networkservice.js, testqueueservice.js, tunnelservice.js): they
//     call $Call.ByName("<prefix>.<Method>").
//
// The ByID hashes are produced by the wails3 toolchain and resolved
// by @wailsio/runtime — which MUST be the same beta version as the
// Go wails/v3 module (CI enforces the version pair; at v0.9.8.5 the
// lockfile resolved runtime beta.20 against Go beta.19 — exactly the
// drift this release eliminates).
//
// This test guards the ByName half of the contract statically: every
// hand-written $Call.ByName target MUST exist as an exported method
// on the corresponding Go service. A renamed or deleted method fails
// HERE, before it can break the runtime call surface. The ByID files
// are verified structurally (their service name must match a
// registered service type).
//
// v0.9.12: the contract is FIELD-EXACT for hand-maintained models.
// profiletypes.js mirrors the Go structs in profiles.go by hand (the
// wails3 generator cannot run on the current host) — so CI now
// reflects over the Go structs and proves the JSDoc @property set,
// names, and optionality match field-for-field. Silent model drift
// (a Go field added without updating the binding, a renamed JSON tag,
// a wrong optional marker) fails the job instead of surfacing as
// undefined fields in the UI at runtime.

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// registeredServices mirrors the application.NewService registrations
// in cmd/freeiran/main.go: binding prefix → service instance.
func registeredServices(t *testing.T) map[string]any {
	t.Helper()

	a := newTestApp(t)

	return map[string]any{
		"github.com/Parsaetak/FreeIran/engine/app.AppService":           NewAppService(a),
		"github.com/Parsaetak/FreeIran/engine/app.SourceService":        NewSourceService(a),
		"github.com/Parsaetak/FreeIran/engine/app.DataService":          NewDataService(a),
		"github.com/Parsaetak/FreeIran/engine/app.StorageService":       NewStorageService(a),
		"github.com/Parsaetak/FreeIran/engine/app.DiagnosticsService":   NewDiagnosticsService(a),
		"github.com/Parsaetak/FreeIran/engine/app.ConnectionService":    NewConnectionService(a),
		"github.com/Parsaetak/FreeIran/engine/app.LogService":           NewLogService(a),
		"github.com/Parsaetak/FreeIran/engine/app.SettingsService":      NewSettingsService(a),
		"github.com/Parsaetak/FreeIran/engine/app.CoreService":          NewCoreService(a),
		"github.com/Parsaetak/FreeIran/engine/app.TestQueueService":     NewTestQueueService(a),
		"github.com/Parsaetak/FreeIran/engine/app.TunnelService":        NewTunnelService(a),
		"github.com/Parsaetak/FreeIran/engine/app.NetworkService":       NewNetworkService(a),
		"github.com/Parsaetak/FreeIran/engine/app.DiscoveryService":     NewDiscoveryService(a),
		"github.com/Parsaetak/FreeIran/engine/app.ProviderService":      NewProviderService(a),
		"github.com/Parsaetak/FreeIran/engine/app.InternetToolsService": NewInternetToolsService(a),
		// v0.9.10: favorites, user groups and the source
		// reliability dashboard.
		"github.com/Parsaetak/FreeIran/engine/app.CollectionService": NewCollectionService(a),
		// v0.9.11: Connection Profiles (P2 §18).
		"github.com/Parsaetak/FreeIran/engine/app.ProfileService": NewProfileService(a),
		// v0.10.2: personal configuration import.
		"github.com/Parsaetak/FreeIran/engine/app.ImportService": NewImportService(a),
	}
}

var (
	// prefixPattern captures the service prefix constant:
	//   const $prefix = "github.com/….<Service>.";
	prefixPattern = regexp.MustCompile(`const \$prefix = "([^"]+)";`)

	// callPattern captures one bound method invocation:
	//   $Call.ByName($prefix + "Method"
	callPattern = regexp.MustCompile(`\$Call\.ByName\(\$prefix \+ "([A-Za-z0-9_]+)"`)

	// byIDPattern captures machine-generated invocations:
	//   $Call.ByID(2904626025)
	byIDPattern = regexp.MustCompile(`\$Call\.ByID\(([0-9]+)\)`)
)

// TestFrontendBindingsMatchGoServices walks the committed binding
// files, extracts every Call.ByName target and asserts the Go service
// exposes an exported method with exactly that name. Machine-generated
// ByID files are verified structurally.
func TestFrontendBindingsMatchGoServices(t *testing.T) {
	root := repoRoot(t)

	bindingsDir := filepath.Join(root,
		"frontend", "bindings", "github.com", "Parsaetak", "FreeIran", "engine", "app")

	entries, err := os.ReadDir(bindingsDir)
	if err != nil {
		t.Fatalf("read bindings dir: %v", err)
	}

	services := registeredServices(t)

	// shortNames maps "TunnelService" → instance for the structural
	// check of machine-generated files (<lowercase-service>service.js).
	shortNames := make(map[string]any, len(services))

	for prefix, service := range services {
		short := prefix[strings.LastIndex(prefix, ".")+1:]
		shortNames[short] = service
	}

	checked := 0

	byIDCalls := 0

	jsFiles := 0

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}

		jsFiles++

		raw, err := os.ReadFile(filepath.Join(bindingsDir, entry.Name()))
		if err != nil {
			t.Fatalf("read binding file %s: %v", entry.Name(), err)
		}

		content := string(raw)

		byIDCalls += len(byIDPattern.FindAllStringSubmatch(content, -1))

		// Structural check: <service>service.js must map to a
		// registered service type (applies to both generations).
		// File names flatten CamelCase (testqueueservice.js →
		// TestQueueService), so the match is case-insensitive.
		if short, ok := serviceFileTypeName(entry.Name()); ok {
			found := false

			for registered := range shortNames {
				if strings.EqualFold(registered, short) {
					found = true

					break
				}
			}

			if !found {
				t.Errorf("%s: binding has no matching Go service registered in cmd/freeiran/main.go",
					entry.Name())
			}
		}

		prefixes := prefixPattern.FindAllStringSubmatch(content, -1)
		if len(prefixes) == 0 {
			continue // generated/model modules without ByName calls
		}

		calls := callPattern.FindAllStringSubmatch(content, -1)
		if len(calls) == 0 {
			continue
		}

		prefix := strings.TrimSuffix(prefixes[0][1], ".") // constant carries the trailing dot

		service, ok := services[prefix]
		if !ok {
			t.Errorf("%s binds unknown service %q (service not registered in cmd/freeiran/main.go?)",
				entry.Name(), prefix)

			continue
		}

		methods := exportedMethods(reflect.TypeOf(service))

		for _, call := range calls {
			checked++

			if _, ok := methods[call[1]]; !ok {
				t.Errorf("%s: binding calls %s.%s but the Go service has no such exported method",
					entry.Name(), prefix, call[1])
			}
		}
	}

	if jsFiles == 0 {
		t.Fatal("no binding files found — repo layout changed")
	}

	if checked == 0 && byIDCalls == 0 {
		t.Fatal("no Call.ByName or Call.ByID invocations found — the binding format changed; update this test")
	}

	t.Logf("verified %d ByName method calls and %d ByID calls across %d binding files",
		checked, byIDCalls, jsFiles)
}

// serviceFileTypeName maps "tunnelservice.js" → "TunnelService"
// (ok=false for models.js / index.js).
func serviceFileTypeName(fileName string) (string, bool) {
	if !strings.HasSuffix(fileName, "service.js") {
		return "", false
	}

	base := strings.TrimSuffix(fileName, "service.js")
	if base == "" {
		return "", false
	}

	return strings.ToUpper(base[:1]) + base[1:] + "Service", true
}

// exportedMethods maps exported method names of a type.
func exportedMethods(t reflect.Type) map[string]reflect.Method {
	out := make(map[string]reflect.Method)

	for i := 0; i < t.NumMethod(); i++ {
		m := t.Method(i)
		if m.PkgPath == "" { // exported
			out[m.Name] = m
		}
	}

	return out
}

// repoRoot walks up from the test's working directory (the package
// dir) until the VERSION file appears.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "VERSION")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root not found (VERSION file missing)")
		}

		dir = parent
	}
}

// ---- v0.9.12: field-exact contract for hand-maintained models ------

// jsPropertyPattern captures one JSDoc @property declaration. Both
// optional-marker spellings occur in the wild and are accepted:
//
//	@property {type} name       (required)
//	@property {type} name=      (optional, Google style)
//	@property {type=} name      (optional, Closure style)
var jsPropertyPattern = regexp.MustCompile(`@property \{([^}]+)\} ([a-zA-Z0-9_]+)(=)?`)

// jsPropertyOptional reports whether one @property match marks the
// field optional (the "=" may live in the type braces or trail the
// name).
func jsPropertyOptional(typeExpr, trailing string) bool {
	return strings.HasSuffix(typeExpr, "=") || trailing == "="
}

// goStructJSONFields reflects the JSON contract of one Go struct:
// field name → optional (omitempty tag or pointer type).
func goStructJSONFields(t reflect.Type) map[string]bool {
	fields := make(map[string]bool)

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)

		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue // not part of the JSON contract
		}

		name := strings.Split(tag, ",")[0]

		optional := strings.Contains(tag, ",omitempty") || f.Type.Kind() == reflect.Pointer

		fields[name] = optional
	}

	return fields
}

// jsDocProperties parses one @typedef block from the model file and
// returns field name → optional.
func jsDocProperties(content, typedefName string) (map[string]bool, bool) {
	anchor := "@typedef {Object} " + typedefName

	idx := strings.Index(content, anchor)
	if idx < 0 {
		return nil, false
	}

	// The typedef's properties run from the anchor to the next
	// @typedef (or end of file).
	rest := content[idx:]

	if next := strings.Index(rest[len(anchor):], "@typedef"); next >= 0 {
		rest = rest[:len(anchor)+next]
	}

	out := make(map[string]bool)

	for _, m := range jsPropertyPattern.FindAllStringSubmatch(rest, -1) {
		// m[1] = the type expression, m[2] = the field name,
		// m[3] = a trailing "=" optional marker (may be empty).
		out[m[2]] = jsPropertyOptional(m[1], m[3])
	}

	return out, true
}

// TestProfileBindingModelsMatchGoStructs pins the MACHINE-GENERATED
// binding models (frontend/bindings/.../engine/app/models.js) to the
// Go structs field-for-field: same JSON names, same optionality.
//
// History: v0.9.11 hand-maintained profiletypes.js as a verified
// mirror because the wails3 generator could not run on that host;
// v0.11.0 regenerated ALL bindings with the pinned wails3
// v3.0.0-beta.19 CLI (reproducibility verified: a second generation
// is byte-identical), which removed the last hand-maintained binding
// files. The field-for-field check is KEPT — a stale committed
// models.js (service signatures changed in Go but bindings not
// regenerated) must still fail loudly instead of silently drifting.
func TestProfileBindingModelsMatchGoStructs(t *testing.T) {
	root := repoRoot(t)

	raw, err := os.ReadFile(filepath.Join(root,
		"frontend", "bindings", "github.com", "Parsaetak", "FreeIran", "engine", "app", "models.js"))
	if err != nil {
		t.Fatalf("read models.js: %v", err)
	}

	content := string(raw)

	goModels := map[string]reflect.Type{
		"ProfileView": reflect.TypeOf(ProfileView{}),
		"ProfileSpec": reflect.TypeOf(ProfileSpec{}),
	}

	for className, goType := range goModels {
		jsFields, ok := generatedModelClassFields(content, className)
		if !ok {
			t.Errorf("models.js: export class %s not found", className)

			continue
		}

		goFields := goStructJSONFields(goType)

		for name, goOptional := range goFields {
			jsOptional, present := jsFields[name]
			if !present {
				t.Errorf("%s: Go field %q (json %q) is missing from the generated JS model", className, name, name)

				continue
			}

			if goOptional != jsOptional {
				t.Errorf("%s: field %q optionality mismatch: Go optional=%v, generated JS optional=%v",
					className, name, goOptional, jsOptional)
			}
		}

		for name := range jsFields {
			if _, ok := goFields[name]; !ok {
				t.Errorf("%s: generated JS model declares field %q that does not exist on the Go struct", className, name)
			}
		}
	}
}

// generatedModelClassFields parses one generated models.js class
// body into field name → optional. The generator emits REQUIRED
// fields as `if (!("name" in $$source)) { ... this["name"] = <default>; }`
// and OPTIONAL (omitempty / pointer) fields as
// `if (false)) { ... this["name"] = undefined; }`.
func generatedModelClassFields(content, className string) (map[string]bool, bool) {
	anchor := "export class " + className + " {"

	idx := strings.Index(content, anchor)
	if idx < 0 {
		return nil, false
	}

	rest := content[idx:]

	if next := strings.Index(rest[len(anchor):], "\nexport class "); next >= 0 {
		rest = rest[:len(anchor)+next]
	}

	// Line-based field extraction. The generator writes REQUIRED
	// fields as `this["name"] = <default>;` (string/number/bool
	// default) and OPTIONAL fields as `this["name"] = undefined;`
	// inside a statically-dead `if (false)` branch — the value
	// assigned IS the optionality marker.
	out := make(map[string]bool)

	assignPattern := regexp.MustCompile(`this\["([a-zA-Z0-9_]+)"\] = `)

	for _, line := range strings.Split(rest, "\n") {
		m := assignPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		out[m[1]] = strings.Contains(line, "= undefined;")
	}

	return out, len(out) > 0
}
