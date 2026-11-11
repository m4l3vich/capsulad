GO ?= go
# Go's internal darwin linker on this host does not emit LC_UUID, which newer
# dyld versions reject. External linking (system clang) is required for local
# macOS development; Linux targets keep the default internal linker.
LDFLAGS ?= $(shell [ "$$(uname -s)" = Darwin ] && echo -linkmode=external)
GOBUILD = $(GO) build -ldflags '$(LDFLAGS)'
BINDIR := build/bin
APPDIR := build/apps

.PHONY: all capsulad capsula apps demo test clean

all: capsulad capsula

capsulad:
	$(GOBUILD) -o $(BINDIR)/capsulad ./cmd/capsulad

capsula:
	$(GOBUILD) -o $(BINDIR)/capsula ./cmd/capsula

apps: $(APPDIR)/org.capsula.clock/clock $(APPDIR)/org.capsula.assistant/assistant $(APPDIR)/org.capsula.media/media $(APPDIR)/org.example.ring-controller/controller

$(APPDIR)/org.capsula.clock/clock: apps/clock/main.go apps/clock/manifest.toml
	mkdir -p $(APPDIR)/org.capsula.clock
	$(GOBUILD) -o $@ ./apps/clock
	cp apps/clock/manifest.toml $(APPDIR)/org.capsula.clock/manifest.toml

$(APPDIR)/org.capsula.assistant/assistant: apps/assistant/main.go apps/assistant/manifest.toml
	mkdir -p $(APPDIR)/org.capsula.assistant
	$(GOBUILD) -o $@ ./apps/assistant
	cp apps/assistant/manifest.toml $(APPDIR)/org.capsula.assistant/manifest.toml

$(APPDIR)/org.example.ring-controller/controller: apps/ring/main.go apps/ring/manifest.toml apps/ring/schemas/ring.protoset apps/ring/docs/ring-api.md
	mkdir -p $(APPDIR)/org.example.ring-controller/schemas $(APPDIR)/org.example.ring-controller/docs
	$(GOBUILD) -o $@ ./apps/ring
	cp apps/ring/manifest.toml $(APPDIR)/org.example.ring-controller/manifest.toml
	cp apps/ring/schemas/ring.protoset $(APPDIR)/org.example.ring-controller/schemas/
	cp apps/ring/docs/ring-api.md $(APPDIR)/org.example.ring-controller/docs/

$(APPDIR)/org.capsula.media/media: apps/media/main.go apps/media/manifest.toml apps/media/schemas/media.protoset
	mkdir -p $(APPDIR)/org.capsula.media/schemas
	$(GOBUILD) -o $@ ./apps/media
	cp apps/media/manifest.toml $(APPDIR)/org.capsula.media/manifest.toml
	cp apps/media/schemas/media.protoset $(APPDIR)/org.capsula.media/schemas/

demo: all apps
	@echo "Run:"
	@echo "  $(BINDIR)/capsula -data-dir build/demo app install $(APPDIR)/org.capsula.clock"
	@echo "  $(BINDIR)/capsula -data-dir build/demo app install $(APPDIR)/org.capsula.assistant"
	@echo "  $(BINDIR)/capsula -data-dir build/demo app install $(APPDIR)/org.capsula.media"
	@echo "  $(BINDIR)/capsula -data-dir build/demo app install $(APPDIR)/org.example.ring-controller"
	@echo "  $(BINDIR)/capsula -data-dir build/demo auth init -hostname localhost -ip 127.0.0.1"
	@echo "  $(BINDIR)/capsulad -platform simulator -data-dir build/demo -web-addr 127.0.0.1:8443 -dev-admin build/demo/run/dev.sock"

test:
	$(GO) test -ldflags "$(LDFLAGS)" ./...

clean:
	rm -rf build
