# Build environment for the musl device artifacts (Amlogic Alpine).
FROM golang:1.22.6-alpine AS go
FROM rust:1.88-alpine3.21

RUN apk add --no-cache \
      build-base cmake python3 curl ca-certificates git tar xz \
      linux-headers unzip file pkgconf libgcc

COPY --from=go /usr/local/go /usr/local/go

ENV PATH=/usr/local/go/bin:/usr/local/cargo/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    GOROOT=/usr/local/go \
    RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo \
    GOTOOLCHAIN=local

WORKDIR /src
