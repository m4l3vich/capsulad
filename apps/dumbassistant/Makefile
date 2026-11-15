APP_ID := org.capsula.dumbassistant
OUT ?= ../../build/apps/$(APP_ID)
ARTIFACT_DIR := $(dir $(OUT))
GO ?= go

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
# The SDK uses cgo to dlopen the shared engine. A hardware aarch64/glibc build
# must pass a target compiler/sysroot (for example
# CC=aarch64-linux-gnu-gcc); a host Go cross build without one is not valid.
CC ?= $(shell $(GO) env CC)
CGO_ENABLED ?= 1
ifeq ($(GOOS),darwin)
GO_LINKMODE := -linkmode=external
else
GO_LINKMODE :=
endif

# Release binaries are stripped: -s drops the symbol table and -w drops DWARF.
LDFLAGS ?= -s -w $(GO_LINKMODE)
GOBUILD = GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=$(CGO_ENABLED) CC='$(CC)' \
	$(GO) build -trimpath -ldflags '$(LDFLAGS)'

.PHONY: all build package test clean

all: package

# build is deliberately phony: the Go command owns incremental rebuilds and
# correctly tracks replace-module SDK sources, compiler flags, cgo headers and
# generated files. A hand-written file prerequisite list silently goes stale
# whenever the SDK changes, so every invocation asks `go build` to decide.
build:
	mkdir -p $(OUT)/assets
	$(GOBUILD) -o $(OUT)/dumbassistant ./cmd/dumbassistant
	cp manifest.toml $(OUT)/manifest.toml
	cp assets/reply.wav $(OUT)/assets/reply.wav

# package writes the installable artifact beside the package directory:
# <artifact-dir>/org.capsula.dumbassistant-<version>.tar.gz. The version comes
# from manifest.toml, so no version is repeated here.
package: build
	@test -x "$(CLI)" || { \
		echo "error: app packager $(CLI) is missing;" >&2; \
		echo "       run 'make $(PACKAGER_TARGET)' in the repository root." >&2; \
		exit 1; \
	}
	@rm -f $(ARTIFACT_DIR)$(APP_ID)-*.tar.gz
	cd $(ARTIFACT_DIR) && $(abspath $(CLI)) app package $(abspath $(OUT))

test:
	$(GO) test ./...

clean:
	rm -rf $(OUT)
	rm -f $(ARTIFACT_DIR)$(APP_ID)-*.tar.gz
