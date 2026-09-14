# FreeIran root Makefile — thin wrappers over the exact commands CI
# (.github/workflows/ci.yml) and release.yml run. Nothing here hides
# steps: every target maps 1:1 to a CI gate so a green `make check`
# locally predicts a green CI run.
#
# Requirements: Go >= 1.26.8 (see go.mod toolchain), Node 22, GNU make.

GO ?= go
NPM ?= npm
CORES_DIR ?= $(CURDIR)/.test-cores

VERSION := $(shell tr -d '[:space:]' < VERSION)
COMMIT  := $(shell git rev-parse --short=8 HEAD 2>/dev/null || echo dev)

.PHONY: help check go-fmt go-vet go-build go-test go-race fake-cores \
        native native-test frontend frontend-ci desktop-windows desktop-linux \
        smoke-test clean

help:
	@echo "FreeIran $(VERSION) — make targets (CI parity):"
	@echo "  make check            go-fmt + go-vet + go-build + go-test + go-race + native-test + frontend-ci"
	@echo "  make fake-cores       build the deterministic fake protocol-core fixtures"
	@echo "  make go-fmt / go-vet / go-build / go-test / go-race"
	@echo "  make native-test / native"
	@echo "  make frontend-ci      npm ci + typecheck + test + build"
	@echo "  make desktop-windows  windows/amd64 GUI-subsystem build (no console window)"
	@echo "  make desktop-linux    linux/amd64 gtk3 build (needs libgtk-3-dev libwebkit2gtk-4.1-dev)"
	@echo "  make smoke-test       headless engine boot/shutdown verification"

check: go-fmt go-vet go-build go-test go-race native-test frontend-ci

fake-cores:
	mkdir -p "$(CORES_DIR)"
	$(GO) build -o "$(CORES_DIR)/fakecore" ./engine/core/testdata/fakecore
	cp "$(CORES_DIR)/fakecore" "$(CORES_DIR)/fake-xray"
	cp "$(CORES_DIR)/fakecore" "$(CORES_DIR)/fake-v2ray"
	cp "$(CORES_DIR)/fakecore" "$(CORES_DIR)/fake-sing-box"

export FREEIRAN_TEST_CORES := $(CORES_DIR)
export FREEIRAN_SKIP_MIGRATION := 1

go-fmt:
	@unformatted=$$($(GO) fmt ./engine/... ./system/... ./cmd/... ./internal/... 2>/dev/null); \
	if [ -n "$$unformatted" ]; then echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; fi

go-vet:
	$(GO) vet ./engine/... ./system/... ./internal/...

go-build:
	$(GO) build ./engine/... ./system/... ./internal/...

go-test: fake-cores
	$(GO) test -count=1 ./engine/... ./system/... ./internal/...

go-race: fake-cores
	$(GO) test -race -count=1 ./engine/... ./system/... ./internal/...

native-test:
	make -C native test

native:
	make -C native
	$(GO) build -tags native_accel ./engine/native

frontend-ci:
	cd frontend && $(NPM) ci && $(NPM) run typecheck && $(NPM) test && $(NPM) run build:embed

desktop-windows: fake-cores
	cd frontend && $(NPM) run build:embed
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
	$(GO) build -trimpath \
		-ldflags "-s -w -H=windowsgui -X github.com/Parsaetak/FreeIran/internal/version.Version=$(VERSION) -X github.com/Parsaetak/FreeIran/internal/version.Commit=$(COMMIT)" \
		-o FreeIran-windows-amd64.exe ./cmd/freeiran
	FREEIRAN_GUI_EXE=$(CURDIR)/FreeIran-windows-amd64.exe \
		$(GO) test -count=1 -run TestWindowsGUISubsystem ./cmd/freeiran

desktop-linux: fake-cores
	cd frontend && $(NPM) run build:embed
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
	$(GO) build -tags gtk3 -trimpath \
		-ldflags "-s -w -X github.com/Parsaetak/FreeIran/internal/version.Version=$(VERSION) -X github.com/Parsaetak/FreeIran/internal/version.Commit=$(COMMIT)" \
		-o FreeIran-linux-amd64 ./cmd/freeiran

smoke-test: desktop-linux
	./FreeIran-linux-amd64 --smoke-test

clean:
	rm -rf "$(CORES_DIR)" FreeIran-windows-amd64.exe FreeIran-linux-amd64 \
		FreeIran-linux-amd64 FreeIran-v*-windows-amd64 FreeIran-v*-linux-amd64
	make -C native clean
