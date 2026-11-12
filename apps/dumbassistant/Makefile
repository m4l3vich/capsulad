APP_ID := org.capsula.dumbassistant
OUT ?= ../../build/apps/$(APP_ID)
GO ?= go

# The Go target decides linking, not the build host: a darwin target needs the
# external linker for LC_UUID, while Linux targets keep the internal one.
GOOS ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)
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

.PHONY: all build test clean

all: build

# build is deliberately phony: the Go command owns incremental rebuilds and
# correctly tracks replace-module SDK sources, compiler flags, cgo headers and
# generated files. A hand-written file prerequisite list silently goes stale
# whenever the SDK changes, so every invocation asks `go build` to decide.
build:
	mkdir -p $(OUT)/assets
	$(GOBUILD) -o $(OUT)/dumbassistant ./cmd/dumbassistant
	cp manifest.toml $(OUT)/manifest.toml
	cp assets/reply.wav $(OUT)/assets/reply.wav

test:
	$(GO) test ./...

clean:
	rm -rf $(OUT)
