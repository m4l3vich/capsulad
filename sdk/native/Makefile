# Capsulad native SDK build helpers (owned by sdk/native).
#
# The root Makefile / Packaging drives release builds; these targets are the
# canonical commands for the native artifact and its runtime metadata.

CARGO ?= cargo
TARGET ?=
TARGET_OS ?= $(shell uname -s | tr '[:upper:]' '[:lower:]')
DEST ?= ../../build/lib
TARGET_DIR := $(if $(TARGET),target/$(TARGET),target)
RELEASE := $(TARGET_DIR)/release

ifeq ($(TARGET_OS),darwin)
  BUILT_LIB := $(RELEASE)/libcapsulad.dylib
  INSTALL_LIB := libcapsulad.1.dylib
  METADATA_LIB := libcapsulad.1.dylib
  LINK_NAME := libcapsulad.dylib
else
  BUILT_LIB := $(RELEASE)/libcapsulad.so
  INSTALL_LIB := libcapsulad.so.1.0.0
  METADATA_LIB := libcapsulad.so.1
  LINK_NAME := libcapsulad.so
endif

.PHONY: all build stage metadata descriptor fmt test clean

# The framework descriptor is compiled into the library (include_bytes!), so a
# regenerated file reaches apps only after rebuild and redeploy.
PROTO_DIR := ../../proto
DESCRIPTOR := descriptors/capsulad-framework.protoset
FRAMEWORK_PROTOS := capsulad/framework/v1/framework.proto \
	capsulad/contracts/clock/v1/clock.proto \
	capsulad/contracts/media/v1/media.proto \
	capsulad/contracts/ring/v1/ring.proto

descriptor:
	protoc -I "$(PROTO_DIR)" --include_imports --descriptor_set_out="$(DESCRIPTOR)" \
		$(FRAMEWORK_PROTOS)

all: stage

build:
	$(CARGO) build --release --manifest-path Cargo.toml $(if $(TARGET),--target $(TARGET))

stage: build
	mkdir -p "$(DEST)"
ifeq ($(TARGET_OS),darwin)
	cp "$(BUILT_LIB)" "$(DEST)/$(INSTALL_LIB)"
	ln -sfn "$(INSTALL_LIB)" "$(DEST)/$(LINK_NAME)"
	./emit-runtime-metadata.sh "$(DEST)/capsulad-native.json" "$(METADATA_LIB)"
else
	cp "$(BUILT_LIB)" "$(DEST)/libcapsulad.so.1.0.0"
	ln -sfn "libcapsulad.so.1.0.0" "$(DEST)/libcapsulad.so.1"
	ln -sfn "libcapsulad.so.1" "$(DEST)/$(LINK_NAME)"
	./emit-runtime-metadata.sh "$(DEST)/capsulad-native.json" "libcapsulad.so.1"
endif

metadata:
	mkdir -p "$(DEST)"
	./emit-runtime-metadata.sh "$(DEST)/capsulad-native.json" "$(METADATA_LIB)"

fmt:
	$(CARGO) fmt --manifest-path Cargo.toml

test:
	$(CARGO) test --manifest-path Cargo.toml $(if $(TARGET),--target $(TARGET))

clean:
	$(CARGO) clean --manifest-path Cargo.toml
