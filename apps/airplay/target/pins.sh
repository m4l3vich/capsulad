# Pinned inputs for the AirPlay target packages.
#
# Sourced by build-target.sh; every value is also visible in the container image
# (as a build argument or an ENV) so an artifact can be traced back to the exact
# toolchain that produced it. Nothing here downloads at source-checkout time.
#
# Why these exact inputs (all measured on this repository, not assumed):
#
# * Base images are pinned by digest, not by tag. A tag is a moving name.
# * Debian buster == glibc 2.28, the stock device's libc. Bullseye (2.31) was
#   tried and rejected: its libm gives exp/exp2/log/pow the GLIBC_2.29 symbol
#   version, which the stock rootfs does not have.
# * deb.debian.org is unusable behind the egress proxy this repository is built
#   through (the bullseye-security index names pool files that 404), so the
#   immutable snapshot the image was built from is pinned instead. It is also
#   the right choice for a durable recipe: an archive must not change under it.
# * The Go toolchain is the official linux/arm64 build, fetched from the Go
#   module proxy because go.dev/dl redirects to dl.google.com, which that proxy
#   answers with 404. The zip is the same content as the release tarball and was
#   verified against sum.golang.org
#   (h1:clsnFM6w6Wx8lF/6lIXxL9lf9WlSaOnspmG2XC20eSw=).
# * Alpine's default CDN delivers ~120-180 kB/s here and stalls apk outright on
#   the multi-megabyte packages (>25 min, twice); mirrors.edge.kernel.org serves
#   the same aarch64 packages at ~1.2 MB/s. Pointing apk at it is a transport
#   choice, not a different package set.
# * FFmpeg 7.1 is the release tarball; its own VERSION/RELEASE file reads 7.1.

AIRPLAY_TARGET_PLATFORM=linux/arm64

AIRPLAY_TARGET_MUSL_IMAGE=alpine:3.21
AIRPLAY_TARGET_MUSL_DIGEST=sha256:bc21487101ae47172509130ee1092cc273a831968d64b617776d1f69c81e42ab
AIRPLAY_TARGET_MUSL_APK_MIRROR=https://mirrors.edge.kernel.org/alpine

AIRPLAY_TARGET_GNU_IMAGE=debian:buster
AIRPLAY_TARGET_GNU_DIGEST=sha256:58ce6f1271ae1c8a2006ff7d3e54e9874d839f573d8009c20154ad0f2fb0a225
AIRPLAY_TARGET_GNU_SNAPSHOT=20240601T000000Z
AIRPLAY_TARGET_GNU_CEILING=2.28

AIRPLAY_TARGET_GO_VERSION=1.22.6
AIRPLAY_TARGET_GO_URL=https://proxy.golang.org/golang.org/toolchain/@v/v0.0.1-go1.22.6.linux-arm64.zip
AIRPLAY_TARGET_GO_SHA256=6fd75a5251556936474521dc4f711ee23afd0b5e7e52b5f46b7bd768879f0078
AIRPLAY_TARGET_GOPROXY=https://proxy.golang.org

AIRPLAY_TARGET_FFMPEG_VERSION=7.1
AIRPLAY_TARGET_FFMPEG_URL=https://ffmpeg.org/releases/ffmpeg-7.1.tar.xz
AIRPLAY_TARGET_FFMPEG_SHA256=40973d44970dbc83ef302b0609f2e74982be2d85916dd2ee7472d30678a7abe6

# Where a caller may drop a target libcapsulad.so.1 for the optional load smoke:
#   $OUT/runtime/<abi>/libcapsulad.so.1
# The runtime is launcher-injected and is never linked into the package.
AIRPLAY_TARGET_RUNTIME_DIR=runtime