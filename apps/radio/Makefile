# Internet radio (native). The package contents are the manifest, the binary,
# the packaged settings document, the media descriptor and the app guide.
# Packaging runs the repository CLI on the build host.
APP_ID := org.capsulad.radio
OUT ?= ../../build/apps
CLI ?= ../../build/bin/capsulad-cli
GO ?= go

.PHONY: all build package test clean

all: package

build:
	OUT=$(abspath $(OUT)) CAPSULAD_CLI=$(abspath $(CLI)) PACKAGE=0 sh build.sh

package:
	OUT=$(abspath $(OUT)) CAPSULAD_CLI=$(abspath $(CLI)) sh build.sh

# Host unit tests. They use no daemon and no network.
test:
	CGO_ENABLED=1 $(GO) test ./...

clean:
	rm -rf $(OUT)/$(APP_ID)
	@rm -f $(OUT)/$(APP_ID)-*.tar.gz