#!/usr/bin/env python3
"""Regenerate the Material Symbols Rounded subset embedded in the SPA.

The SPA renders a fixed set of icon ligatures (see src/lib/iconNames.js and the
<Icon> bodies). Shipping the full 6,670-glyph family costs 5.4 MB in the
daemon; this script produces the rounded-only subset for exactly those names.

The output is a checked-in build input, not a post-build edit:

  src/assets/material-symbols-rounded-subset.woff2
  src/assets/icon-font.json        manifest: glyphs, axes, hashes, versions

Every name must exist in the pinned material-symbols package. fontTools drops
unknown names silently, so this script fails before writing anything and lists
them. It also shapes each name with HarfBuzz at FILL 0 and FILL 1 and requires a
single glyph, and rejects output whose variable axes changed.

Reproduce:

  python3 -m venv build/font-venv
  build/font-venv/bin/pip install -r web/scripts/requirements-fonts.txt
  build/font-venv/bin/python web/scripts/build-icon-font.py

Determinism: the subset is produced twice in the same process and the bytes
must match before the files are replaced. The manifest records the source and
output SHA-256 so `npm run check:icons` can detect a stale font.

Options:

  --check           do not write; fail if the checked-in font or manifest drift
  --require NAME    also require NAME (repeatable; used by the failure test)
"""

from __future__ import annotations

import argparse
import hashlib
import io
import json
import subprocess
import tempfile
from importlib.metadata import version as package_version_of
from pathlib import Path

WEB = Path(__file__).resolve().parent.parent
SOURCE = WEB / "node_modules" / "material-symbols" / "material-symbols-rounded.woff2"
FONT = WEB / "src" / "assets" / "material-symbols-rounded-subset.woff2"
MANIFEST = WEB / "src" / "assets" / "icon-font.json"
EXTRACTOR = WEB / "scripts" / "icon-names.mjs"

PINNED_FONTTOOLS = "4.68.0"
PINNED_MATERIAL_SYMBOLS = "0.48.3"
EXPECTED_AXES = {
    "FILL": (0.0, 0.0, 1.0),
    "GRAD": (-50.0, 0.0, 200.0),
    "opsz": (20.0, 24.0, 48.0),
    "wght": (100.0, 400.0, 700.0),
}
ASCII_UNICODES = "U+0020-007E"


def fail(message: str) -> None:
    raise SystemExit(f"build-icon-font: {message}")


def package_version() -> str:
    package = json.loads((SOURCE.parent / "package.json").read_text())
    return package["version"]


def icon_names(extra: list[str]) -> list[str]:
    result = subprocess.run(
        ["node", str(EXTRACTOR)],
        cwd=WEB,
        check=True,
        capture_output=True,
        text=True,
    )
    names = set(json.loads(result.stdout))
    names.update(extra)
    return sorted(names)


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def make_subset(names: list[str]) -> bytes:
    from fontTools import subset, version as fonttools_version

    if fonttools_version != PINNED_FONTTOOLS:
        fail(
            f"fontTools {fonttools_version} is not the pinned {PINNED_FONTTOOLS}; "
            "install web/scripts/requirements-fonts.txt"
        )

    with tempfile.TemporaryDirectory(prefix="capsulad-font-") as tmp:
        out = Path(tmp) / "subset.woff2"
        subset.main([
            str(SOURCE),
            f"--glyphs={','.join(names)}",
            f"--unicodes={ASCII_UNICODES}",
            "--no-layout-closure",
            "--glyph-names",
            "--layout-features=*",
            "--flavor=woff2",
            f"--output-file={out}",
        ])
        return out.read_bytes()


def shaper(data: bytes):
    import uharfbuzz as hb
    from fontTools.ttLib import TTFont

    font = TTFont(io.BytesIO(data))
    order = font.getGlyphOrder()
    font.flavor = None
    sfnt = io.BytesIO()
    font.save(sfnt)
    face = hb.Face(sfnt.getvalue())

    def shape(name: str, fill: int) -> list[str]:
        shaped = hb.Font(face)
        shaped.set_variations({"FILL": fill})
        buffer = hb.Buffer()
        buffer.add_str(name)
        buffer.guess_segment_properties()
        hb.shape(shaped, buffer)
        return [order[info.codepoint] for info in buffer.glyph_infos]

    return shape


def validate(names: list[str], data: bytes, source_data: bytes) -> dict:
    import uharfbuzz as hb
    from fontTools.ttLib import TTFont
    from fontTools import version as fonttools_version

    font = TTFont(io.BytesIO(data))
    glyphs = font.getGlyphOrder()
    missing = [name for name in names if name not in glyphs]
    if missing:
        fail(f"source font has no glyph for: {', '.join(missing)}")

    axes = {axis.axisTag: (axis.minValue, axis.defaultValue, axis.maxValue) for axis in font["fvar"].axes}
    if axes != EXPECTED_AXES:
        fail(f"variable axes changed: {axes!r}; review before regenerating")
    if not hasattr(font["GSUB"].table, "FeatureVariations"):
        fail("GSUB FeatureVariations was dropped; FILL=1 would render the wrong glyph")

    # Shaping must pick the same glyph as the full family. Material Symbols
    # substitutes every filled ligature with its `.fill` alternate when FILL=1,
    # so retaining only the outline glyphs renders a different design in
    # browsers that honour FeatureVariations.
    source_shape = shaper(source_data)
    subset_shape = shaper(data)
    for name in names:
        for fill in (0, 1):
            expected = source_shape(name, fill)
            actual = subset_shape(name, fill)
            if actual != expected:
                fail(
                    f"{name!r} shapes to {actual!r} at FILL {fill}, "
                    f"but the full font shapes to {expected!r}"
                )

    return {
        "fonttools": fonttools_version,
        "uharfbuzz": package_version_of("uharfbuzz"),
        "harfbuzz": hb.version_string(),
        "glyph_count": len(glyphs),
        "axes": {tag: list(values) for tag, values in axes.items()},
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true", help="verify without writing")
    parser.add_argument("--require", action="append", default=[], metavar="NAME")
    args = parser.parse_args()

    if not SOURCE.is_file():
        fail(f"missing {SOURCE}; run npm ci in web/ first")
    try:
        import brotli  # noqa: F401  (WOFF2 compression)
        import uharfbuzz  # noqa: F401  (shaping validation)
        from fontTools import version as fonttools_version
    except ImportError as error:
        fail(f"missing font tool ({error}); install web/scripts/requirements-fonts.txt")
    if fonttools_version != PINNED_FONTTOOLS:
        fail(
            f"fontTools {fonttools_version} is not the pinned {PINNED_FONTTOOLS}; "
            "install web/scripts/requirements-fonts.txt"
        )
    version = package_version()
    if version != PINNED_MATERIAL_SYMBOLS:
        fail(
            f"material-symbols {version} is not the pinned {PINNED_MATERIAL_SYMBOLS}; "
            "update the pin, the glyph list and the license copy together"
        )

    names = icon_names(args.require)
    from fontTools.ttLib import TTFont

    available = set(TTFont(SOURCE).getGlyphOrder())
    missing = [name for name in names if name not in available]
    if missing:
        fail(
            "source font has no glyph for: "
            + ", ".join(missing)
            + f" (material-symbols {version}); add a valid equivalent in src/lib/iconNames.js"
        )

    fill_glyphs = sorted(name + ".fill" for name in names if name + ".fill" in available)
    subset_glyphs = names + fill_glyphs
    first = make_subset(subset_glyphs)
    second = make_subset(subset_glyphs)
    if first != second:
        fail("two identical subset runs produced different bytes; refusing to write")
    details = validate(names, first, SOURCE.read_bytes())

    manifest = {
        "family": "Material Symbols Rounded",
        "license": "Apache-2.0",
        "source": {
            "package": "material-symbols",
            "version": version,
            "file": SOURCE.name,
            "sha256": sha256(SOURCE.read_bytes()),
        },
        "font": FONT.name,
        "font_bytes": len(first),
        "font_sha256": sha256(first),
        "ascii_unicodes": ASCII_UNICODES,
        "glyphs": names,
        "fill_glyphs": fill_glyphs,
        "glyph_count": details["glyph_count"],
        "axes": details["axes"],
        "fonttools": details["fonttools"],
        "uharfbuzz": details["uharfbuzz"],
        "harfbuzz": details["harfbuzz"],
        "brotli": package_version_of("brotli"),
    }
    manifest_bytes = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode()

    if args.check:
        current_font = FONT.read_bytes() if FONT.is_file() else b""
        current_manifest = MANIFEST.read_bytes() if MANIFEST.is_file() else b""
        if current_font != first or current_manifest != manifest_bytes:
            fail("checked-in icon font or manifest is stale; run without --check")
        print(f"icon font is current: {len(first)} bytes, {len(names)} icons")
        return

    FONT.parent.mkdir(parents=True, exist_ok=True)
    FONT.write_bytes(first)
    MANIFEST.write_bytes(manifest_bytes)
    print(
        f"wrote {FONT.relative_to(WEB)}: {len(first)} bytes, "
        f"{len(names)} icons, {details['glyph_count']} glyphs (sha256 {manifest['font_sha256']})"
    )


if __name__ == "__main__":
    main()
