# Capsula native SDK build helpers (owned by sdk/native).
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
  BUILT_LIB := $(RELEASE)/libcapsula.dylib
  INSTALL_LIB := libcapsula.1.dylib
  METADATA_LIB := libcapsula.1.dylib
  LINK_NAME := libcapsula.dylib
else
  BUILT_LIB := $(RELEASE)/libcapsula.so
  INSTALL_LIB := libcapsula.so.1.0.0
  METADATA_LIB := libcapsula.so.1
  LINK_NAME := libcapsula.so
endif

.PHONY: all build stage metadata fmt test clean

all: stage

build:
	$(CARGO) build --release --manifest-path Cargo.toml $(if $(TARGET),--target $(TARGET))

stage: build
	mkdir -p "$(DEST)"
ifeq ($(TARGET_OS),darwin)
	cp "$(BUILT_LIB)" "$(DEST)/$(INSTALL_LIB)"
	ln -sfn "$(INSTALL_LIB)" "$(DEST)/$(LINK_NAME)"
	./emit-runtime-metadata.sh "$(DEST)/capsula-native.json" "$(METADATA_LIB)"
else
	cp "$(BUILT_LIB)" "$(DEST)/libcapsula.so.1.0.0"
	ln -sfn "libcapsula.so.1.0.0" "$(DEST)/libcapsula.so.1"
	ln -sfn "libcapsula.so.1" "$(DEST)/$(LINK_NAME)"
	./emit-runtime-metadata.sh "$(DEST)/capsula-native.json" "libcapsula.so.1"
endif

metadata:
	mkdir -p "$(DEST)"
	./emit-runtime-metadata.sh "$(DEST)/capsula-native.json" "$(METADATA_LIB)"

fmt:
	$(CARGO) fmt --manifest-path Cargo.toml

test:
	$(CARGO) test --manifest-path Cargo.toml $(if $(TARGET),--target $(TARGET))

clean:
	$(CARGO) clean --manifest-path Cargo.toml
