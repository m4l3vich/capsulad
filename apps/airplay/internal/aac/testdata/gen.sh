#!/bin/sh
# Regenerate the AAC fixtures. They are produced by a real encoder (ffmpeg) and
# the reference PCM by ffmpeg's own decode of the same file, so the test does
# not compare the decoder against itself.
set -eu
cd "$(dirname "$0")"
FFMPEG=${FFMPEG:-ffmpeg}

gen() {
  name=$1; rate=$2; channels=$3; filter=$4
  "$FFMPEG" -hide_banner -loglevel error -y \
    -f lavfi -i "$filter" -ar "$rate" -ac "$channels" -c:a aac -b:a 128k -f adts "$name.adts"
  "$FFMPEG" -hide_banner -loglevel error -y \
    -i "$name.adts" -f s16le -c:a pcm_s16le "$name.raw"
}

gen aac-lc-44100-stereo 44100 2 "sine=frequency=440:sample_rate=44100:duration=0.12"
gen aac-lc-48000-mono 48000 1 "sine=frequency=660:sample_rate=48000:duration=0.12"
