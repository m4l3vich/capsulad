# Build environment for the glibc device artifacts.
#
# Debian buster is glibc 2.28. Building everything here is valid on the RK3308
# image (glibc 2.33) and is the exact baseline required by Amlogic stock.
FROM golang:1.22.6-bullseye AS go
FROM rust:1.88-bullseye AS rust

FROM debian:buster@sha256:58ce6f1271ae1c8a2006ff7d3e54e9874d839f573d8009c20154ad0f2fb0a225

ARG SNAPSHOT=20240601T000000Z

RUN printf 'deb http://snapshot.debian.org/archive/debian/%s/ buster main\ndeb http://snapshot.debian.org/archive/debian-security/%s/ buster/updates main\n' "$SNAPSHOT" "$SNAPSHOT" > /etc/apt/sources.list \
 && apt-get -o Acquire::Check-Valid-Until=false update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      build-essential cmake python3 curl ca-certificates git xz-utils unzip \
      file binutils pkg-config libssl1.1 libatomic1 \
 && rm -rf /var/lib/apt/lists/*

COPY --from=go /usr/local/go /usr/local/go
COPY --from=rust /usr/local/rustup /usr/local/rustup
COPY --from=rust /usr/local/cargo /usr/local/cargo

ENV PATH=/usr/local/go/bin:/usr/local/cargo/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    GOROOT=/usr/local/go \
    RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo \
    GOTOOLCHAIN=local

WORKDIR /src
