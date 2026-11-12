GO ?= go
CARGO ?= cargo

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
LIBDIR ?= build/lib
INCLUDEDIR ?= build/include
APPDIR ?= build/apps

NATIVE_DIR ?= sdk/native
NATIVE_MANIFEST ?= $(NATIVE_DIR)/Cargo.toml
NATIVE_HEADER ?= $(NATIVE_DIR)/include/capsula/capsula.h

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
NATIVE_SOURCE := libcapsula.dylib
NATIVE_LIBRARY := libcapsula.1.dylib
NATIVE_METADATA_LIBRARY := libcapsula.1.dylib
else
NATIVE_SOURCE := libcapsula.so
NATIVE_LIBRARY := libcapsula.so.1.0.0
NATIVE_METADATA_LIBRARY := libcapsula.so.1
endif

.PHONY: all release debug all-targets daemon daemon-release daemon-debug capsulad capsula sdk sdk-go sdk-rust native dumbassistant test test-sdk check-sdk-abi test-dumbassistant clean

all: release

release:
	$(MAKE) PROFILE=release all-targets

debug:
	$(MAKE) PROFILE=debug all-targets

all-targets: capsulad capsula native sdk

# Go-only build, including cross targets without a native SDK or cgo.
daemon: capsulad capsula

daemon-release:
	$(MAKE) PROFILE=release daemon

daemon-debug:
	$(MAKE) PROFILE=debug daemon

capsulad:
	mkdir -p $(BINDIR)
	rm -f $(BINDIR)/capsulad
	$(GOBUILD) -o $(BINDIR)/capsulad ./cmd/capsulad

capsula:
	mkdir -p $(BINDIR)
	rm -f $(BINDIR)/capsula
	$(GOBUILD) -o $(BINDIR)/capsula ./cmd/capsula

# The Go SDK is its own module; it exposes the thin binding over libcapsula.
# It uses cgo, so a cross build also needs a matching C toolchain.
sdk: sdk-go sdk-rust

sdk-go:
	cd sdk/go && $(CGO_GO_ENV) $(GO) build ./...

# The Rust binding loads the library dynamically; it does not link the engine.
sdk-rust:
	$(CARGO) build --manifest-path sdk/rust/Cargo.toml $(NATIVE_CARGO_MODE) $(NATIVE_CARGO_TARGET)

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
	mkdir -p $(LIBDIR) $(INCLUDEDIR)/capsula
	rm -f $(LIBDIR)/libcapsula.so $(LIBDIR)/libcapsula.so.1 $(LIBDIR)/libcapsula.so.1.0.0 $(LIBDIR)/libcapsula.dylib $(LIBDIR)/libcapsula.1.dylib $(LIBDIR)/capsula-native.json $(INCLUDEDIR)/capsula.h $(INCLUDEDIR)/capsula/capsula.h
	$(CARGO) build --manifest-path $(NATIVE_MANIFEST) $(NATIVE_CARGO_MODE) $(NATIVE_CARGO_TARGET)
	@test -f $(NATIVE_OUTDIR)/$(NATIVE_SOURCE) || { \
		echo "error: Cargo did not produce $(NATIVE_OUTDIR)/$(NATIVE_SOURCE)." >&2; \
		echo "       Install the Rust target and linker, or set NATIVE_TARGET for the intended ABI." >&2; \
		exit 1; \
	}
ifeq ($(GOOS),darwin)
	cp $(NATIVE_OUTDIR)/$(NATIVE_SOURCE) $(LIBDIR)/$(NATIVE_LIBRARY)
	ln -sfn $(NATIVE_LIBRARY) $(LIBDIR)/libcapsula.dylib
else
	cp $(NATIVE_OUTDIR)/$(NATIVE_SOURCE) $(LIBDIR)/$(NATIVE_LIBRARY)
	ln -sfn $(NATIVE_LIBRARY) $(LIBDIR)/libcapsula.so.1
	ln -sfn libcapsula.so.1 $(LIBDIR)/libcapsula.so
endif
	cp $(NATIVE_HEADER) $(INCLUDEDIR)/capsula/capsula.h
	sh $(NATIVE_METADATA_SCRIPT) $(LIBDIR)/capsula-native.json $(NATIVE_METADATA_LIBRARY)

# Optional first-party app. It is a separate nested module and is never part
# of `all`, `release` or `debug`.
dumbassistant:
	mkdir -p $(APPDIR)/org.capsula.dumbassistant/assets
	cd apps/dumbassistant && CGO_ENABLED=1 $(GOBUILD) -o $(CURDIR)/$(APPDIR)/org.capsula.dumbassistant/dumbassistant ./cmd/dumbassistant
	cp apps/dumbassistant/manifest.toml $(APPDIR)/org.capsula.dumbassistant/manifest.toml
	cp apps/dumbassistant/assets/reply.wav $(APPDIR)/org.capsula.dumbassistant/assets/reply.wav

test:
	$(GO) test -ldflags "$(GO_LINKMODE)" ./...

test-sdk: check-sdk-abi
	cd sdk/go && $(CGO_GO_ENV) $(GO) test ./...
	$(CARGO) test --manifest-path sdk/rust/Cargo.toml $(NATIVE_CARGO_MODE) $(NATIVE_CARGO_TARGET)

check-sdk-abi:
	sh sdk/go/regenerate-abi.sh --check

test-dumbassistant:
	cd apps/dumbassistant && $(CGO_GO_ENV) $(GO) test ./...

# Remove generated binaries and packages but leave build/demo (operator data)
# untouched.
clean:
	rm -rf $(BINDIR) $(LIBDIR) $(INCLUDEDIR) $(APPDIR)
	@if [ -f $(NATIVE_MANIFEST) ]; then $(CARGO) clean --manifest-path $(NATIVE_MANIFEST); fi
