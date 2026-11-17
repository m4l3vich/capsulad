# Builder for the linux-gnu AirPlay device package: Debian buster == glibc 2.28,
# the stock device's libc, plus the minimal static FFmpeg from
# container/ffmpeg.sh.
#
# Pinned by digest (apps/airplay/target/pins.sh records the same value).
#
# Why buster: linking against glibc 2.28 caps the binary's symbol versions at
# 2.28, which is what stock provides. A bullseye (2.31) build of the same source
# required exp/exp2/log/pow@GLIBC_2.29 from libm and would not load there.
#
# Why the snapshot archive: deb.debian.org is inconsistent behind the egress
# proxy this repository is built through (the bullseye-security index names pool
# files that return 404), and buster has left the live archive anyway. A dated
# snapshot is immutable, which is what a pinned build wants.
FROM debian:buster@sha256:58ce6f1271ae1c8a2006ff7d3e54e9874d839f573d8009c20154ad0f2fb0a225

ARG GO_VERSION=1.22.6
ARG GO_SHA256=6fd75a5251556936474521dc4f711ee23afd0b5e7e52b5f46b7bd768879f0078
ARG FFMPEG_SHA256=40973d44970dbc83ef302b0609f2e74982be2d85916dd2ee7472d30678a7abe6
ARG FFMPEG_VERSION=7.1
ARG FFMPEG_URL=https://ffmpeg.org/releases/ffmpeg-7.1.tar.xz
ARG SNAPSHOT=20240601T000000Z
ARG GOPROXY_DOWNLOAD=https://proxy.golang.org

ENV AIRPLAY_FFMPEG_VERSION=${FFMPEG_VERSION} \
    AIRPLAY_FFMPEG_URL=${FFMPEG_URL} \
    AIRPLAY_FFMPEG_SHA256=${FFMPEG_SHA256}

RUN printf 'deb http://snapshot.debian.org/archive/debian/%s/ buster main\ndeb http://snapshot.debian.org/archive/debian-security/%s/ buster/updates main\n' "$SNAPSHOT" "$SNAPSHOT" > /etc/apt/sources.list \
 && apt-get -o Acquire::Check-Valid-Until=false update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      gcc make libc6-dev binutils pkg-config libatomic1 xz-utils ca-certificates file unzip \
 && rm -rf /var/lib/apt/lists/* \
 && gcc --version | head -1 \
 && ldd --version | head -1

# The official Go toolchain (linux/arm64), sha256-verified, from the module
# proxy (go.dev/dl redirects to dl.google.com, which the proxy answers with
# 404). The toolchain itself is statically linked, so the host glibc is
# irrelevant to it.
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