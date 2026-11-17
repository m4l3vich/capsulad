GO ?= go
CARGO ?= cargo
CXX ?= c++

# The C++ example links libdl on Linux only.
ifeq ($(shell uname -s 2>/dev/null),Darwin)
CXX_DL :=
else
CXX_DL := -ldl
endif

# Build profile. `make` and `make release` strip Go binaries and use Cargo's
# release profile; `make debug` keeps symbols and uses Cargo's dev profile.
PROFILE ?= release
ifeq ($(filter $(PROFILE),release debug),)
$(error PROFILE must be release or debug)
endif

# The Go target decides linking, not the build host: a darwin target needs the
# external linker for LC_UUID, while Linux cross-builds keep the internal one.
GOOS ?= $(shell $(GO) env GOOS 2>/dev/null)
GOARCH ?= $(shell $(GO) env GOARCH 2>/dev/null)
HOST_GOOS := $(shell $(GO) env GOHOSTOS 2>/dev/null)
HOST_GOARCH := $(shell $(GO) env GOHOSTARCH 2>/dev/null)
NATIVE_CROSS := $(if $(filter-out $(HOST_GOOS)/$(HOST_GOARCH),$(GOOS)/$(GOARCH)),1,)
ifeq ($(GOOS),darwin)
GO_LINKMODE := -linkmode=external
else
GO_LINKMODE :=
endif
HOST_GO_LINKMODE := $(if $(filter darwin,$(HOST_GOOS)),-linkmode=external,)

ifeq ($(PROFILE),release)
GO_BUILD_FLAGS := -trimpath
GO_LDFLAGS := -s -w $(GO_LINKMODE)
NATIVE_CARGO_MODE := --release
NATIVE_PROFILE_DIR := release
else
GO_BUILD_FLAGS :=
GO_LDFLAGS := $(GO_LINKMODE)
NATIVE_CARGO_MODE :=
NATIVE_PROFILE_DIR := debug
endif

GO_ENV := GOOS=$(GOOS) GOARCH=$(GOARCH)
CGO_GO_ENV := CGO_ENABLED=1 $(GO_ENV)
GOBUILD := $(GO_ENV) $(GO) build $(GO_BUILD_FLAGS) -ldflags '$(GO_LDFLAGS)'

BINDIR ?= build/bin
HOST_CLI ?= $(BINDIR)/capsulad-cli-host
LIBDIR ?= build/lib
INCLUDEDIR ?= build/include
APPDIR ?= build/apps
DEVAPPSDIR ?= build/devapps
DEPLOYDIR ?= build/deploy-bundle
DEPLOY_TARBALL ?= $(DEPLOYDIR).tar.gz
# Include the development apps staged in $(DEVAPPSDIR) in the bundle. They are
# never built here: `make devapps` produces their artifacts first.
BUNDLE_DEVAPPS ?= 0

# App artifacts are packaged with a CLI that runs on the build host. A
# cross-target CLI cannot execute there, so cross builds package with a
# host-built copy. `make deploy-bundle` never runs a packager.
ifeq ($(NATIVE_CROSS),1)
PACKAGER := $(HOST_CLI)
PACKAGER_TARGET := capsulad-cli-host
else
PACKAGER := $(BINDIR)/capsulad-cli
PACKAGER_TARGET := capsulad-cli
endif

NATIVE_DIR ?= sdk/native
NATIVE_MANIFEST ?= $(NATIVE_DIR)/Cargo.toml
NATIVE_HEADER ?= $(NATIVE_DIR)/include/capsulad/capsulad.h

# An explicit Rust target is required when GOOS/GOARCH is not the host, because
# Cargo would otherwise silently build the host library. The Capsula Mini is
# aarch64 glibc 2.33, so its target is aarch64-unknown-linux-gnu, not musl.
NATIVE_TARGET ?= $(CARGO_BUILD_TARGET)
NATIVE_OUTDIR := $(NATIVE_DIR)/target$(if $(NATIVE_TARGET),/$(NATIVE_TARGET))/$(NATIVE_PROFILE_DIR)
NATIVE_CARGO_TARGET = $(if $(NATIVE_TARGET),--target $(NATIVE_TARGET),)

# Stable ABI metadata is emitted by NativeCore's authoritative script so the
# capability vocabulary cannot drift from the C ABI contract.
NATIVE_METADATA_SCRIPT ?= $(NATIVE_DIR)/emit-runtime-metadata.sh

ifeq ($(GOOS),darwin)
NATIVE_SOURCE := libcapsulad.dylib
NATIVE_LIBRARY := libcapsulad.1.dylib
NATIVE_METADATA_LIBRARY := libcapsulad.1.dylib
else
NATIVE_SOURCE := libcapsulad.so
NATIVE_LIBRARY := libcapsulad.so.1.0.0
NATIVE_METADATA_LIBRARY := libcapsulad.so.1
endif

.PHONY: all release debug all-targets daemon daemon-release daemon-debug capsulad capsulad-cli capsulad-cli-host deploy-bundle devapps web sdk sdk-go sdk-rust native interpreter interpreter-quickjs interpreter-micropython dumbassistant marusia simon pa radio radio-tuner homeassistant product-apps test test-go test-race test-sdk check-sdk-abi test-dumbassistant test-marusia test-apps test-simon test-pa test-radio test-radio-tuner test-homeassistant test-interpreter clean

all: release

release:
	$(MAKE) PROFILE=release all-targets

debug:
	$(MAKE) PROFILE=debug all-targets

all-targets: capsulad capsulad-cli native sdk

# Go-only build, including cross targets without a native SDK or cgo.
daemon: capsulad capsulad-cli

daemon-release:
	$(MAKE) PROFILE=release daemon

daemon-debug:
	$(MAKE) PROFILE=debug daemon

capsulad:
	mkdir -p $(BINDIR)
	rm -f $(BINDIR)/capsulad
	$(GOBUILD) -o $(BINDIR)/capsulad ./cmd/capsulad

capsulad-cli:
	mkdir -p $(BINDIR)
	rm -f $(BINDIR)/capsulad-cli
	$(GOBUILD) -o $(BINDIR)/capsulad-cli ./cmd/capsulad-cli

# Host-executable CLI used to package app artifacts when GOOS/GOARCH is not the
# build host. It is a build-time tool only: it never enters a deployment bundle.
capsulad-cli-host:
	mkdir -p $(BINDIR)
	rm -f $(HOST_CLI)
	GOOS=$(HOST_GOOS) GOARCH=$(HOST_GOARCH) $(GO) build $(GO_BUILD_FLAGS) -ldflags '$(HOST_GO_LINKMODE)' -o $(HOST_CLI) ./cmd/capsulad-cli

# One directory that `capsulad-cli deploy` accepts as-is. Layout:
# capsulad, capsulad-cli, lib/ (native runtime incl. headers), apps/*.tar.gz,
# boot/S99btctl.sh. App artifacts are produced and packaged by the app build
# targets (`make dumbassistant`, `make devapps`); assembly only copies the
# ready-made .tar.gz files, so nothing is ever packaged for or on the device.
# Native runtime symlinks are rebuilt from capsulad-native.json during deploy;
# the bundle carries only the regular runtime files so its tarball is extractable.
deploy-bundle: capsulad capsulad-cli native
	rm -rf $(DEPLOYDIR) $(DEPLOY_TARBALL)
	mkdir -p $(DEPLOYDIR)/lib/include $(DEPLOYDIR)/apps $(DEPLOYDIR)/boot
	cp $(BINDIR)/capsulad $(DEPLOYDIR)/capsulad
	cp $(BINDIR)/capsulad-cli $(DEPLOYDIR)/capsulad-cli
	@for f in $(LIBDIR)/*; do \
		if [ -f "$$f" ] && [ ! -L "$$f" ]; then cp "$$f" $(DEPLOYDIR)/lib/; fi; \
	done
	cp -R $(INCLUDEDIR)/capsulad $(DEPLOYDIR)/lib/include/
	cp deploy/rockchip-stock/S99btctl.sh $(DEPLOYDIR)/boot/S99btctl.sh
	@set -e; \
	bundle_app() { \
		id=$$1; dir=$$2; target=$$3; copied=; \
		for f in "$$dir"/"$$id"-*.tar.gz; do \
			[ -f "$$f" ] || continue; \
			cp "$$f" $(DEPLOYDIR)/apps/; \
			echo "bundled $$f"; \
			copied=1; \
		done; \
		if [ -z "$$copied" ]; then \
			echo "error: app $$id is built in $$dir but has no .tar.gz artifact." >&2; \
			echo "       run 'make $$target' to compile and package it, then rerun make deploy-bundle." >&2; \
			exit 1; \
		fi; \
	}; \
	for d in $(APPDIR)/*/; do \
		[ -f "$$d/manifest.toml" ] || continue; \
		id=$$(basename "$$d"); \
		target=$${id#org.capsulad.}; \
		if [ "$$id" = "org.capsulad.radio.tuner" ]; then target=radio-tuner; fi; \
		bundle_app "$$id" "$(APPDIR)" "$$target"; \
	done; \
	if [ "$(BUNDLE_DEVAPPS)" = "1" ]; then \
		staged=; \
		for d in $(DEVAPPSDIR)/*/; do \
			[ -f "$$d/manifest.toml" ] || continue; \
			staged=1; \
			bundle_app "$$(basename "$$d")" "$(DEVAPPSDIR)" devapps; \
		done; \
		if [ -z "$$staged" ]; then \
			echo "error: BUNDLE_DEVAPPS=1 but $(DEVAPPSDIR) has no staged dev apps." >&2; \
			echo "       run 'make devapps' first, then rerun make deploy-bundle." >&2; \
			exit 1; \
		fi; \
	fi
	COPYFILE_DISABLE=1 tar -C $(DEPLOYDIR) -czf $(DEPLOY_TARBALL) .
	@echo "deployment bundle: $(DEPLOYDIR) and $(DEPLOY_TARBALL)"

# Optional first-party app packages, built by the app workspace tooling. The
# script is authoritative for which apps exist and how they are packaged.
devapps:
	sh apps/devtools/build.sh

# Independently built browser SPA. Output is embedded by internal/webui, so
# run this before rebuilding capsulad when frontend sources change. The built
# dist is checked in, so ordinary Go builds do not need Node.
web:
	cd web && npm ci && npm run build

# The Go SDK is its own module; it exposes the thin binding over libcapsulad.
# It uses cgo, so a cross build also needs a matching C toolchain.
sdk: sdk-go sdk-rust

sdk-go:
	cd sdk/go && $(CGO_GO_ENV) $(GO) build ./...

# The Rust binding loads the library dynamically; it does not link the engine.
sdk-rust:
	$(CARGO) build --locked --manifest-path sdk/rust/Cargo.toml $(NATIVE_CARGO_MODE) $(NATIVE_CARGO_TARGET)

native:
	@test -f $(NATIVE_MANIFEST) || { echo "error: missing $(NATIVE_MANIFEST)" >&2; exit 1; }
	@test -f $(NATIVE_HEADER) || { echo "error: missing $(NATIVE_HEADER)" >&2; exit 1; }
	@test -f $(NATIVE_METADATA_SCRIPT) || { echo "error: missing $(NATIVE_METADATA_SCRIPT)" >&2; exit 1; }
	@if [ "$(NATIVE_CROSS)" = "1" ] && [ -z "$(NATIVE_TARGET)" ]; then \
		echo "error: refusing a host native SDK build for target $(GOOS)/$(GOARCH)." >&2; \
		echo "       Set NATIVE_TARGET to a Rust triple (for example aarch64-unknown-linux-gnu)," >&2; \
		echo "       install that target and linker, or run make daemon for a Go-only build." >&2; \
		exit 1; \
	fi
	@case "$(GOOS):$(NATIVE_TARGET)" in \
		darwin:*linux*|linux:*darwin*) \
			echo "error: GOOS=$(GOOS) does not match NATIVE_TARGET=$(NATIVE_TARGET)" >&2; exit 1;; \
	esac
	mkdir -p $(LIBDIR) $(INCLUDEDIR)/capsulad
	$(CARGO) build --locked --manifest-path $(NATIVE_MANIFEST) $(NATIVE_CARGO_MODE) $(NATIVE_CARGO_TARGET)
	@test -f $(NATIVE_OUTDIR)/$(NATIVE_SOURCE) || { \
		echo "error: Cargo did not produce $(NATIVE_OUTDIR)/$(NATIVE_SOURCE)." >&2; \
		echo "       Install the Rust target and linker, or set NATIVE_TARGET for the intended ABI." >&2; \
		exit 1; \
	}
	@# Clean the previous staging only after the build succeeded: a failed
	@# cross build must not remove the installed host runtime.
	rm -f $(LIBDIR)/libcapsulad.so $(LIBDIR)/libcapsulad.so.1 $(LIBDIR)/libcapsulad.so.1.0.0 $(LIBDIR)/libcapsulad.dylib $(LIBDIR)/libcapsulad.1.dylib $(LIBDIR)/capsulad-native.json $(INCLUDEDIR)/capsulad.h $(INCLUDEDIR)/capsulad/capsulad.h
ifeq ($(GOOS),darwin)
	cp $(NATIVE_OUTDIR)/$(NATIVE_SOURCE) $(LIBDIR)/$(NATIVE_LIBRARY)
	ln -sfn $(NATIVE_LIBRARY) $(LIBDIR)/libcapsulad.dylib
else
	cp $(NATIVE_OUTDIR)/$(NATIVE_SOURCE) $(LIBDIR)/$(NATIVE_LIBRARY)
	ln -sfn $(NATIVE_LIBRARY) $(LIBDIR)/libcapsulad.so.1
	ln -sfn libcapsulad.so.1 $(LIBDIR)/libcapsulad.so
endif
	cp $(NATIVE_HEADER) $(INCLUDEDIR)/capsulad/capsulad.h
	sh $(NATIVE_METADATA_SCRIPT) $(LIBDIR)/capsulad-native.json $(NATIVE_METADATA_LIBRARY)

# Script runtimes. Optional because they build third-party engines from
# pinned sources; stage the hosts beside libcapsulad so the launcher can find
# them. QUICKJS_SRC/MICROPYTHON_SRC may point at already extracted pinned trees.
interpreter:
	sh interpreter/build.sh

interpreter-quickjs:
	TARGETS=quickjs sh interpreter/build.sh

interpreter-micropython:
	TARGETS=micropython sh interpreter/build.sh

# Optional first-party app. It is a separate nested module and is never part
# of `all`, `release` or `debug`. The binary is compiled and packaged on the
# build host; the staged package directory and its <id>-<version>.tar.gz land
# side by side in $(APPDIR), and that archive is what a bundle carries.
dumbassistant: $(PACKAGER_TARGET)
	rm -rf $(APPDIR)/org.capsulad.dumbassistant
	mkdir -p $(APPDIR)/org.capsulad.dumbassistant/assets
	cd apps/dumbassistant && CGO_ENABLED=1 $(GOBUILD) -o $(CURDIR)/$(APPDIR)/org.capsulad.dumbassistant/dumbassistant ./cmd/dumbassistant
	cp apps/dumbassistant/manifest.toml $(APPDIR)/org.capsulad.dumbassistant/manifest.toml
	cp -R apps/dumbassistant/assets/. $(APPDIR)/org.capsulad.dumbassistant/assets/
	@rm -f $(APPDIR)/org.capsulad.dumbassistant-*.tar.gz
	cd $(APPDIR) && $(abspath $(PACKAGER)) app package org.capsulad.dumbassistant

# Marusia is the second optional first-party app. It has no packaged assets:
# the reply is a TTS URL fetched at turn time. The staged package directory and
# its <id>-<version>.tar.gz land side by side in $(APPDIR).
marusia: $(PACKAGER_TARGET)
	rm -rf $(APPDIR)/org.capsulad.marusia
	mkdir -p $(APPDIR)/org.capsulad.marusia
	cd apps/marusia && CGO_ENABLED=1 $(GOBUILD) -o $(CURDIR)/$(APPDIR)/org.capsulad.marusia/marusia ./cmd/marusia
	cp apps/marusia/manifest.toml $(APPDIR)/org.capsulad.marusia/manifest.toml
	@rm -f $(APPDIR)/org.capsulad.marusia-*.tar.gz
	cd $(APPDIR) && $(abspath $(PACKAGER)) app package org.capsulad.marusia

# Simon Says is a QuickJS script app: the package is its manifest plus main.js,
# with the contract test suite deliberately left out. Same conventions as the
# assistants (staged package directory and <id>-<version>.tar.gz side by side
# under $(APPDIR)); the script is authoritative for OUT/PACKAGE=0/CAPSULAD_CLI.
simon: $(PACKAGER_TARGET)
	OUT=$(abspath $(APPDIR)) CAPSULAD_CLI=$(abspath $(PACKAGER)) sh apps/simon/build.sh

# The PA Speaker MicroPython service: nothing is compiled, so its own script
# stages the manifest, the script, the descriptor and the interface
# documentation and packages them. It needs no native runtime, only the CLI.
pa: $(PACKAGER_TARGET)
	OUT=$(abspath $(APPDIR)) CAPSULAD_CLI=$(abspath $(PACKAGER)) sh apps/pa/build.sh

# The Internet Radio front end. The native tuner companion is its own package
# (`make radio-tuner`), because a script host cannot fetch or decode a stream.
# The app's own Makefile owns its package contents, including the raw-protobuf
# contract; the repository target only supplies the output directory and the
# build-host packager.
radio: $(PACKAGER_TARGET)
	$(MAKE) -C apps/radio package OUT=$(abspath $(APPDIR))/org.capsulad.radio CLI=$(abspath $(PACKAGER))

# The native tuner compiles and packages itself, and needs the host native
# runtime (`make native`) to link the shared SDK. A device package is the same
# script with GOOS/GOARCH/CC set for aarch64.
radio-tuner:
	OUT=$(abspath $(APPDIR)) sh apps/radio/tuner/build.sh

# The Home Assistant bridge is a native app: the build script compiles the Go
# binary and packages it with the manifest, its descriptor and its bridge
# documentation beside it. It needs the host native runtime (`make native`).
homeassistant:
	OUT=$(abspath $(APPDIR)) sh apps/homeassistant/build.sh

# All five product app packages in one step. `make native` first for the two
# native ones; each target is also usable on its own.
product-apps: simon pa radio radio-tuner homeassistant

# `make test` runs every Go module that ships in this repository: the root Go
# module, the nested app modules (both assistants, the radio tuner companion and
# the Home Assistant bridge), the Rust binding and the native crate (the frozen
# C ABI), plus a compile of the public C++ example. `make test-race` adds the
# race detector to the root module. The script-app suites need Node or CPython,
# so they stay out of it: run them with `make test-apps` (`test-simon`,
# `test-pa`, `test-radio`).
test: test-go test-sdk test-dumbassistant test-marusia test-radio-tuner test-homeassistant

test-go:
	$(GO) test -ldflags "$(GO_LINKMODE)" ./...

test-race:
	$(GO) test -race -ldflags "$(GO_LINKMODE)" ./...

# The native crate is the only test suite that exercises the real ABI; the
# C++ example proves the shipped header still compiles.
test-sdk: check-sdk-abi
	cd sdk/go && $(CGO_GO_ENV) $(GO) test ./...
	$(CARGO) test --locked --manifest-path sdk/rust/Cargo.toml $(NATIVE_CARGO_MODE) $(NATIVE_CARGO_TARGET)
	$(CARGO) test --locked --manifest-path $(NATIVE_MANIFEST) $(NATIVE_CARGO_MODE) $(NATIVE_CARGO_TARGET)
	mkdir -p $(BINDIR)
	$(CXX) -std=c++17 -pthread -I $(NATIVE_DIR)/include sdk/cpp/example.cpp -o $(BINDIR)/capsulad-cpp-example $(CXX_DL)

check-sdk-abi:
	sh sdk/go/regenerate-abi.sh --check

test-dumbassistant:
	cd apps/dumbassistant && $(CGO_GO_ENV) $(GO) test ./...

# Tests run on the build host, so the host link mode applies: a darwin host
# needs the external linker for LC_UUID. No test reaches the network; the
# protocol tests use in-process TLS listeners.
test-marusia:
	cd apps/marusia && $(CGO_GO_ENV) $(GO) test -ldflags "$(HOST_GO_LINKMODE)" ./...

# The MicroPython host's prelude is pure Python, so its self-test (timer
# registration, cancellation, clock and poll behaviour) runs under CPython
# without building a host.
test-interpreter:
	python3 interpreter/micropython/sdk_test.py

# Host-side suites. Simon's contract tests load the shipped main.js into a fake
# QuickJS host under Node; the Internet Radio unit tests import the shipped
# main.py with a scripted `capsulad` module under CPython; the tuner and bridge
# companions are nested Go modules; and the MicroPython host's pure-Python
# prelude has its own self-test (timers included).
test-apps: test-simon test-radio test-radio-tuner test-homeassistant test-pa test-interpreter

test-simon:
	node --test apps/simon/test/*.test.mjs

test-pa:
	python3 apps/pa/tests/test_wav.py
	python3 apps/pa/tests/test_app.py

test-radio:
	$(MAKE) -C apps/radio test

test-radio-tuner:
	cd apps/radio/tuner && $(CGO_GO_ENV) $(GO) test ./...

test-homeassistant:
	cd apps/homeassistant && $(CGO_GO_ENV) $(GO) test -ldflags "$(HOST_GO_LINKMODE)" ./...

# Remove generated binaries and packages but leave build/demo (operator data)
# untouched.
clean:
	rm -rf $(BINDIR) $(LIBDIR) $(INCLUDEDIR) $(APPDIR) $(DEVAPPSDIR) $(DEPLOYDIR) $(DEPLOY_TARBALL)
	@if [ -f $(NATIVE_MANIFEST) ]; then $(CARGO) clean --manifest-path $(NATIVE_MANIFEST); fi
