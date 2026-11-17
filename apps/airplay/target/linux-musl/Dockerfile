# Builder for the linux-musl AirPlay device package: Alpine 3.21 (musl 1.2.5)
# plus the minimal static FFmpeg from container/ffmpeg.sh.
#
# Pinned by digest (apps/airplay/target/pins.sh records the same value): a tag
# is a moving name, and this image is what defines the artifact's libc.
FROM alpine:3.21@sha256:bc21487101ae47172509130ee1092cc273a831968d64b617776d1f69c81e42ab

ARG GO_VERSION=1.22.6
ARG GO_SHA256=6fd75a5251556936474521dc4f711ee23afd0b5e7e52b5f46b7bd768879f0078
ARG FFMPEG_SHA256=40973d44970dbc83ef302b0609f2e74982be2d85916dd2ee7472d30678a7abe6
ARG FFMPEG_VERSION=7.1
ARG FFMPEG_URL=https://ffmpeg.org/releases/ffmpeg-7.1.tar.xz
ARG APK_MIRROR=https://mirrors.edge.kernel.org/alpine
ARG GOPROXY_DOWNLOAD=https://proxy.golang.org

ENV AIRPLAY_FFMPEG_VERSION=${FFMPEG_VERSION} \
    AIRPLAY_FFMPEG_URL=${FFMPEG_URL} \
    AIRPLAY_FFMPEG_SHA256=${FFMPEG_SHA256}

# dl-cdn.alpinelinux.org delivers ~120-180 kB/s through the egress proxy this
# repository is built behind and stalls apk on the multi-megabyte packages;
# mirrors.edge.kernel.org serves the same aarch64 package set at ~1.2 MB/s.
RUN printf '%s/v3.21/main\n%s/v3.21/community\n' "$APK_MIRROR" "$APK_MIRROR" > /etc/apk/repositories \
 && apk add --no-cache build-base pkgconf tar xz linux-headers ca-certificates unzip file libgcc

# The official Go toolchain (linux/arm64), sha256-verified. Alpine's own `go`
# package needs the same stalled multi-megabyte download as gcc does, so it is
# not required; the official toolchain is statically linked and runs on musl.
COPY toolchain-go${GO_VERSION}.linux-arm64.zip /tmp/toolchain.zip
RUN echo "$GO_SHA256  /tmp/toolchain.zip" | sha256sum -c - \
 && mkdir -p /tmp/tc \
 && unzip -q /tmp/toolchain.zip -d /tmp/tc \
 && mv "/tmp/tc/golang.org/toolchain@v0.0.1-go${GO_VERSION}.linux-arm64" /usr/local/go \
 && rm -rf /tmp/tc /tmp/toolchain.zip \
 && /usr/local/go/bin/go version

ENV PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    GOROOT=/usr/local/go \
    GOPATH=/go \
    GOMODCACHE=/gomodcache \
    GOCACHE=/gocache \
    GOPROXY=off \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=mod \
    PKG_CONFIG_PATH=/opt/ffmpeg/lib/pkgconfig

COPY ffmpeg-${FFMPEG_VERSION}.tar.xz /build/ffmpeg.tar.xz
COPY container/ffmpeg.sh /usr/local/share/capsulad-airplay/ffmpeg.sh
RUN echo "$FFMPEG_SHA256  /build/ffmpeg.tar.xz" | sha256sum -c - \
 && sh /usr/local/share/capsulad-airplay/ffmpeg.sh

# Bake the module graph so a build needs no network at run time (go.sum pins
# every module). The tree is only mounted later, at run time.
COPY deps/ /deps/
# The module proxy in this build environment intermittently drops a stream
# ("stream error: stream ID ...; INTERNAL_ERROR"), so the download is retried;
# the module cache is content-addressed, which makes a retry cheap and exact.
RUN for i in 1 2 3 4 5; do \
      ( cd /deps/repo && GOFLAGS=-mod=mod GOPROXY=${GOPROXY_DOWNLOAD} /usr/local/go/bin/go mod download ) \
      && ( cd /deps/repo/apps/airplay && GOFLAGS=-mod=mod GOPROXY=${GOPROXY_DOWNLOAD} /usr/local/go/bin/go mod download ) \
      && break; \
      echo "module download attempt $i failed; retrying"; sleep 5; \
      [ "$i" = 5 ] && exit 1; \
    done; du -sh /gomodcache

COPY container/run.sh /usr/local/share/capsulad-airplay/run.sh

WORKDIR /work
CMD ["/bin/sh", "/usr/local/share/capsulad-airplay/run.sh"]