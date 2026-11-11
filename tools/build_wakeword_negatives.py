#!/usr/bin/env python3
"""Build CNEG0002 warmed negative-embedding corpora from licence-clean audio.

This script owns source-manifest validation, ffmpeg normalisation and
provenance. It deliberately does not implement the openWakeWord features a
second time: it converts every input to mono 16 kHz signed-16-bit little-endian
PCM WAV and invokes the native exporter in cmd/wakeword-negatives, which runs
the same oww frontend/embedding network as the live detector.

Manifest (JSON)::

    {
      "version": 1,
      "sources": [
        {"id": "librispeech-dev-clean",
         "url": "https://www.openslr.org/12/",
         "license": "CC-BY-4.0",
         "language": "en",
         "attribution": "Vassil Panayotov, Guoguo Chen, Daniel Povey, Sanjeev Khudanpur (2015)"}
      ],
      "streams": [
        {"id": "ls-dev-...", "source_id": "librispeech-dev-clean",
         "path": "audio/speaker/chapter.flac",
         "url": "https://www.openslr.org/resources/12/dev-clean.tar.gz",
         "source_ref": "dev-clean/19/198/19-198-0000.flac",
         "kind": "speech", "split": "train", "language": "en"}
      ]
    }

Only these licences are accepted: CC0-1.0, CC-BY-4.0, CC-BY-3.0,
Public-Domain. NC/ND/SA/unknown licences are rejected. CC-BY sources must carry
an attribution string. Paths may be absolute or relative to the manifest; they
are converted into private relative work copies. Each source file is one stream
and one whole-file split, so no embedding window can join two files or sessions.
Every stream is preceded by a 37120-sample (29 * 1280) preroll of real noise
from its own split; the native exporter chooses the clip, offset and quiet gain
deterministically from the stream ID. The produced file is a deterministic gzip
container of the CNEG0002 stream (raw CNEG0002 is also accepted on verify).
Exposure time is the sum of ``original_samples`` divided by 16000, i.e. the
preroll is not counted as new audio.

The output sidecar (<out>.provenance.json.gz by default) retains the source
manifest, actual per-file URLs, source checksums, language coverage, per-stream
preroll decisions, preroll timing and the native per-stream receipt. It is a
deterministic gzip JSON document (mtime 0, no filename) so the complete
licensing snapshot fits on the device; an explicit ``--provenance`` path without
a ``.gz`` suffix is written as plain JSON. ``--embed-previous-provenance``
embeds an earlier sidecar snapshot verbatim, once, from .gz or plain JSON.

Usage::

    python3 tools/build_wakeword_negatives.py \
        --manifest /tmp/capsulad-negative-corpus/manifest.json \
        --out /tmp/capsulad-negative-corpus/negatives.bin

    python3 tools/build_wakeword_negatives.py --verify negatives.bin

Requires ffmpeg on PATH (or --ffmpeg PATH) and Go for the native exporter (or a
prebuilt --exporter binary).
"""

from __future__ import annotations

import argparse
import gzip
import hashlib
import json
import os
import re
import shutil
import struct
import subprocess
import sys
import tempfile
import wave
from pathlib import Path

MAGIC = b"CNEG0002"
FORMAT_VERSION = 2
EMBEDDING_DIM = 96
HOP_SAMPLES = 1280
FIRST_EMBEDDING_SAMPLES = 12512
PREROLL_SAMPLES = 37120  # 29 * 1280; keeps the source mel/backbone phase intact
FEATURE_EXTRACTOR = "oww-google-v1"
# Source-manifest schema stays at version 1; only the binary container bumped.
MANIFEST_VERSION = 1
# Both the compressed file and the decompressed container are bounded.
MAX_NEGATIVES_CONTAINER = 32 << 20
MAX_NEGATIVES_HEADER = 4 << 20
# One stream may not exceed 64 MiB of float16 payload.
MAX_NEGATIVES_FRAMES = 349525
GZIP_MAGIC = b"\x1f\x8b"

KINDS = {"speech", "music", "noise"}
SPLITS = {"train", "tune", "test"}

# Canonical spellings written to the binary header.
ALLOWED_LICENSES = {
    "cc0-1.0": "CC0-1.0",
    "cc-by-4.0": "CC-BY-4.0",
    "cc-by-3.0": "CC-BY-3.0",
    "public-domain": "Public-Domain",
}
CC_BY = {"CC-BY-4.0", "CC-BY-3.0"}
BAD_LICENSE_RE = re.compile(r"(^|[-_ ])(nc|nd|sa)([-_ ]|$)", re.I)


class CorpusError(Exception):
    """A manifest, input or output failed validation."""


def fail(message: str) -> "None":
    raise CorpusError(message)


def canonical_license(raw: object) -> str:
    text = str(raw or "").strip()
    if not text:
        fail("missing license")
    low = text.lower().replace("_", "-").replace(" ", "-")
    if (
        "noncommercial" in low
        or "non-commercial" in low
        or "noderiv" in low
        or "no-deriv" in low
        or "sharealike" in low
        or "share-alike" in low
        or BAD_LICENSE_RE.search(low)
    ):
        fail(f"licence {text!r} is NC/ND/SA and is not allowed")
    canonical = ALLOWED_LICENSES.get(low)
    if canonical is None:
        fail(
            f"unknown licence {text!r} (allowed: CC0-1.0, CC-BY-4.0, "
            "CC-BY-3.0, Public-Domain)"
        )
    return canonical


def check_id(value: object, what: str) -> str:
    text = str(value or "")
    if not text:
        fail(f"{what}: empty id")
    if len(text.encode("utf-8")) > 200:
        fail(f"{what} {text!r}: id longer than 200 bytes")
    for ch in text:
        if ord(ch) < 0x20 or ord(ch) == 0x7F:
            fail(f"{what} {text!r}: id contains a control character")
    return text


def check_url(value: object, what: str, allow_file: bool = True) -> str:
    text = str(value or "").strip()
    if not text:
        fail(f"{what}: missing url")
    for ch in text:
        if ord(ch) < 0x20 or ord(ch) == 0x7F:
            fail(f"{what}: url contains a control character")
    return text


def load_json(path: Path) -> object:
    try:
        with path.open("r", encoding="utf-8") as fh:
            return json.load(fh)
    except FileNotFoundError:
        fail(f"manifest not found: {path}")
    except json.JSONDecodeError as exc:
        fail(f"manifest is not valid JSON: {path}: {exc}")


def validate_manifest(
    raw: object, base_dir: Path
) -> tuple[list[dict], list[dict], dict[str, dict]]:
    if not isinstance(raw, dict):
        fail("manifest must be a JSON object")
    version = raw.get("version", MANIFEST_VERSION)
    if version != MANIFEST_VERSION:
        fail(f"unsupported manifest version {version!r}")
    sources = raw.get("sources")
    streams = raw.get("streams")
    if not isinstance(sources, list) or not sources:
        fail("manifest sources must be a non-empty list")
    if not isinstance(streams, list) or not streams:
        fail("manifest streams must be a non-empty list")

    clean_sources: list[dict] = []
    by_id: dict[str, dict] = {}
    for idx, item in enumerate(sources):
        if not isinstance(item, dict):
            fail(f"sources[{idx}] must be an object")
        sid = check_id(item.get("id"), f"sources[{idx}]")
        if sid in by_id:
            fail(f"duplicate source id {sid!r}")
        url = check_url(item.get("url"), f"source {sid!r}")
        license_ = canonical_license(item.get("license"))
        attribution = str(item.get("attribution") or "").strip()
        if license_ in CC_BY and not attribution:
            fail(
                f"CC-BY source {sid!r} needs a non-empty attribution string "
                "for legal credit"
            )
        src = {"id": sid, "url": url, "license": license_}
        if attribution:
            src["attribution"] = attribution
        language = str(item.get("language") or "").strip()
        if language:
            src["language"] = language
        by_id[sid] = src
        clean_sources.append(src)

    clean_streams: list[dict] = []
    used_ids: set[str] = set()
    used_paths: dict[str, str] = {}
    for idx, item in enumerate(streams):
        if not isinstance(item, dict):
            fail(f"streams[{idx}] must be an object")
        stid = check_id(item.get("id"), f"streams[{idx}]")
        if stid in used_ids:
            fail(f"duplicate stream id {stid!r}")
        used_ids.add(stid)
        source_id = str(item.get("source_id") or "")
        if source_id not in by_id:
            fail(f"stream {stid!r}: unknown source_id {source_id!r}")
        kind = str(item.get("kind") or "")
        if kind not in KINDS:
            fail(f"stream {stid!r}: invalid kind {kind!r} (speech|music|noise)")
        split = str(item.get("split") or "")
        if split not in SPLITS:
            fail(f"stream {stid!r}: invalid split {split!r} (train|tune|test)")
        raw_path = str(item.get("path") or "").strip()
        if not raw_path:
            fail(f"stream {stid!r}: missing path")
        if "\x00" in raw_path:
            fail(f"stream {stid!r}: path contains NUL")
        path = Path(raw_path).expanduser()
        if not path.is_absolute():
            path = base_dir / path
        try:
            path = path.resolve(strict=True)
        except FileNotFoundError:
            fail(f"stream {stid!r}: input not found: {path}")
        except OSError as exc:
            fail(f"stream {stid!r}: cannot resolve {path}: {exc}")
        if not path.is_file():
            fail(f"stream {stid!r}: input is not a regular file: {path}")
        key = str(path)
        if key in used_paths:
            fail(
                f"stream {stid!r} reuses file {path} already used by "
                f"{used_paths[key]!r} (whole-file holdouts)"
            )
        used_paths[key] = stid

        stream = {
            "id": stid,
            "source_id": source_id,
            "path": raw_path,
            "kind": kind,
            "split": split,
            "resolved_input": path,
        }
        for optional in ("url", "source_ref", "sha256", "language"):
            value = item.get(optional)
            if value is not None and str(value).strip():
                stream[optional] = str(value).strip()
        if "url" in stream:
            check_url(stream["url"], f"stream {stid!r}", allow_file=True)
        clean_streams.append(stream)

    present_splits = {s["split"] for s in clean_streams}
    for split in SPLITS:
        if split not in present_splits:
            fail(f"manifest has no {split} streams; train/tune/test are all required")
    unused = set(by_id) - {s["source_id"] for s in clean_streams}
    if unused:
        fail(f"sources without streams: {', '.join(sorted(unused))}")
    return clean_sources, clean_streams, by_id


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def safe_stem(text: str) -> str:
    stem = re.sub(r"[^A-Za-z0-9._-]+", "_", text).strip("._-")
    return (stem or "stream")[:80]


def convert_one(src: Path, dst: Path, ffmpeg: str, copy_only: bool) -> int:
    dst.parent.mkdir(parents=True, exist_ok=True)
    if copy_only:
        try:
            shutil.copyfile(src, dst)
        except OSError as exc:
            fail(f"copy {src} -> {dst}: {exc}")
    else:
        cmd = [
            ffmpeg,
            "-nostdin",
            "-hide_banner",
            "-loglevel",
            "error",
            "-y",
            "-i",
            str(src),
            "-map_metadata",
            "-1",
            "-vn",
            "-ac",
            "1",
            "-ar",
            "16000",
            "-c:a",
            "pcm_s16le",
            "-f",
            "wav",
            str(dst),
        ]
        try:
            proc = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        except FileNotFoundError:
            fail(f"ffmpeg not found: {ffmpeg!r} (use --ffmpeg PATH)")
        if proc.returncode != 0:
            detail = proc.stderr.decode("utf-8", "replace").strip()
            fail(f"ffmpeg failed for {src}:\n{detail}")

    try:
        with wave.open(str(dst), "rb") as wav:
            channels = wav.getnchannels()
            rate = wav.getframerate()
            width = wav.getsampwidth()
            samples = wav.getnframes()
    except (wave.Error, EOFError) as exc:
        fail(f"{dst} is not a readable PCM WAV: {exc}")
    if channels != 1 or rate != 16000 or width != 2:
        fail(
            f"{dst}: converted layout is {rate} Hz, {channels} ch, "
            f"{width * 8}-bit; need 16000/1/16"
        )
    if samples <= 0:
        fail(f"{dst}: no audio samples")
    return samples


def expected_frames(original_samples: int) -> int:
    """Frames produced by the warmed stream.

    ``original_samples`` is the source body without the preroll; the formula
    therefore adds PREROLL_SAMPLES before the first-embedding offset.
    """
    total = PREROLL_SAMPLES + int(original_samples)
    if total < FIRST_EMBEDDING_SAMPLES:
        return 0
    return (total - FIRST_EMBEDDING_SAMPLES) // HOP_SAMPLES + 1


def build_resolved_manifest(
    raw_manifest: dict,
    sources: list[dict],
    streams: list[dict],
    work: Path,
    ffmpeg: str,
    copy_only: bool,
) -> tuple[dict, list[dict], list[dict]]:
    audio_dir = work / "audio"
    audio_dir.mkdir(parents=True, exist_ok=True)
    resolved_streams: list[dict] = []
    receipts: list[dict] = []
    skipped: list[dict] = []
    seen_hashes: dict[str, str] = {}
    for index, stream in enumerate(streams):
        src: Path = stream["resolved_input"]
        source_hash = sha256_file(src)
        dst = audio_dir / f"{index:04d}-{safe_stem(stream['id'])}.wav"
        samples = convert_one(src, dst, ffmpeg, copy_only)
        frames = expected_frames(samples)
        if frames <= 0:
            print(
                f"build-wakeword-negatives: skipping {stream['id']!r}: "
                f"{samples} samples, need at least {FIRST_EMBEDDING_SAMPLES}",
                file=sys.stderr,
            )
            skipped.append(
                {
                    "id": stream["id"],
                    "source_id": stream["source_id"],
                    "source_path": str(src),
                    "source_sha256": source_hash,
                    "original_samples": samples,
                    "kind": stream["kind"],
                    "split": stream["split"],
                    "reason": "too_short",
                }
            )
            continue
        if frames > MAX_NEGATIVES_FRAMES:
            fail(
                f"stream {stream['id']!r}: {frames} frames exceeds limit "
                f"{MAX_NEGATIVES_FRAMES} (64 MiB payload)"
            )
        if source_hash in seen_hashes:
            fail(
                f"stream {stream['id']!r} duplicates audio content already used by "
                f"{seen_hashes[source_hash]!r}; refusing to fake repeats as hours"
            )
        seen_hashes[source_hash] = stream["id"]
        out_stream = {
            "id": stream["id"],
            "source_id": stream["source_id"],
            "path": str(dst.relative_to(work)),
            "kind": stream["kind"],
            "split": stream["split"],
            "samples": samples,
        }
        for key in ("url", "source_ref", "sha256", "language"):
            if key in stream:
                out_stream[key] = stream[key]
        resolved_streams.append(out_stream)
        receipts.append(
            {
                "id": stream["id"],
                "source_id": stream["source_id"],
                "source_path": str(src),
                "source_sha256": source_hash,
                "converted_path": str(dst),
                "converted_sha256": sha256_file(dst),
                "original_samples": samples,
                "preroll_samples": PREROLL_SAMPLES,
                "frames": frames,
                "kind": stream["kind"],
                "split": stream["split"],
                "language": stream.get("language", ""),
                "url": stream.get("url", ""),
                "source_ref": stream.get("source_ref", ""),
            }
        )
        print(
            f"build-wakeword-negatives: converted {stream['kind']}/"
            f"{stream['split']} {stream['id']} ({samples / 16000:.1f}s, "
            f"{frames} frames)",
            file=sys.stderr,
        )
    if not resolved_streams:
        fail("no stream produced at least one embedding")
    present_splits = {s["split"] for s in resolved_streams}
    for split in SPLITS:
        if split not in present_splits:
            fail(f"no usable {split} streams remain after conversion")
    resolved = {
        "version": MANIFEST_VERSION,
        "sources": sources,
        "streams": resolved_streams,
    }
    return resolved, receipts, skipped


def marshal_checked(header: object, payload_len: int) -> bytes:
    if not isinstance(header, dict):
        fail("header is not a JSON object")
    required = {
        "version": FORMAT_VERSION,
        "embedding_dim": EMBEDDING_DIM,
        "hop_samples": HOP_SAMPLES,
        "feature_extractor": FEATURE_EXTRACTOR,
        "first_embedding_samples": FIRST_EMBEDDING_SAMPLES,
        "preroll_samples": PREROLL_SAMPLES,
    }
    for key, want in required.items():
        got = header.get(key)
        if got != want:
            fail(f"header {key} is {got!r}, expected {want!r}")
    sources = header.get("sources")
    streams = header.get("streams")
    if not isinstance(sources, list) or not sources:
        fail("header sources must be a non-empty list")
    if not isinstance(streams, list) or not streams:
        fail("header streams must be a non-empty list")

    source_ids: set[str] = set()
    for idx, src in enumerate(sources):
        if not isinstance(src, dict):
            fail(f"header sources[{idx}] is not an object")
        sid = check_id(src.get("id"), f"header sources[{idx}]")
        if sid in source_ids:
            fail(f"header duplicate source id {sid!r}")
        source_ids.add(sid)
        check_url(src.get("url"), f"header source {sid!r}")
        license_ = canonical_license(src.get("license"))
        attribution = str(src.get("attribution") or "").strip()
        if license_ in CC_BY and not attribution:
            fail(f"header CC-BY source {sid!r} is missing attribution")
        if "attribution" in src and not isinstance(src["attribution"], str):
            fail(f"header source {sid!r}: attribution must be a string")

    stream_ids: set[str] = set()
    for idx, stream in enumerate(streams):
        if not isinstance(stream, dict):
            fail(f"header streams[{idx}] is not an object")
        stid = check_id(stream.get("id"), f"header streams[{idx}]")
        if stid in stream_ids:
            fail(f"header duplicate stream id {stid!r}")
        stream_ids.add(stid)
        if stream.get("source_id") not in source_ids:
            fail(f"header stream {stid!r}: unknown source_id")
        if stream.get("kind") not in KINDS:
            fail(f"header stream {stid!r}: invalid kind")
        if stream.get("split") not in SPLITS:
            fail(f"header stream {stid!r}: invalid split")
        original = stream.get("original_samples")
        if (
            not isinstance(original, int)
            or isinstance(original, bool)
            or original <= 0
        ):
            fail(
                f"header stream {stid!r}: original_samples must be a positive integer"
            )
        frames = stream.get("frames")
        if not isinstance(frames, int) or isinstance(frames, bool):
            fail(f"header stream {stid!r}: frames must be an integer")
        if frames <= 0 or frames > MAX_NEGATIVES_FRAMES:
            fail(
                f"header stream {stid!r}: frames {frames} outside 1..{MAX_NEGATIVES_FRAMES}"
            )
        want_frames = expected_frames(original)
        if frames != want_frames:
            fail(
                f"header stream {stid!r}: {frames} frames, expected {want_frames} "
                f"for {PREROLL_SAMPLES} preroll + {original} original samples"
            )

    present_splits = {str(stream.get("split", "")) for stream in streams}
    for split in SPLITS:
        if split not in present_splits:
            fail(f"header has no {split} streams")

    # Check exact payload size by subtraction so a malformed frame count cannot
    # be masked by integer overflow in the verifier.
    remaining = payload_len
    for stream in streams:
        need = stream["frames"] * EMBEDDING_DIM * 2
        if need > remaining:
            fail(
                f"payload is short for stream {stream.get('id')!r}: "
                f"need {need} bytes, have {remaining}"
            )
        remaining -= need
    if remaining != 0:
        fail(f"payload has {remaining} excess bytes after the declared streams")
    return json.dumps(header, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


def read_container(path: Path) -> tuple[bytes, bool]:
    """Return the logical CNEG0002 bytes, transparently inflating gzip.

    Both the compressed file and the decompressed container are bounded; gzip
    CRC errors, truncation and trailing bytes after the member are hard errors.
    """
    try:
        size = path.stat().st_size
    except OSError as exc:
        fail(f"cannot stat {path}: {exc}")
    if size > MAX_NEGATIVES_CONTAINER:
        fail(
            f"{path}: file is {size} bytes, exceeding the "
            f"{MAX_NEGATIVES_CONTAINER}-byte bound"
        )
    try:
        with path.open("rb") as fh:
            head = fh.read(2)
            fh.seek(0)
            if head == GZIP_MAGIC:
                return _inflate_bounded(fh, path), True
            data = fh.read(MAX_NEGATIVES_CONTAINER + 1)
    except OSError as exc:
        fail(f"cannot read {path}: {exc}")
    if len(data) > MAX_NEGATIVES_CONTAINER:
        fail(
            f"{path}: logical container exceeds the "
            f"{MAX_NEGATIVES_CONTAINER}-byte bound"
        )
    return data, False


def _inflate_bounded(fh, path: Path) -> bytes:
    import zlib

    dec = zlib.decompressobj(16 + zlib.MAX_WBITS)
    out = bytearray()
    try:
        while True:
            chunk = fh.read(1 << 20)
            if not chunk:
                break
            while chunk:
                room = MAX_NEGATIVES_CONTAINER + 1 - len(out)
                if room <= 0:
                    fail(
                        f"{path}: decompressed container exceeds the "
                        f"{MAX_NEGATIVES_CONTAINER}-byte bound"
                    )
                before = chunk
                piece = dec.decompress(chunk, room)
                out.extend(piece)
                chunk = dec.unconsumed_tail
                if not piece and chunk == before:
                    fail(f"{path}: gzip stream made no progress")
                if len(out) > MAX_NEGATIVES_CONTAINER:
                    fail(
                        f"{path}: decompressed container exceeds the "
                        f"{MAX_NEGATIVES_CONTAINER}-byte bound"
                    )
                if not chunk:
                    break
        out.extend(dec.flush())
    except zlib.error as exc:
        fail(f"{path}: invalid gzip stream: {exc}")
    if len(out) > MAX_NEGATIVES_CONTAINER:
        fail(
            f"{path}: decompressed container exceeds the "
            f"{MAX_NEGATIVES_CONTAINER}-byte bound"
        )
    if not dec.eof:
        fail(f"{path}: truncated gzip stream")
    if dec.unused_data:
        fail(f"{path}: {len(dec.unused_data)} trailing bytes after the gzip member")
    return bytes(out)


def read_cneg(path: Path) -> tuple[dict, bytes, bytes, dict]:
    data, compressed = read_container(path)
    if len(data) < 12:
        fail(f"{path}: file is too short for {MAGIC.decode()}")
    if data[:8] != MAGIC:
        fail(f"{path}: bad magic {data[:8]!r}, expected {MAGIC!r}")
    header_len = struct.unpack_from("<I", data, 8)[0]
    if header_len == 0 or header_len > MAX_NEGATIVES_HEADER:
        fail(f"{path}: invalid header length {header_len}")
    if 12 + header_len > len(data):
        fail(f"{path}: truncated header (declared {header_len} bytes)")
    header_bytes = data[12 : 12 + header_len]
    try:
        header = json.loads(header_bytes.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(f"{path}: invalid header JSON: {exc}")
    payload = data[12 + header_len :]
    meta = {
        "compressed": compressed,
        "file_bytes": path.stat().st_size,
        "logical_bytes": len(data),
    }
    return header, header_bytes, payload, meta


def verify_cneg(path: Path, resolved_manifest: dict | None = None) -> dict:
    header, header_bytes, payload, meta = read_cneg(path)
    marshal_checked(header, len(payload))

    # Reject NaN/Infinity half-floats: exponent bits all ones.
    for (bits,) in struct.iter_unpack("<H", payload):
        if (bits & 0x7C00) == 0x7C00:
            fail(f"{path}: payload contains a non-finite float16 value")

    if resolved_manifest is not None:
        want_sources = resolved_manifest.get("sources", [])
        got_sources = header["sources"]
        if len(want_sources) != len(got_sources):
            fail("header/source manifest source count mismatch")
        for want, got in zip(want_sources, got_sources):
            for key in ("id", "url"):
                if str(want.get(key, "")).strip() != str(got.get(key, "")).strip():
                    fail(f"header source {want.get('id')!r}: {key} mismatch")
            if str(want.get("license", "")).strip().lower() != str(
                got.get("license", "")
            ).strip().lower():
                fail(f"header source {want.get('id')!r}: license mismatch")
            if str(want.get("attribution") or "") != str(got.get("attribution") or ""):
                fail(f"header source {want.get('id')!r}: attribution mismatch")
        want_streams = resolved_manifest.get("streams", [])
        got_streams = header["streams"]
        if len(want_streams) != len(got_streams):
            fail("header/stream manifest stream count mismatch")
        for want, got in zip(want_streams, got_streams):
            for key in ("id", "source_id", "kind", "split"):
                if str(want.get(key, "")) != str(got.get(key, "")):
                    fail(f"header stream {want.get('id')!r}: {key} mismatch")
            if "samples" in want:
                want_frames = expected_frames(int(want["samples"]))
                if got.get("frames") != want_frames:
                    fail(
                        f"header stream {want['id']!r}: {got.get('frames')} frames, "
                        f"expected {want_frames} from {want['samples']} samples"
                    )

    by_split: dict[str, int] = {}
    by_kind: dict[str, int] = {}
    total_frames = 0
    total_original = 0
    for stream in header["streams"]:
        frames = stream["frames"]
        total_frames += frames
        total_original += int(stream["original_samples"])
        by_split[stream["split"]] = by_split.get(stream["split"], 0) + frames
        by_kind[stream["kind"]] = by_kind.get(stream["kind"], 0) + frames
    return {
        "file": str(path),
        "bytes": meta["file_bytes"],
        "compressed": meta["compressed"],
        "logical_bytes": meta["logical_bytes"],
        "header_bytes": len(header_bytes),
        "preroll_samples": header["preroll_samples"],
        "sources": len(header["sources"]),
        "streams": len(header["streams"]),
        "frames": total_frames,
        "original_samples": total_original,
        # Exposure is raw source audio only; the borrowed preroll is not new.
        "duration_seconds": round(total_original / 16000, 3),
        "preroll_seconds": round(
            header["preroll_samples"] * len(header["streams"]) / 16000, 3
        ),
        "payload_bytes": len(payload),
        "by_split": by_split,
        "by_kind": by_kind,
        "feature_extractor": header["feature_extractor"],
        "first_embedding_samples": header["first_embedding_samples"],
        "embedding_dim": header["embedding_dim"],
        "hop_samples": header["hop_samples"],
    }


def run_exporter(
    manifest: Path,
    out: Path,
    provenance: Path,
    exporter: str | None,
    chunk_samples: int,
) -> None:
    if exporter:
        exe = Path(exporter).expanduser()
        if not exe.is_absolute():
            exe = Path.cwd() / exe
        if not exe.exists():
            fail(f"exporter binary not found: {exe}")
        cmd = [str(exe)]
        cwd = None
    else:
        repo = Path(__file__).resolve().parent.parent
        cmd = ["go", "run", "./cmd/wakeword-negatives"]
        cwd = repo
    cmd += [
        "-manifest",
        str(manifest),
        "-out",
        str(out),
        "-provenance",
        str(provenance),
        "-chunk-samples",
        str(chunk_samples),
    ]
    print("build-wakeword-negatives: exec " + " ".join(cmd), file=sys.stderr)
    try:
        proc = subprocess.run(cmd, cwd=str(cwd) if cwd else None)
    except FileNotFoundError:
        fail("Go toolchain not found; use --exporter with a prebuilt binary")
    if proc.returncode != 0:
        fail(f"native exporter failed with exit code {proc.returncode}")


def read_json_maybe_gzip(path: Path) -> object:
    """Load JSON that may be a deterministic gzip sidecar or plain text."""
    try:
        with path.open("rb") as fh:
            head = fh.read(2)
    except OSError as exc:
        fail(f"cannot read {path}: {exc}")
    try:
        if head == GZIP_MAGIC:
            with gzip.open(path, "rt", encoding="utf-8") as fh:
                return json.load(fh)
        with path.open("r", encoding="utf-8") as fh:
            return json.load(fh)
    except (OSError, EOFError, json.JSONDecodeError) as exc:
        fail(f"cannot parse JSON sidecar {path}: {exc}")


def write_json_atomic(path: Path, value: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(prefix=".cneg-prov-", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(value, fh, ensure_ascii=False, indent=2)
            fh.write("\n")
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp_name, path)
    except Exception:
        try:
            os.unlink(tmp_name)
        except OSError:
            pass
        raise


def write_json_artifact(path: Path, value: object) -> None:
    """Atomically write provenance, gzip-compressed when the path ends in .gz.

    The gzip header is deterministic: filename suppressed and mtime zero, so
    identical provenance always produces identical bytes (standard library).
    """
    payload = (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(prefix=".cneg-prov-", dir=str(path.parent))
    try:
        with os.fdopen(fd, "wb") as raw:
            if path.suffix == ".gz":
                with gzip.GzipFile(
                    filename="", mode="wb", compresslevel=9, fileobj=raw, mtime=0
                ) as gz:
                    gz.write(payload)
            else:
                raw.write(payload)
            raw.flush()
            os.fsync(raw.fileno())
        os.replace(tmp_name, path)
    except Exception:
        try:
            os.unlink(tmp_name)
        except OSError:
            pass
        raise


def write_provenance(
    out: Path,
    provenance: Path,
    raw_manifest: dict,
    resolved_manifest: dict,
    receipts: list[dict],
    skipped: list[dict],
    native_provenance: Path,
    previous_provenance: Path | None = None,
) -> None:
    native = read_json_maybe_gzip(native_provenance)
    source_language = {
        str(src.get("id")): str(src.get("language") or "")
        for src in resolved_manifest.get("sources", [])
    }
    coverage: dict[str, dict[str, float]] = {}
    by_split: dict[str, int] = {}
    by_kind: dict[str, int] = {}
    total_frames = 0
    total_seconds = 0.0
    for rec in receipts:
        source_id = str(rec.get("source_id", ""))
        language = str(rec.get("language") or source_language.get(source_id) or "")
        if not language:
            low_id = source_id.lower()
            if rec.get("kind") in ("music", "noise"):
                language = "zxx"
            elif "librispeech" in low_id or "openslr" in low_id:
                language = "en"
            elif "fleurs" in low_id:
                language = "ru"
            else:
                language = "unspecified"
        seconds = float(rec.get("original_samples", 0)) / 16000.0
        frames = int(rec.get("frames", 0))
        entry = coverage.setdefault(
            language, {"streams": 0, "seconds": 0.0, "frames": 0}
        )
        entry["streams"] += 1
        entry["seconds"] += seconds
        entry["frames"] += frames
        total_frames += frames
        total_seconds += seconds
        by_split[str(rec.get("split", ""))] = by_split.get(
            str(rec.get("split", "")), 0
        ) + frames
        by_kind[str(rec.get("kind", ""))] = by_kind.get(
            str(rec.get("kind", "")), 0
        ) + frames
    for entry in coverage.values():
        entry["seconds"] = round(entry["seconds"], 3)
    prev_raw = None
    prev_path = None
    prev_sha = None
    if previous_provenance is not None:
        prev_path = str(previous_provenance)
        prev_sha = sha256_file(previous_provenance)
        prev_raw = read_json_maybe_gzip(previous_provenance)
    doc = {
        "format": "CNEG0002",
        "builder": "tools/build_wakeword_negatives.py",
        "output": str(out),
        "output_bytes": out.stat().st_size,
        "output_logical_bytes": native.get("output_logical_bytes"),
        "output_sha256": sha256_file(out),
        "preroll_samples": PREROLL_SAMPLES,
        "source_manifest": raw_manifest,
        "resolved_manifest": resolved_manifest,
        "streams": receipts,
        "skipped_streams": skipped,
        "language_coverage": coverage,
        "totals": {
            "streams": len(receipts),
            "skipped_streams": len(skipped),
            "frames": total_frames,
            "original_samples": int(round(total_seconds * 16000)),
            "exposure_seconds": round(total_seconds, 3),
            "preroll_samples_total": PREROLL_SAMPLES * len(receipts),
            "by_split_frames": by_split,
            "by_kind_frames": by_kind,
        },
        "native": native,
    }
    if prev_raw is not None:
        # Embedded verbatim once, so the new sidecar remains self-contained
        # without recursively duplicating older layers.
        doc["previous_provenance"] = {
            "path": prev_path,
            "sha256": prev_sha,
            "document": prev_raw,
        }
    write_json_artifact(provenance, doc)


def build(args: argparse.Namespace) -> dict:
    manifest_path = Path(args.manifest).expanduser().resolve()
    out = Path(args.out).expanduser().resolve()
    provenance = (
        Path(args.provenance).expanduser().resolve()
        if args.provenance
        else Path(str(out) + ".provenance.json.gz")
    )
    if out.exists() and not args.force:
        fail(f"output exists: {out} (use --force to replace)")
    if provenance == out:
        fail("--provenance must not be the output file")
    out.parent.mkdir(parents=True, exist_ok=True)
    provenance.parent.mkdir(parents=True, exist_ok=True)

    raw = load_json(manifest_path)
    sources, streams, _ = validate_manifest(raw, manifest_path.parent)

    work_arg = args.work
    if work_arg:
        work = Path(work_arg).expanduser().resolve()
        work.mkdir(parents=True, exist_ok=True)
        temp_work = False
    else:
        work = Path(tempfile.mkdtemp(prefix="wakeword-negatives-"))
        temp_work = True

    try:
        resolved, receipts, skipped = build_resolved_manifest(
            raw, sources, streams, work, args.ffmpeg, args.no_convert
        )
        resolved_path = work / "manifest.resolved.json"
        write_json_atomic(resolved_path, resolved)
        native_prov = work / "native.provenance.json"
        run_exporter(
            resolved_path,
            out,
            native_prov,
            args.exporter,
            args.chunk_samples,
        )
        if not out.is_file():
            fail(f"native exporter did not create {out}")
        if args.no_verify:
            summary = {"file": str(out), "bytes": out.stat().st_size}
        else:
            summary = verify_cneg(out, resolved)
        write_provenance(
            out,
            provenance,
            raw,
            resolved,
            receipts,
            skipped,
            native_prov,
            Path(args.embed_previous_provenance).expanduser().resolve()
            if args.embed_previous_provenance
            else None,
        )
        summary["provenance"] = str(provenance)
        summary["skipped_streams"] = len(skipped)
        return summary
    finally:
        if temp_work and not args.keep_work:
            try:
                out.relative_to(work)
                print(
                    f"build-wakeword-negatives: keeping work dir {work} because "
                    "it contains the output",
                    file=sys.stderr,
                )
            except ValueError:
                shutil.rmtree(work, ignore_errors=True)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Build or verify a CNEG0002 warmed negative-embedding corpus.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "Examples:\n"
            "  build_wakeword_negatives.py --manifest m.json --out negatives.bin\n"
            "  build_wakeword_negatives.py --verify negatives.bin\n"
        ),
    )
    parser.add_argument("--manifest", help="source manifest JSON")
    parser.add_argument("--out", help="CNEG0002 output path (gzip container)")
    parser.add_argument(
        "--provenance",
        help="provenance path (default: <out>.provenance.json.gz, deterministic gzip; "
        "a path without .gz is written as plain JSON)",
    )
    parser.add_argument(
        "--verify",
        metavar="CNEG",
        help="verify an existing CNEG0002 file (gzip or raw) instead of building",
    )
    parser.add_argument("--ffmpeg", default="ffmpeg", help="ffmpeg executable")
    parser.add_argument(
        "--exporter",
        help="prebuilt wakeword-negatives binary (default: go run ./cmd/wakeword-negatives)",
    )
    parser.add_argument(
        "--work",
        help="conversion/work directory (default: a temporary directory)",
    )
    parser.add_argument("--keep-work", action="store_true", help="keep the temp work directory")
    parser.add_argument(
        "--no-convert",
        action="store_true",
        help="inputs are already mono 16 kHz PCM WAV; copy instead of ffmpeg",
    )
    parser.add_argument(
        "--no-verify",
        action="store_true",
        help="skip the post-build structural verification",
    )
    parser.add_argument(
        "--embed-previous-provenance",
        help="embed an earlier sidecar JSON verbatim (once) in the new sidecar",
    )
    parser.add_argument("--force", action="store_true", help="replace an existing output")
    parser.add_argument(
        "--chunk-samples",
        type=int,
        default=16000,
        help="samples pushed through the native frontend per chunk",
    )
    parser.add_argument("--json", action="store_true", help="print the summary as JSON")
    args = parser.parse_args(argv)

    try:
        if args.verify:
            summary = verify_cneg(Path(args.verify).expanduser().resolve())
        else:
            if not args.manifest or not args.out:
                parser.error("--manifest and --out are required unless --verify")
            summary = build(args)
    except CorpusError as exc:
        print(f"build-wakeword-negatives: error: {exc}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("build-wakeword-negatives: interrupted", file=sys.stderr)
        return 130
    if args.json:
        print(json.dumps(summary, ensure_ascii=False, indent=2))
    else:
        print(
            "build-wakeword-negatives: "
            f"{summary.get('streams', '?')} streams, "
            f"{summary.get('frames', '?')} frames, "
            f"{summary.get('duration_seconds', '?')} s exposure, "
            f"{summary.get('bytes', '?')} bytes"
        )
        if "by_kind" in summary:
            print(f"  by_kind={summary['by_kind']}")
        if "by_split" in summary:
            print(f"  by_split={summary['by_split']}")
        if "provenance" in summary:
            print(f"  provenance={summary['provenance']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
