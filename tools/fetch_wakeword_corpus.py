#!/usr/bin/env python3
"""Fetch licence-clean wake-word negative corpora and write a source manifest.

The recipe was verified against the upstream licence statements:

* LibriSpeech (OpenSLR SLR12): CC-BY-4.0.
  https://www.openslr.org/12/  (md5sum.txt is authoritative)
* Microsoft MS-SNSD ``noise_train``: the Freesound clips are CC0-1.0; the
  DEMAND environment files are CC-BY-SA-3.0 and are deliberately excluded.
  https://github.com/microsoft/MS-SNSD
* Kevin MacLeod / Incompetech: CC-BY-4.0.
  https://incompetech.com/music/royalty-free/licenses/
  Catalog: https://incompetech.com/music/royalty-free/pieces.json

No NC/ND corpus is downloaded. This script writes only source manifests and
audio downloads; tools/build_wakeword_negatives.py validates the manifest,
runs ffmpeg and invokes the native oww exporter.

Commands::

    # Print exact URLs, licences, attributions and split recipe.
    python3 tools/fetch_wakeword_corpus.py plan

    # Download the already-selected ~16 min music and ~16 min noise.
    python3 tools/fetch_wakeword_corpus.py fetch \
        --out /tmp/capsulad-negative-corpus

    # Optional: build a manifest from extracted LibriSpeech FLACs.
    python3 tools/fetch_wakeword_corpus.py speech-manifest \
        --root /tmp/LibriSpeech --out /tmp/capsulad-negative-corpus/manifest-speech.json \
        --seconds-per-speaker 135

    # Combine manifests before building.
    python3 tools/fetch_wakeword_corpus.py merge \
        --input /tmp/capsulad-negative-corpus/manifest-noise-music.json \
        --input /tmp/capsulad-negative-corpus/manifest-speech.json \
        --out /tmp/capsulad-negative-corpus/manifest.json

    # Build the binary (the actual command to run after frontend review).
    python3 tools/build_wakeword_negatives.py \
        --manifest /tmp/capsulad-negative-corpus/manifest.json \
        --out /tmp/capsulad-negative-corpus/negatives.bin
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import urllib.parse
import urllib.request
from pathlib import Path

USER_AGENT = "capsulad-wakeword-corpus/1"

LIBRISPEECH_ATTRIBUTION = (
    "Vassil Panayotov, Guoguo Chen, Daniel Povey, Sanjeev Khudanpur (2015), "
    "LibriSpeech: an ASR corpus based on public domain audio books, ICASSP 2015"
)
INCOMPETECH_ATTRIBUTION = (
    "Kevin MacLeod (incompetech.com), Creative Commons Attribution 4.0, "
    "http://creativecommons.org/licenses/by/4.0/"
)
MS_SNSD_ATTRIBUTION = (
    "Microsoft MS-SNSD noise_train Freesound subset (individual clips CC0-1.0); "
    "DEMAND CC-BY-SA-3.0 files excluded"
)

SOURCES = {
    "librispeech": {
        "id": "librispeech",
        "url": "https://www.openslr.org/12/",
        "license": "CC-BY-4.0",
        "attribution": LIBRISPEECH_ATTRIBUTION,
        "language": "en",
    },
    "incompetech": {
        "id": "incompetech",
        "url": "https://incompetech.com/music/royalty-free/",
        "license": "CC-BY-4.0",
        "attribution": INCOMPETECH_ATTRIBUTION,
    },
    "ms-snsd-freesound": {
        "id": "ms-snsd-freesound",
        "url": "https://github.com/microsoft/MS-SNSD",
        "license": "CC0-1.0",
        "attribution": MS_SNSD_ATTRIBUTION,
    },
    "fleurs-ru": {
        "id": "fleurs-ru",
        "url": "https://huggingface.co/datasets/google/fleurs",
        "license": "CC-BY-4.0",
        "attribution": (
            "Google FLEURS (Conneau et al., 2022), CC-BY-4.0; "
            "https://huggingface.co/datasets/google/fleurs"
        ),
        "language": "ru",
    },
}

FLEURS_RU_DEV = {
    "primary": "https://huggingface.co/datasets/google/fleurs",
    "url": "https://huggingface.co/datasets/google/fleurs/resolve/main/data/ru_ru/audio/dev.tar.gz",
    "bytes": 187377526,
    "sha256": "4dc533cd0f312f749bd6d0fe968f3183288d938e07ed97dc141d9f10570457f9",
    "metadata": "data/ru_ru/dev.tsv",
    "license": "CC-BY-4.0",
    "language": "ru",
}

LIBRISPEECH_ARCHIVES = {
    "dev-clean": {
        "url": "https://openslr.trmal.net/resources/12/dev-clean.tar.gz",
        "mirror": "https://openslr.elda.org/resources/12/dev-clean.tar.gz",
        "md5": "42e2234ba48799c1f50f24a7926300a1",
        "size": "337M",
    },
    "test-clean": {
        "url": "https://openslr.trmal.net/resources/12/test-clean.tar.gz",
        "mirror": "https://openslr.elda.org/resources/12/test-clean.tar.gz",
        "md5": "32fa31d27d2e1cad72775fee3f4849a9",
        "size": "346M",
    },
    "train-clean-100": {
        "url": "https://openslr.trmal.net/resources/12/train-clean-100.tar.gz",
        "mirror": "https://openslr.elda.org/resources/12/train-clean-100.tar.gz",
        "md5": "2a93770f6d5c6c964bc36631d331a522",
        "size": "6.3G",
    },
}

# Six distinct Kevin MacLeod tracks, ~16.4 min total. Whole-file splits.
MUSIC_TRACKS = [
    {"file": "Carefree.mp3", "seconds": 205, "split": "train"},
    {"file": "Local Forecast - Elevator.mp3", "seconds": 189, "split": "train"},
    {"file": "Cipher2.mp3", "seconds": 231, "split": "tune"},
    {"file": "Monkeys Spinning Monkeys.mp3", "seconds": 125, "split": "tune"},
    {"file": "Sneaky Snitch.mp3", "seconds": 137, "split": "test"},
    {"file": "Investigations.mp3", "seconds": 94, "split": "test"},
]
MUSIC_URL_BASE = "https://incompetech.com/music/royalty-free/mp3-royaltyfree/"

# MS-SNSD "noise_train" files that are actually DEMAND (CC-BY-SA-3.0). They are
# 5 min / 9,600,172-byte files. Everything else in noise_train is Freesound CC0.
DEMAND_FILES = {
    "Bus_1.wav",
    "CafeTeria_1.wav",
    "Cafe_1.wav",
    "Car_1.wav",
    "Field_1.wav",
    "Hallway_1.wav",
    "Kitchen_1.wav",
    "LivingRoom_1.wav",
    "Metro_1.wav",
    "Office_1.wav",
    "Park_1.wav",
    "Restaurant_1.wav",
    "Square_1.wav",
    "Station_1.wav",
    "Traffic_1.wav",
    "Washing_1.wav",
}
DEMAND_SIZE = 9_600_172
NOISE_URL_BASE = "https://raw.githubusercontent.com/microsoft/MS-SNSD/master/noise_train/"

# The 19 CC0 Freesound clips already selected for the capsule (about 16 min).
# The split keeps every clip whole and gives each split several noise types.
NOISE_FILES = {
    "train": [
        "AirConditioner_1.wav",
        "AirConditioner_2.wav",
        "AirConditioner_3.wav",
        "Typing_1.wav",
        "Typing_2.wav",
        "ShuttingDoor_1.wav",
        "ShuttingDoor_2.wav",
        "VacuumCleaner_1.wav",
        "VacuumCleaner_4.wav",
        "WasherDryer_1.wav",
    ],
    "tune": [
        "AirConditioner_4.wav",
        "Typing_3.wav",
        "ShuttingDoor_3.wav",
        "VacuumCleaner_2.wav",
        "WasherDryer_3.wav",
    ],
    "test": [
        "Typing_4.wav",
        "ShuttingDoor_4.wav",
        "VacuumCleaner_3.wav",
        "WasherDryer_2.wav",
    ],
}


def fail(message: str) -> "None":
    raise SystemExit(f"fetch-wakeword-corpus: error: {message}")


def request(url: str) -> urllib.request.Request:
    return urllib.request.Request(url, headers={"User-Agent": USER_AGENT})


def download(url: str, dest: Path, force: bool = False) -> None:
    dest.parent.mkdir(parents=True, exist_ok=True)
    if dest.exists() and not force and dest.stat().st_size > 0:
        print(f"fetch-wakeword-corpus: reuse {dest}", file=sys.stderr)
        return
    print(f"fetch-wakeword-corpus: GET {url}", file=sys.stderr)
    fd, tmp_name = tempfile.mkstemp(prefix=dest.name + ".", suffix=".part", dir=str(dest.parent))
    try:
        with os.fdopen(fd, "wb") as out:
            with urllib.request.urlopen(request(url), timeout=120) as response:
                if response.status != 200:
                    fail(f"{url}: HTTP {response.status}")
                shutil.copyfileobj(response, out, length=1 << 20)
            out.flush()
            os.fsync(out.fileno())
        if os.path.getsize(tmp_name) == 0:
            fail(f"{url}: empty download")
        os.replace(tmp_name, dest)
    except Exception:
        try:
            os.unlink(tmp_name)
        except OSError:
            pass
        raise


def md5_file(path: Path) -> str:
    digest = hashlib.md5()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def probe_duration(path: Path, ffprobe: str) -> float:
    cmd = [
        ffprobe,
        "-v",
        "error",
        "-show_entries",
        "format=duration",
        "-of",
        "csv=p=0",
        str(path),
    ]
    try:
        proc = subprocess.run(cmd, capture_output=True, text=True)
    except FileNotFoundError:
        fail(f"ffprobe not found: {ffprobe!r} (use --ffprobe PATH)")
    if proc.returncode != 0:
        fail(f"ffprobe failed for {path}: {proc.stderr.strip()}")
    try:
        return float(proc.stdout.strip().splitlines()[0])
    except (ValueError, IndexError):
        fail(f"ffprobe returned no duration for {path}")


def media_plan() -> dict:
    music = []
    for track in MUSIC_TRACKS:
        music.append(
            {
                "file": track["file"],
                "url": MUSIC_URL_BASE + urllib.parse.quote(track["file"], safe=""),
                "seconds": track["seconds"],
                "split": track["split"],
                "license": "CC-BY-4.0",
            }
        )
    noise = []
    for split, files in NOISE_FILES.items():
        for name in files:
            if name in DEMAND_FILES:
                fail(f"internal recipe error: {name} is a DEMAND file")
            noise.append(
                {
                    "file": name,
                    "url": NOISE_URL_BASE + urllib.parse.quote(name, safe=""),
                    "split": split,
                    "license": "CC0-1.0",
                    "source_ref": "noise_train/" + name,
                }
            )
    return {
        "sources": list(SOURCES.values()),
        "music": {
            "url_base": MUSIC_URL_BASE,
            "license_page": "https://incompetech.com/music/royalty-free/licenses/",
            "catalog": "https://incompetech.com/music/royalty-free/pieces.json",
            "tracks": music,
            "total_seconds": sum(t["seconds"] for t in MUSIC_TRACKS),
        },
        "noise": {
            "url_base": NOISE_URL_BASE,
            "excluded_demand_files": sorted(DEMAND_FILES),
            "excluded_size": DEMAND_SIZE,
            "files": noise,
        },
        "speech": {
            "archives": LIBRISPEECH_ARCHIVES,
            "attribution": LIBRISPEECH_ATTRIBUTION,
            "fleurs_ru_dev": FLEURS_RU_DEV,
            "recommendation": (
                "Use dev-clean for train/tune speakers and test-clean for test "
                "speakers; keep every speaker whole and cap about 135 s per speaker. "
                "FLEURS ru dev provides about 30 min of Russian speech after "
                "extraction; keep each WAV whole and record source.language=ru."
            ),
        },
        "builder_command": (
            "python3 tools/build_wakeword_negatives.py "
            "--manifest <manifest.json> --out <negatives.bin>"
        ),
    }


def build_manifest_from_media(out: Path, ffprobe: str, force: bool) -> dict:
    sources = [SOURCES["incompetech"], SOURCES["ms-snsd-freesound"]]
    streams = []
    for track in MUSIC_TRACKS:
        name = track["file"]
        dest = out / "music" / name
        url = MUSIC_URL_BASE + urllib.parse.quote(name, safe="")
        download(url, dest, force=force)
        seconds = probe_duration(dest, ffprobe)
        if abs(seconds - track["seconds"]) > max(3.0, track["seconds"] * 0.05):
            print(
                f"fetch-wakeword-corpus: warning: {name} duration {seconds:.1f}s "
                f"differs from catalog {track['seconds']}s",
                file=sys.stderr,
            )
        streams.append(
            {
                "id": "incompetech-" + Path(name).stem.lower().replace(" ", "-"),
                "source_id": "incompetech",
                "path": str(dest.relative_to(out)),
                "url": url,
                "kind": "music",
                "split": track["split"],
            }
        )
    for split, files in NOISE_FILES.items():
        for name in files:
            if name in DEMAND_FILES:
                fail(f"refusing DEMAND file {name}")
            dest = out / "noise" / name
            url = NOISE_URL_BASE + urllib.parse.quote(name, safe="")
            download(url, dest, force=force)
            seconds = probe_duration(dest, ffprobe)
            if seconds <= 0:
                fail(f"{dest}: no audio")
            streams.append(
                {
                    "id": "ms-snsd-" + Path(name).stem.lower().replace("_", "-"),
                    "source_id": "ms-snsd-freesound",
                    "path": str(dest.relative_to(out)),
                    "url": url,
                    "source_ref": "noise_train/" + name,
                    "kind": "noise",
                    "split": split,
                }
            )
    manifest = {"version": 1, "sources": sources, "streams": streams}
    manifest_path = out / "manifest-noise-music.json"
    manifest_path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    total = sum(
        probe_duration(out / s["path"], ffprobe) for s in streams
    )
    print(
        f"fetch-wakeword-corpus: wrote {manifest_path} "
        f"({len(streams)} streams, {total / 60:.1f} min)",
        file=sys.stderr,
    )
    print(
        "fetch-wakeword-corpus: next: python3 tools/build_wakeword_negatives.py "
        f"--manifest {manifest_path} --out {out / 'negatives.bin'}",
        file=sys.stderr,
    )
    return manifest


def librispeech_set(path: Path, root: Path) -> str:
    try:
        parts = path.relative_to(root).parts
    except ValueError:
        parts = path.parts
    for known in LIBRISPEECH_ARCHIVES:
        if known in parts:
            return known
    for part in parts:
        if part.endswith("-clean") or part.startswith("train-clean"):
            return part
    return ""


def speaker_of(path: Path, root: Path) -> str:
    try:
        parts = path.relative_to(root).parts
    except ValueError:
        parts = path.parts
    for marker in LIBRISPEECH_ARCHIVES:
        if marker in parts:
            index = parts.index(marker)
            if index + 1 < len(parts):
                return parts[index + 1]
    stem = path.stem
    if "-" in stem:
        return stem.split("-", 1)[0]
    return path.parent.name


def assign_speakers(groups: dict[str, dict[str, list[Path]]]) -> dict[str, str]:
    """Return (set, speaker) -> split, keeping every speaker whole."""
    result: dict[str, str] = {}
    dev_speakers: list[tuple[str, str]] = []
    test_speakers: list[tuple[str, str]] = []
    other_speakers: list[tuple[str, str]] = []
    for set_name, speakers in groups.items():
        for speaker in speakers:
            key = (set_name, speaker)
            low = (set_name + "/" + speaker).lower()
            if "test" in low:
                test_speakers.append(key)
            elif "dev" in low:
                dev_speakers.append(key)
            else:
                other_speakers.append(key)
    for index, key in enumerate(sorted(dev_speakers)):
        result["/".join(key)] = "train" if index % 2 == 0 else "tune"
    for key in sorted(test_speakers):
        result["/".join(key)] = "test"
    ordered = sorted(other_speakers)
    for index, key in enumerate(ordered):
        if len(ordered) >= 3 and index >= int(len(ordered) * 0.8):
            split = "test"
        elif len(ordered) >= 3 and index >= int(len(ordered) * 0.6):
            split = "tune"
        else:
            split = "train"
        result["/".join(key)] = split
    return result


def speech_manifest(args: argparse.Namespace) -> dict:
    roots = [Path(r).expanduser().resolve() for r in args.root]
    for root in roots:
        if not root.is_dir():
            fail(f"speech root is not a directory: {root}")
    groups: dict[str, dict[str, list[Path]]] = {}
    files: dict[tuple[str, str], list[Path]] = {}
    for root in roots:
        for path in sorted(root.rglob("*.flac")):
            set_name = librispeech_set(path, root)
            speaker = speaker_of(path, root)
            files.setdefault((set_name, speaker), []).append(path)
            groups.setdefault(set_name, {}).setdefault(speaker, []).append(path)
    if not files:
        fail("no .flac files found under --root")
    split_map = assign_speakers(groups)
    streams = []
    total_seconds = 0.0
    for (set_name, speaker), paths in sorted(files.items()):
        split = split_map.get(f"{set_name}/{speaker}", "train")
        budget = float(args.seconds_per_speaker)
        used = 0.0
        for path in sorted(paths, key=lambda p: p.name):
            duration = probe_duration(path, args.ffprobe)
            if duration < args.min_duration:
                continue
            if used + duration > budget and used > 0:
                break
            used += duration
            total_seconds += duration
            archive = LIBRISPEECH_ARCHIVES.get(set_name, {})
            relative = path.name
            for root in roots:
                try:
                    relative = str(path.relative_to(root))
                    break
                except ValueError:
                    continue
            streams.append(
                {
                    "id": (
                        "ls-"
                        + (set_name or "speech")
                        + "-"
                        + speaker
                        + "-"
                        + path.stem
                    ),
                    "source_id": "librispeech",
                    "path": str(path),
                    "url": archive.get("url", SOURCES["librispeech"]["url"]),
                    "source_ref": relative,
                    "kind": "speech",
                    "split": split,
                }
            )
        if used == 0:
            print(
                f"fetch-wakeword-corpus: warning: no usable audio for "
                f"{set_name}/{speaker}",
                file=sys.stderr,
            )
    if not streams:
        fail("speech manifest would be empty")
    manifest = {
        "version": 1,
        "sources": [SOURCES["librispeech"]],
        "streams": streams,
    }
    out = Path(args.out).expanduser().resolve()
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    by_split: dict[str, int] = {}
    for stream in streams:
        by_split[stream["split"]] = by_split.get(stream["split"], 0) + 1
    print(
        f"fetch-wakeword-corpus: wrote {out}: {len(streams)} streams, "
        f"{total_seconds / 60:.1f} min, speakers/split {by_split}",
        file=sys.stderr,
    )
    return manifest


def fleurs_manifest(args: argparse.Namespace) -> dict:
    root = Path(args.root).expanduser().resolve()
    if not root.is_dir():
        fail(f"FLEURS root is not a directory: {root}")
    wavs = sorted(root.rglob("*.wav"))
    if not wavs:
        fail(f"no .wav files under {root}")
    source = SOURCES["fleurs-ru"]
    splits = ["train", "tune", "test"]
    totals = {split: 0.0 for split in splits}
    streams = []
    skipped = 0
    for index, path in enumerate(wavs):
        duration = probe_duration(path, args.ffprobe)
        if duration < args.min_duration:
            skipped += 1
            continue
        chosen = None
        for attempt in range(len(splits)):
            split = splits[(index + attempt) % len(splits)]
            if args.max_seconds_per_split <= 0 or (
                totals[split] + duration <= args.max_seconds_per_split
            ):
                chosen = split
                break
        if chosen is None:
            skipped += 1
            continue
        totals[chosen] += duration
        streams.append(
            {
                "id": "fleurs-ru-" + path.stem,
                "source_id": "fleurs-ru",
                "path": str(path),
                "url": FLEURS_RU_DEV["url"],
                "source_ref": str(path.relative_to(root)),
                "kind": "speech",
                "split": chosen,
                "language": "ru",
            }
        )
    if not streams:
        fail("FLEURS manifest would be empty")
    manifest = {"version": 1, "sources": [source], "streams": streams}
    out = Path(args.out).expanduser().resolve()
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    print(
        f"fetch-wakeword-corpus: wrote {out}: {len(streams)} Russian streams, "
        f"{sum(totals.values()) / 60:.1f} min "
        f"({', '.join(f'{s}={totals[s] / 60:.1f}m' for s in splits)}), "
        f"skipped {skipped}",
        file=sys.stderr,
    )
    return manifest


def merge_manifests(inputs: list[Path], out: Path) -> dict:
    sources: dict[str, dict] = {}
    streams: list[dict] = []
    stream_ids: set[str] = set()
    paths: set[str] = set()
    for path in inputs:
        try:
            doc = json.loads(path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            fail(f"manifest not found: {path}")
        except json.JSONDecodeError as exc:
            fail(f"{path}: invalid JSON: {exc}")
        for src in doc.get("sources", []):
            sid = src.get("id")
            if sid in sources and sources[sid] != src:
                fail(f"source {sid!r} differs between manifests")
            sources[sid] = src
        for stream in doc.get("streams", []):
            stid = stream.get("id")
            if stid in stream_ids:
                fail(f"duplicate stream id {stid!r} across manifests")
            stream_ids.add(stid)
            key = os.path.realpath(str(stream.get("path", "")))
            if key in paths:
                fail(f"duplicate input file {key} across manifests")
            paths.add(key)
            streams.append(stream)
    for stream in streams:
        if stream.get("source_id") not in sources:
            fail(f"stream {stream.get('id')!r}: unknown source_id")
    manifest = {"version": 1, "sources": list(sources.values()), "streams": streams}
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    print(
        f"fetch-wakeword-corpus: wrote {out}: {len(streams)} streams, "
        f"{len(sources)} sources",
        file=sys.stderr,
    )
    return manifest


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    p_plan = sub.add_parser("plan", help="print exact URLs, licences and splits")
    p_plan.add_argument("--json", action="store_true", default=True)

    p_fetch = sub.add_parser("fetch", help="download music and noise and write a manifest")
    p_fetch.add_argument("--out", required=True)
    p_fetch.add_argument("--ffprobe", default="ffprobe")
    p_fetch.add_argument("--force", action="store_true")

    p_speech = sub.add_parser("speech-manifest", help="manifest from extracted LibriSpeech FLACs")
    p_speech.add_argument("--root", action="append", required=True)
    p_speech.add_argument("--out", required=True)
    p_speech.add_argument("--ffprobe", default="ffprobe")
    p_speech.add_argument("--seconds-per-speaker", type=float, default=135.0)
    p_speech.add_argument("--min-duration", type=float, default=0.8)

    p_fleurs = sub.add_parser(
        "fleurs-manifest", help="manifest from an extracted Google FLEURS ru dev WAV set"
    )
    p_fleurs.add_argument("--root", required=True)
    p_fleurs.add_argument("--out", required=True)
    p_fleurs.add_argument("--ffprobe", default="ffprobe")
    p_fleurs.add_argument("--max-seconds-per-split", type=float, default=600.0)
    p_fleurs.add_argument("--min-duration", type=float, default=0.8)

    p_merge = sub.add_parser("merge", help="merge source manifests")
    p_merge.add_argument("--input", action="append", required=True)
    p_merge.add_argument("--out", required=True)

    p_lic = sub.add_parser("licenses", help="print the policy and excluded DEMAND files")
    p_lic.add_argument("--json", action="store_true", default=True)

    args = parser.parse_args(argv)
    if args.command == "plan":
        print(json.dumps(media_plan(), indent=2))
    elif args.command == "fetch":
        build_manifest_from_media(
            Path(args.out).expanduser().resolve(), args.ffprobe, args.force
        )
    elif args.command == "speech-manifest":
        speech_manifest(args)
    elif args.command == "fleurs-manifest":
        fleurs_manifest(args)
    elif args.command == "merge":
        merge_manifests(
            [Path(p).expanduser().resolve() for p in args.input],
            Path(args.out).expanduser().resolve(),
        )
    elif args.command == "licenses":
        print(
            json.dumps(
                {
                    "allowed": ["CC0-1.0", "CC-BY-4.0", "CC-BY-3.0", "Public-Domain"],
                    "rejected": ["NC", "ND", "SA", "unknown"],
                    "excluded_demand_files": sorted(DEMAND_FILES),
                    "excluded_demand_size": DEMAND_SIZE,
                    "notes": (
                        "MS-SNSD README: Freesound noise files are CC0; DEMAND "
                        "environment files are CC-BY-SA-3.0 and are excluded."
                    ),
                },
                indent=2,
            )
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
