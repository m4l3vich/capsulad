APP_ID := org.capsulad.cadence
# OUT is the directory that holds the package directory; the archive lands in
# it, beside <OUT>/<app-id>.
OUT ?= ../../build/apps
GO ?= go

# The target ABI is glibc (linux-gnu, the Rockchip stock board) by default;
# TARGET_ABI=linux-musl (or the alias "musl") builds for Alpine. It selects the
# cross compiler's default and is written into the staged manifest, because a
# glibc-linked binary cannot run under musl however the manifest describes it.
# An explicit empty value falls back to the default so a root target that does
# not define TARGET_ABI cannot break the build.
TARGET_ABI ?= linux-gnu
# A command-line TARGET_ABI overrides an ordinary assignment, so the default and
# the alias need override to take effect.
ifeq ($(strip $(TARGET_ABI)),)
override TARGET_ABI := linux-gnu
endif
ifeq ($(TARGET_ABI),musl)
override TARGET_ABI := linux-musl
endif

# The Go target decides linking, not the build host: a darwin target needs the
# external linker for LC_UUID, while Linux targets keep the internal one.
GOOS ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)
# Packaging always runs on the build host, so a cross build needs the host copy
# of the repository CLI (`make capsulad-cli-host` in the repository root).
HOST_GOOS := $(shell $(GO) env GOHOSTOS)
HOST_GOARCH := $(shell $(GO) env GOHOSTARCH)
ifeq ($(GOOS)/$(GOARCH),$(HOST_GOOS)/$(HOST_GOARCH))
CLI ?= ../../build/bin/capsulad-cli
PACKAGER_TARGET := capsulad-cli
else
CLI ?= ../../build/bin/capsulad-cli-host
PACKAGER_TARGET := capsulad-cli-host
endif
# The SDK uses cgo to dlopen the shared engine, so a cross build needs a target
# compiler/sysroot. Its default follows the target ABI: the musl toolchain
# triplet is aarch64/x86_64-linux-musl-gcc, not the goarch name. CC is a make
# built-in (cc), so "?=" never applies; override only the built-in default and
# keep an explicit command-line or environment CC.
MUSL_TRIPLE := $(if $(filter arm64 aarch64,$(GOARCH)),aarch64,$(if $(filter amd64 x86_64,$(GOARCH)),x86_64,$(GOARCH)))
ifeq ($(TARGET_ABI),linux-musl)
ifeq ($(origin CC),default)
CC := $(MUSL_TRIPLE)-linux-musl-gcc
endif
else
ifeq ($(origin CC),default)
CC := $(shell $(GO) env CC)
endif
endif
CGO_ENABLED ?= 1
ifeq ($(GOOS),darwin)
GO_LINKMODE := -linkmode=external
else
GO_LINKMODE :=
endif
# Tests run on the build host, so the host link mode applies (a darwin host
# needs the external linker for LC_UUID even when the target is linux).
ifeq ($(HOST_GOOS),darwin)
HOST_GO_LINKMODE := -linkmode=external
else
HOST_GO_LINKMODE :=
endif

# Release binaries are stripped: -s drops the symbol table and -w drops DWARF.
LDFLAGS ?= -s -w $(GO_LINKMODE)
GOBUILD = GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=$(CGO_ENABLED) CC='$(CC)' \
	$(GO) build -trimpath -ldflags '$(LDFLAGS)'

.PHONY: all build package test clean check-abi

all: package

# build is deliberately phony: the Go command owns incremental rebuilds and
# correctly tracks replace-module SDK sources, compiler flags, cgo headers and
# generated files. The package carries the binary, the manifest and the
# operator-facing README.md user guide; there are no runtime assets.
# build may run on the host for inspection; only package insists on Linux.
build:
	rm -rf $(OUT)/$(APP_ID)
	mkdir -p $(OUT)/$(APP_ID)
	$(GOBUILD) -o $(OUT)/$(APP_ID)/cadence ./cmd/cadence
	@sed -E 's/^abi = "[^"]*"$$/abi = "$(TARGET_ABI)"/' manifest.toml > $(OUT)/$(APP_ID)/manifest.toml
	cp README.md $(OUT)/$(APP_ID)/README.md
	@grep -q '^abi = "$(TARGET_ABI)"$$' $(OUT)/$(APP_ID)/manifest.toml || { \
		echo "error: manifest.toml has no single entrypoint abi line to set to $(TARGET_ABI)" >&2; \
		exit 1; \
	}

# check-abi proves the packaged binary matches its manifest through the shared
# executable-side checker: its ELF interpreter must name the loader the declared
# libc uses. A darwin host binary is refused outright: this manifest declares a
# Linux ABI, so a host artifact is for tests only and must never be packaged.
check-abi: build
	@case "$(GOOS)" in \
	linux) \
		sh $(abspath ../../tools/check-elf-interp.sh) $(OUT)/$(APP_ID)/cadence "$(TARGET_ABI)";; \
	*) echo "error: this manifest declares a Linux ABI, but GOOS=$(GOOS);" >&2; \
	   echo "       a host binary is for tests only; build the package with GOOS=linux" >&2; \
	   echo "       (a target toolchain or an arm64 Linux container), never a darwin binary." >&2; \
	   exit 1;; \
	esac

# package writes the installable artifact beside the package directory:
# <artifact-dir>/org.capsulad.cadence-<version>.tar.gz. The version comes from
# manifest.toml, so no version is repeated here.
package: check-abi
	@test -x "$(CLI)" || { \
		echo "error: app packager $(CLI) is missing;" >&2; \
		echo "       run 'make $(PACKAGER_TARGET)' in the repository root." >&2; \
		exit 1; \
	}
	@rm -f $(OUT)/$(APP_ID)-*.tar.gz
	cd $(OUT) && $(abspath $(CLI)) app package $(abspath $(OUT))/$(APP_ID)

test:
	$(GO) test -ldflags '$(HOST_GO_LINKMODE)' ./...

clean:
	rm -rf $(OUT)/$(APP_ID)
	rm -f $(OUT)/$(APP_ID)-*.tar.gz
