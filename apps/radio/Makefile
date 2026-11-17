# Internet radio (MicroPython). A script package: staging copies the manifest,
# the app script and the published descriptors; packaging runs the repository
# CLI on the build host. Nothing here is compiled and nothing ships to the
# device except the .tar.gz artifact.
APP_ID := org.capsulad.radio
OUT ?= ../../build/apps/$(APP_ID)
ARTIFACT_DIR := $(dir $(OUT))
CLI ?= ../../build/bin/capsulad-cli
PYTHON ?= python3
PROTOC ?= protoc

.PHONY: all build package descriptors test e2e e2e-tuner clean

all: package

build:
	rm -rf $(OUT)
	mkdir -p $(OUT)/proto
	cp micropython/manifest.toml micropython/main.py $(OUT)/
	cp micropython/proto/radio.proto micropython/proto/radio.protoset \
	   micropython/proto/media.protoset micropython/proto/radio.md \
	   micropython/proto/media.md $(OUT)/proto/

# The artifact is what the device installs; packaging validates the manifest,
# the settings, the interface descriptors and the package layout.
package: build
	@test -x "$(CLI)" || { \
		echo "error: app packager $(CLI) is missing;" >&2; \
		echo "       run 'make capsulad-cli' in the repository root." >&2; \
		exit 1; \
	}
	@rm -f $(ARTIFACT_DIR)$(APP_ID)-*.tar.gz
	cd $(ARTIFACT_DIR) && $(abspath $(CLI)) app package $(abspath $(OUT))

# Regenerate the published descriptors after editing the contracts.
descriptors:
	$(PROTOC) --proto_path=micropython/proto --include_imports \
		--descriptor_set_out=micropython/proto/radio.protoset \
		micropython/proto/radio.proto
	$(PROTOC) --proto_path=../.. --include_imports \
		--descriptor_set_out=micropython/proto/media.protoset \
		../../proto/capsulad/contracts/media/v1/media.proto

# Host unit tests: the shipped main.py driven by a scripted host double.
test:
	cd tests && $(PYTHON) -m unittest discover -s . -p 'test_*.py'

# Simulator end-to-end: the real daemon, the real MicroPython host, the app and
# the test tuner fixture. Needs build/bin/{capsulad,capsulad-cli} and
# build/lib/{libcapsulad,capsulad-micropython}.
e2e: build
	sh tests/e2e.sh

# Live end-to-end with the real native tuner companion and a real MP3 station
# (network). Needs build/apps/org.capsulad.radio.tuner-*.tar.gz.
e2e-tuner: build
	sh tests/e2e-tuner.sh

clean:
	rm -rf $(OUT)
	@rm -f $(ARTIFACT_DIR)$(APP_ID)-*.tar.gz
