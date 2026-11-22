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

# Build tags for capsulad-cli only. grpcnotrace drops gRPC's optional
# golang.org/x/net/trace hooks (and the text/template stack only they reach);
# the CLI never registers those handlers and gRPC tracing is off by default, so
# the wire behavior is unchanged. The daemon and app builds keep the default
# tag set. This is the CLI's measured release saving, not a semantics change.
CLI_TAGS ?= grpcnotrace

BINDIR ?= build/bin
HOST_CLI ?= $(BINDIR)/capsulad-cli-host
LIBDIR ?= build/lib
INCLUDEDIR ?= build/include
APPDIR ?= build/apps
DEVAPPSDIR ?= build/devapps
DEPLOYDIR ?= build/deploy-bundle
DEPLOY_TARBALL ?= $(DEPLOYDIR).tar.gz
# Include the CLI in the deployment bundle. The default keeps the documented
# adb flow, which runs `capsulad-cli` from the pushed bundle. BUNDLE_CLI=0
# assembles a CLI-free device bundle: the CLI is not a prerequisite and is not
# copied, while a separately installed <data-dir>/capsulad-cli is left as it is.
BUNDLE_CLI ?= 1
# One predicate decides both the prerequisite and the copy. Reject anything
# else instead of building a CLI the bundle then omits.
ifneq ($(words $(BUNDLE_CLI)),1)
$(error BUNDLE_CLI must be 0 or 1, got '$(BUNDLE_CLI)')
endif
ifneq ($(filter 0 1,$(BUNDLE_CLI)),$(BUNDLE_CLI))
$(error BUNDLE_CLI must be 0 or 1, got '$(BUNDLE_CLI)')
endif
CLI_ENABLED := $(if $(filter 1,$(BUNDLE_CLI)),1,0)
CLI_PREREQ := $(if $(filter 1,$(CLI_ENABLED)),capsulad-cli,)
# Include the development apps staged in $(DEVAPPSDIR) in the bundle. They are
# never built here: `make devapps` produces their artifacts first.
BUNDLE_DEVAPPS ?= 0
# Native runtime in the bundle. The normal target carries it; BUNDLE_NATIVE=0
# assembles a bundle without it (static Go and script apps only), and native app
# packages are then rejected at install by the profile's ABI gate.
BUNDLE_NATIVE ?= 1
ifeq ($(BUNDLE_NATIVE),0)
NATIVE_PREREQ :=
else
NATIVE_PREREQ := native
endif

# Deployment target assembled into the bundle. The bundle layout is identical
# on every target; only the boot hook file differs. `capsulad-cli deploy
# -platform <target>` must be given the matching value.
DEPLOY_PLATFORM ?= rockchip-stock
ifeq ($(DEPLOY_PLATFORM),amlogic-stock)
BOOT_HOOK_SRC := deploy/amlogic-stock/S99sysmon.sh
else ifeq ($(DEPLOY_PLATFORM),amlogic-alpine)
BOOT_HOOK_SRC := deploy/amlogic-alpine/capsulad
else
BOOT_HOOK_SRC := deploy/rockchip-stock/S99btctl.sh
endif
BOOT_HOOK_NAME := $(notdir $(BOOT_HOOK_SRC))

# ABI every app package is built for. App build scripts stage this value into
# their manifest; `capsulad-cli app package` never rewrites it. It exists only
# for a Linux target: a host (darwin) package is not labelled or ELF-gated as a
# Linux artifact, preserving the existing development behaviour.
ifeq ($(GOOS),linux)
ifeq ($(DEPLOY_PLATFORM),amlogic-alpine)
TARGET_ABI ?= linux-musl
else
TARGET_ABI ?= linux-gnu
endif
export TARGET_ABI
else
export TARGET_ABI :=
endif
# Highest GLIBC symbol version the target accepts; exported to the app packager
# only for a Linux target (a host package has no glibc ceiling and no ELF gate).
ifeq ($(GOOS),linux)
ifeq ($(DEPLOY_PLATFORM),amlogic-stock)
MAX_GLIBC ?= 2.28
else ifeq ($(DEPLOY_PLATFORM),amlogic-alpine)
MAX_GLIBC ?=
else
MAX_GLIBC ?= 2.33
endif
export CAPSULAD_MAX_GLIBC := $(MAX_GLIBC)
export CAPSULAD_TARGET_ARCH := $(GOARCH)
else
export CAPSULAD_MAX_GLIBC :=
export CAPSULAD_TARGET_ARCH :=
endif

# The Amlogic targets are aarch64; the stock image is glibc and Alpine is musl.
# Default the Rust target from the deploy platform so the assembled bundle
# cannot silently carry a runtime built for the wrong libc. An explicit
# NATIVE_TARGET still wins.
ifeq ($(NATIVE_TARGET),)
ifeq ($(DEPLOY_PLATFORM),amlogic-alpine)
NATIVE_TARGET := aarch64-unknown-linux-musl
else ifeq ($(DEPLOY_PLATFORM),amlogic-stock)
NATIVE_TARGET := aarch64-unknown-linux-gnu
endif
endif
# Derived from the effective target (defaulted or explicit), so every libc gate
# applies to the exact command DEVELOPMENT.md documents.
# findstring, not filter: make treats only the first % as a wildcard, so
# %-linux-gnu% never matched (and the libc gate was silently skipped).
NATIVE_LIBC := $(if $(findstring linux-musl,$(NATIVE_TARGET)),musl,$(if $(findstring linux-gnu,$(NATIVE_TARGET)),gnu,))

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

# Rust musl targets default to crt-static, and a static runtime drops the
# cdylib crate type ("dropping unsupported crate type `cdylib`"). The Alpine
# native runtime must be a dynamically linked musl shared object, so only that
# target gets -crt-static off. glibc and Darwin builds are untouched.
NATIVE_RUSTFLAGS :=
ifneq ($(filter %-linux-musl,$(NATIVE_TARGET)),)
NATIVE_RUSTFLAGS := RUSTFLAGS="-C target-feature=-crt-static"
endif

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

.PHONY: check-target-toolchain all release debug all-targets daemon daemon-release daemon-debug capsulad capsulad-cli capsulad-cli-host deploy-bundle devapps web sdk sdk-go sdk-rust native interpreter interpreter-quickjs interpreter-micropython dumbassistant marusia cadence simon soundboard hardwaretest pa radio homeassistant airplay airplay-target product-apps test test-go test-race test-sdk check-sdk-abi test-dumbassistant test-marusia test-cadence test-apps test-simon test-soundboard test-hardwaretest test-pa test-radio test-homeassistant test-airplay test-interpreter clean

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

check-target-toolchain:
	@sh tools/check-target-cc.sh "$(GOOS)" "$(GOARCH)" "$(HOST_GOOS)" "$(TARGET_ABI)" "$(CC)"

capsulad:
	mkdir -p $(BINDIR)
	rm -f $(BINDIR)/capsulad
	$(GOBUILD) -o $(BINDIR)/capsulad ./cmd/capsulad

capsulad-cli:
	mkdir -p $(BINDIR)
	rm -f $(BINDIR)/capsulad-cli
	$(GOBUILD) -tags=$(CLI_TAGS) -o $(BINDIR)/capsulad-cli ./cmd/capsulad-cli

# Host-executable CLI used to package app artifacts when GOOS/GOARCH is not the
# build host. It is a build-time tool only: it never enters a deployment bundle.
capsulad-cli-host:
	mkdir -p $(BINDIR)
	rm -f $(HOST_CLI)
	GOOS=$(HOST_GOOS) GOARCH=$(HOST_GOARCH) $(GO) build $(GO_BUILD_FLAGS) -tags=$(CLI_TAGS) -ldflags '$(HOST_GO_LINKMODE)' -o $(HOST_CLI) ./cmd/capsulad-cli

# One directory that `capsulad-cli deploy` accepts as-is. Layout:
# capsulad, capsulad-cli (unless BUNDLE_CLI=0), lib/ (native runtime incl.
# headers), apps/*.tar.gz, boot/<hook>. App artifacts are produced and packaged
# by the app build targets (`make dumbassistant`, `make devapps`); assembly only
# copies the ready-made .tar.gz files, so nothing is ever packaged for or on the
# device.
# DEPLOY_PLATFORM selects the boot hook (rockchip-stock, amlogic-stock or
# amlogic-alpine); pass the same value to `capsulad-cli deploy -platform`.
# Native runtime symlinks are rebuilt from capsulad-native.json during deploy;
# the bundle carries only the regular runtime files so its tarball is extractable.
deploy-bundle: capsulad $(CLI_PREREQ) $(NATIVE_PREREQ)
	rm -rf $(DEPLOYDIR) $(DEPLOY_TARBALL)
	mkdir -p $(DEPLOYDIR)/lib/include $(DEPLOYDIR)/apps $(DEPLOYDIR)/boot
	@if [ -e "$(APPDIR)/org.capsulad.radio.tuner" ] || \
	    ls "$(APPDIR)"/org.capsulad.radio.tuner-*.tar.gz >/dev/null 2>&1; then \
		echo "error: $(APPDIR) still holds the retired org.capsulad.radio.tuner companion." >&2; \
		echo "       The single org.capsulad.radio app now fetches and decodes in-process." >&2; \
		echo "       Remove the stale stage and archive explicitly, then rerun:" >&2; \
		echo "         rm -rf $(APPDIR)/org.capsulad.radio.tuner $(APPDIR)/org.capsulad.radio.tuner-*.tar.gz" >&2; \
		exit 1; \
	fi
	cp $(BINDIR)/capsulad $(DEPLOYDIR)/capsulad
ifeq ($(CLI_ENABLED),1)
	cp $(BINDIR)/capsulad-cli $(DEPLOYDIR)/capsulad-cli
else
	@echo "bundle: no CLI (BUNDLE_CLI=0); deploy and recovery need a separately pushed device CLI"
endif
	@for f in $(LIBDIR)/*; do \
		if [ -f "$$f" ] && [ ! -L "$$f" ]; then cp "$$f" $(DEPLOYDIR)/lib/; fi; \
	done
	@if [ -d $(INCLUDEDIR)/capsulad ]; then cp -R $(INCLUDEDIR)/capsulad $(DEPLOYDIR)/lib/include/; fi
	cp $(BOOT_HOOK_SRC) $(DEPLOYDIR)/boot/$(BOOT_HOOK_NAME)
	@if [ "$(BUNDLE_NATIVE)" = "0" ]; then \
		echo "bundle: no native runtime (BUNDLE_NATIVE=0); native app packages are unsupported on this target"; \
	elif [ -n "$(NATIVE_LIBC)" ]; then \
		lib=$$(ls $(DEPLOYDIR)/lib/libcapsulad.so.*.* 2>/dev/null | head -1); \
		if [ -z "$$lib" ]; then \
			echo "error: $(DEPLOY_PLATFORM) needs the native runtime, but $(DEPLOYDIR)/lib has no libcapsulad.so." >&2; \
			echo "       build it with NATIVE_TARGET=$(NATIVE_TARGET) (a $(NATIVE_LIBC) toolchain is required), or pass BUNDLE_NATIVE=0 explicitly." >&2; \
			exit 1; \
		fi; \
		sh tools/check-native-libc.sh "$$lib" "$(NATIVE_LIBC)" || { \
			echo "error: $(DEPLOY_PLATFORM) would install a runtime the image cannot load." >&2; \
			exit 1; \
		}; \
		echo "native runtime: $$lib ($(NATIVE_LIBC))"; \
	fi
	@set -e; \
	bundle_app() { \
		id=$$1; dir=$$2; target=$$3; copied=; \
		for f in "$$dir"/"$$id"-*.tar.gz; do \
			[ -f "$$f" ] || continue; \
			cp "$$f" $(DEPLOYDIR)/apps/; \
			if [ -n "$(NATIVE_LIBC)" ]; then \
				runtime=$$(tar -xzOf "$$f" manifest.toml 2>/dev/null | sed -n 's/^[[:space:]]*runtime[[:space:]]*=[[:space:]]*"\(.*\)".*/\1/p' | head -1); \
				case "$$runtime" in \
				native) \
					abi=$$(tar -xzOf "$$f" manifest.toml 2>/dev/null | sed -n 's/^[[:space:]]*abi[[:space:]]*=[[:space:]]*"\(.*\)".*/\1/p' | head -1); \
					case "$$abi" in \
					*"$(NATIVE_LIBC)"*) ;; \
					*) echo "error: $$f declares abi \"$$abi\", but $(DEPLOY_PLATFORM) needs $(NATIVE_LIBC); rebuild it with TARGET_ABI=$(TARGET_ABI)." >&2; exit 1 ;; \
					esac ;; \
				quickjs | micropython) \
					: ;; \
				*) echo "error: $$f declares runtime \"$$runtime\"; the $(DEPLOY_PLATFORM) ABI gate needs native, quickjs or micropython." >&2; exit 1 ;; \
				esac; \
			fi; \
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

# Independently built browser SPA and the settings-document kit. Outputs are
# embedded by internal/webui and internal/appui, so run this before rebuilding
# capsulad when their sources change. Both outputs are checked in, so ordinary
# Go builds do not need Node.
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
	$(NATIVE_RUSTFLAGS) $(CARGO) build --locked --manifest-path $(NATIVE_MANIFEST) $(NATIVE_CARGO_MODE) $(NATIVE_CARGO_TARGET)
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
	cp apps/dumbassistant/README.md $(APPDIR)/org.capsulad.dumbassistant/README.md
	@$(MAKE) check-target-toolchain
	@if [ "$(GOOS)" = linux ]; then sh tools/stage-manifest-abi.sh $(APPDIR)/org.capsulad.dumbassistant/manifest.toml "$(TARGET_ABI)"; fi
	cp -R apps/dumbassistant/assets/. $(APPDIR)/org.capsulad.dumbassistant/assets/
	@$(MAKE) check-target-toolchain
	@if [ "$(GOOS)" = linux ]; then sh tools/check-elf-interp.sh $(APPDIR)/org.capsulad.dumbassistant/dumbassistant "$(TARGET_ABI)" $(if $(filter linux-musl,$(TARGET_ABI)),require-dynamic,); fi
	@rm -f $(APPDIR)/org.capsulad.dumbassistant-*.tar.gz
	cd $(APPDIR) && $(abspath $(PACKAGER)) app package org.capsulad.dumbassistant

# Marusia is the second optional first-party app. It has no runtime assets
# beyond its self-contained settings document: the reply is a TTS URL fetched
# at turn time. The package carries the binary, the manifest, the app-owned
# settings page and the operator-facing README.md guide. The staged package
# directory and its <id>-<version>.tar.gz land side by side in $(APPDIR).
marusia: $(PACKAGER_TARGET)
	@test -f apps/marusia/ui/settings.html || { \
		echo "error: apps/marusia/ui/settings.html is missing" >&2; \
		exit 1; \
	}
	rm -rf $(APPDIR)/org.capsulad.marusia
	mkdir -p $(APPDIR)/org.capsulad.marusia/ui
	cd apps/marusia && CGO_ENABLED=1 $(GOBUILD) -o $(CURDIR)/$(APPDIR)/org.capsulad.marusia/marusia ./cmd/marusia
	cp apps/marusia/manifest.toml $(APPDIR)/org.capsulad.marusia/manifest.toml
	cp apps/marusia/README.md $(APPDIR)/org.capsulad.marusia/README.md
	cp -R apps/marusia/ui/. $(APPDIR)/org.capsulad.marusia/ui/
	@$(MAKE) check-target-toolchain
	@if [ "$(GOOS)" = linux ]; then sh tools/stage-manifest-abi.sh $(APPDIR)/org.capsulad.marusia/manifest.toml "$(TARGET_ABI)"; fi
	@$(MAKE) check-target-toolchain
	@if [ "$(GOOS)" = linux ]; then sh tools/check-elf-interp.sh $(APPDIR)/org.capsulad.marusia/marusia "$(TARGET_ABI)" $(if $(filter linux-musl,$(TARGET_ABI)),require-dynamic,); fi
	@rm -f $(APPDIR)/org.capsulad.marusia-*.tar.gz
	cd $(APPDIR) && $(abspath $(PACKAGER)) app package org.capsulad.marusia

# Cadence is the third optional first-party assistant: a client for a
# self-hosted Cadence server. Its own Makefile owns the package contents (the
# binary, manifest and README.md guide; no runtime assets), and the repository
# target supplies the output directory and the build-host packager.
cadence: $(PACKAGER_TARGET)
	$(MAKE) -C apps/cadence package OUT=$(abspath $(APPDIR)) CLI=$(abspath $(PACKAGER))

# Simon Says is a QuickJS script app: the package is its manifest, main.js,
# app guide and pre-rendered audio/ cues, with the contract test suite
# deliberately left out. Same conventions as the assistants (staged package
# directory and <id>-<version>.tar.gz side by side under $(APPDIR)); the script
# is authoritative for OUT/PACKAGE=0/CAPSULAD_CLI.
simon: $(PACKAGER_TARGET)
	OUT=$(abspath $(APPDIR)) CAPSULAD_CLI=$(abspath $(PACKAGER)) sh apps/simon/build.sh

# Soundboard is a QuickJS script app: the package is its manifest, main.js, app
# guide and custom settings UI. User sounds are shared /assets files, not
# package content; the contract suite stays out of the installed artifact.
soundboard: $(PACKAGER_TARGET)
	OUT=$(abspath $(APPDIR)) CAPSULAD_CLI=$(abspath $(PACKAGER)) sh apps/soundboard/build.sh

# Hardware Test is a native foreground test app. Its build script owns the
# package contents (binary, manifest, README) and needs the host native runtime
# to link the shared SDK.
hardwaretest:
	OUT=$(abspath $(APPDIR)) sh apps/hardwaretest/build.sh

# The PA Speaker MicroPython service: nothing is compiled, so its own script
# stages the manifest, the script, the descriptor and the interface
# documentation and packages them. It needs no native runtime, only the CLI.
pa: $(PACKAGER_TARGET)
	OUT=$(abspath $(APPDIR)) CAPSULAD_CLI=$(abspath $(PACKAGER)) sh apps/pa/build.sh

# The Internet Radio app is one native package: it fetches and decodes station
# audio in-process and serves the standard media contract. The app's build
# script owns its package contents (binary, manifest, settings UI, descriptor,
# README) and needs the host native runtime to link the shared SDK.
radio: $(PACKAGER_TARGET)
	OUT=$(abspath $(APPDIR)) CAPSULAD_CLI=$(abspath $(PACKAGER)) sh apps/radio/build.sh

# The Home Assistant integration is a native app: the build script compiles the Go
# binary and packages it with the manifest, its descriptor and its bridge
# documentation beside it. It needs the host native runtime (`make native`).
homeassistant:
	OUT=$(abspath $(APPDIR)) sh apps/homeassistant/build.sh

# The AirPlay 2 receiver is a native app: the build script compiles the Go
# binary, stages the manifest, the media-contract descriptor and the interface
# documentation, and packages them only for a Linux target (PACKAGE=auto). On
# the host it stages a development build and writes no archive, because a
# Darwin binary is not a device package. It needs the host native runtime
# (`make native`) and FFmpeg's libavcodec/libavutil.
airplay:
	OUT=$(abspath $(APPDIR)) sh apps/airplay/build.sh

# The device packages for both target libcs, built inside the pinned containers
# (apps/airplay/target/). Unlike `airplay`, this needs no host FFmpeg, no host
# native runtime and no cross compiler: it needs Docker. Artifacts and the LGPL
# relink material land in AIRPLAY_TARGET_OUT, never in the source tree.
#   make airplay-target                                    # both libcs
#   make airplay-target AIRPLAY_TARGET_ABI=linux-musl      # one libc
#   make airplay-target AIRPLAY_TARGET_OUT=/tmp/airplay    # elsewhere
AIRPLAY_TARGET_OUT ?= $(abspath $(APPDIR))/target
AIRPLAY_TARGET_ABI ?= all
airplay-target:
	OUT=$(AIRPLAY_TARGET_OUT) TARGET_ABI=$(AIRPLAY_TARGET_ABI) sh apps/airplay/target/build-target.sh

# All five product app packages in one step. `make native` first for the three
# native ones; each target is also usable on its own.
product-apps: simon pa radio homeassistant airplay

# `make test` runs every Go module that ships in this repository: the root Go
# module, the nested app modules (both assistants, Internet Radio and the Home
# Assistant bridge), the Rust binding and the native crate (the frozen C ABI),
# plus a compile of the public C++ example. `make test-race` adds the race
# detector to the root module. The script-app suites need Node or CPython, so
# they stay out of it: run them with `make test-apps` (`test-simon`, `test-pa`,
# `test-interpreter`).
test: test-go test-sdk test-dumbassistant test-marusia test-cadence test-radio test-homeassistant test-airplay

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

# The Cadence client is a nested Go module too. Its protocol tests use the
# upstream cadence.v2 golden frames and in-process peers; the Opus test needs
# libopus on the host (it skips with a clear message when it is missing).
test-cadence:
	cd apps/cadence && $(CGO_GO_ENV) $(GO) test -ldflags "$(HOST_GO_LINKMODE)" ./...

# The MicroPython host's prelude is pure Python, so its self-test (timer
# registration, cancellation, clock and poll behaviour) runs under CPython
# without building a host.
test-interpreter:
	@mkdir -p $(BINDIR)
	$(CC) -std=c11 -Wall -Wextra -Iinterpreter/common interpreter/common/capsulad_paths_test.c -o $(BINDIR)/capsulad-paths-test
	$(BINDIR)/capsulad-paths-test
	$(CC) -std=c11 -Wall -Wextra -Iinterpreter/common interpreter/common/capsulad_files_test.c -o $(BINDIR)/capsulad-files-test
	$(BINDIR)/capsulad-files-test
	python3 interpreter/micropython/sdk_test.py

# Host-side suites. Simon's contract tests load the shipped main.js into a fake
# QuickJS host under Node; Internet Radio's unit tests are a nested Go module;
# the Home Assistant integration is another nested Go module; and the MicroPython
# host's pure-Python prelude has its own self-test (timers included).
test-apps: test-simon test-soundboard test-hardwaretest test-radio test-homeassistant test-pa test-interpreter

test-simon:
	node --test apps/simon/test/*.test.mjs

test-soundboard:
	node --test apps/soundboard/test/*.test.mjs

test-hardwaretest:
	cd apps/hardwaretest && $(CGO_GO_ENV) $(GO) test -ldflags "$(HOST_GO_LINKMODE)" ./...

test-pa:
	python3 apps/pa/tests/test_wav.py
	python3 apps/pa/tests/test_app.py

test-radio:
	cd apps/radio && $(CGO_GO_ENV) $(GO) test -ldflags "$(HOST_GO_LINKMODE)" ./...

test-homeassistant:
	cd apps/homeassistant && $(CGO_GO_ENV) $(GO) test -ldflags "$(HOST_GO_LINKMODE)" ./...

# The AirPlay 2 receiver's tests are protocol-, codec- and session-level: no
# daemon, no device and no network. The hw package compiles against the cgo SDK
# binding, so this uses the same host link mode as the other native apps. When
# the host has FFmpeg's development libraries the AAC decoder and its
# end-to-end stream test are built too; without them those tests are absent and
# the receiver refuses AAC streams.
AIRPLAY_FFMPEG_TAGS := $(shell pkg-config --exists libavcodec libavutil 2>/dev/null && echo -tags ffmpeg)
test-airplay:
	cd apps/airplay && $(CGO_GO_ENV) $(GO) test $(AIRPLAY_FFMPEG_TAGS) -ldflags "$(HOST_GO_LINKMODE)" ./...

# Remove generated binaries and packages but leave build/demo (operator data)
# untouched.
clean:
	rm -rf $(BINDIR) $(LIBDIR) $(INCLUDEDIR) $(APPDIR) $(DEVAPPSDIR) $(DEPLOYDIR) $(DEPLOY_TARBALL)
	@if [ -f $(NATIVE_MANIFEST) ]; then $(CARGO) clean --manifest-path $(NATIVE_MANIFEST); fi
