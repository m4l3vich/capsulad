#!/usr/bin/env bash
#
# Regenerates the ALAC decoder fixtures checked into this directory.
#
#   ./gen.sh
#
# Requirements: ffmpeg (7.x or newer) and python3 (stdlib only).
# The fixtures are deterministic for a given ffmpeg version: the input signal is
# a 440 Hz sine followed by a short noise burst (anoisesrc with a fixed seed).
#
# Produced per fixture <name>:
#   <name>.m4a        ALAC-in-MP4 (the container we extract frames from)
#   <name>.raw        reference PCM decoded by ffmpeg (s16le / s24le)
#   <name>.frames.bin length-prefixed ALAC frames + the ALAC magic cookie
#
set -euo pipefail
cd "$(dirname "$0")"

FFMPEG="${FFMPEG:-ffmpeg}"
PYTHON="${PYTHON:-python3}"

for tool in "$FFMPEG" "$PYTHON"; do
  command -v "$tool" >/dev/null || { echo "$tool not found: set FFMPEG= or PYTHON=" >&2; exit 1; }
done

SINE_SECS=0.06
NOISE_SECS=0.04

# mux() <out> <sample_rate> <channels> <sample_fmt> <bits_per_raw_sample>
mux() {
  local out="$1" sr="$2" ch="$3" fmt="$4" bps="$5"
  local pan
  if [ "$ch" = "2" ]; then
    # right channel is deliberately different from the left so that the stereo
    # decorrelation ("interlacing") paths are actually exercised.
    pan="pan=stereo|c0=c0|c1=0.5*c0,"
  else
    pan=""
  fi
  "$FFMPEG" -hide_banner -loglevel error -y \
    -f lavfi -i "sine=frequency=440:sample_rate=${sr}:duration=${SINE_SECS}" \
    -f lavfi -i "anoisesrc=color=white:seed=1234:amplitude=0.3:sample_rate=${sr}:duration=${NOISE_SECS}" \
    -filter_complex "[0:a][1:a]concat=n=2:v=0:a=1,${pan}aformat=sample_fmts=${fmt}:channel_layouts=$( [ "$ch" = "2" ] && echo stereo || echo mono )[a]" \
    -map "[a]" -ar "$sr" -ac "$ch" -c:a alac -bits_per_raw_sample "$bps" \
    "$out"
}

# raw() <in> <out> <pcm_codec> <raw_fmt>
raw() {
  "$FFMPEG" -hide_banner -loglevel error -y -i "$1" -f "$4" -c:a "$3" "$2"
}

# frames() <m4a> <frames.bin> <label>
frames() {
  "$PYTHON" gen_frames.py "$1" "$2" "$3"
}

echo "== 16-bit stereo 44100 (ALAC CPE)"
mux alac-stereo44100-16.m4a 44100 2 s16 16
raw alac-stereo44100-16.m4a alac-stereo44100-16.raw pcm_s16le s16le
frames alac-stereo44100-16.m4a alac-stereo44100-16.frames.bin alac-stereo44100-16

echo "== 16-bit mono 48000 (ALAC SCE)"
mux alac-mono48000-16.m4a 48000 1 s16 16
raw alac-mono48000-16.m4a alac-mono48000-16.raw pcm_s16le s16le
frames alac-mono48000-16.m4a alac-mono48000-16.frames.bin alac-mono48000-16

echo "== 24-bit stereo 44100 (ALAC CPE, 8 extra bits per sample)"
mux alac-stereo44100-24.m4a 44100 2 s32 24
raw alac-stereo44100-24.m4a alac-stereo44100-24.raw pcm_s24le s24le
frames alac-stereo44100-24.m4a alac-stereo44100-24.frames.bin alac-stereo44100-24

echo "== 16-bit mono 48000 verbatim (ALAC SCE, uncompressed samples)"
# compression_level 0 makes the encoder emit isnotcompressed ("verbatim") frames,
# which is the escape / per-sample code path of the decoder.
"$FFMPEG" -hide_banner -loglevel error -y \
  -f lavfi -i "sine=frequency=660:sample_rate=48000:duration=${SINE_SECS}" \
  -f lavfi -i "anoisesrc=color=white:seed=4321:amplitude=0.2:sample_rate=48000:duration=${NOISE_SECS}" \
  -filter_complex "[0:a][1:a]concat=n=2:v=0:a=1,aformat=sample_fmts=s16:channel_layouts=mono[a]" \
  -map "[a]" -ar 48000 -ac 1 -c:a alac -compression_level 0 \
  alac-mono48000-16-verbatim.m4a
raw alac-mono48000-16-verbatim.m4a alac-mono48000-16-verbatim.raw pcm_s16le s16le
frames alac-mono48000-16-verbatim.m4a alac-mono48000-16-verbatim.frames.bin alac-mono48000-16-verbatim

echo "== 16-bit mono 44100 with silence (ALAC SCE, zero-run blocks)"
# 30 ms tone, 50 ms of digital silence, 20 ms tone: the silence makes the
# encoder emit zero-run blocks, the run limited part of the Rice coder.
"$FFMPEG" -hide_banner -loglevel error -y \
  -f lavfi -i "aevalsrc='if(lt(t,0.03),0.4*sin(880*PI*t),if(lt(t,0.08),0,0.4*sin(1320*PI*t)))':s=44100:d=0.1" \
  -af "aformat=sample_fmts=s16:channel_layouts=mono" \
  -c:a alac alac-mono44100-16-silence.m4a
raw alac-mono44100-16-silence.m4a alac-mono44100-16-silence.raw pcm_s16le s16le
frames alac-mono44100-16-silence.m4a alac-mono44100-16-silence.frames.bin alac-mono44100-16-silence

echo
echo "== sizes"
du -ch *.m4a *.raw *.frames.bin | tail -1
