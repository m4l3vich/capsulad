#!/usr/bin/env python3
"""Generate web/src/lib/timezone-map-data.js from public geodata.

Inputs (not checked in):

  combined.json           evansiroky/timezone-boundary-builder, release 2026f,
                          asset timezones.geojson.zip, member combined.json.
                          https://github.com/evansiroky/timezone-boundary-builder/releases/download/2026f/timezones.geojson.zip
                          One feature per IANA zone (419 of them), land only.
                          Boundaries (c) timezone-boundary-builder contributors,
                          ODbL 1.0.
                          Do NOT use timezones-now.geojson.zip: that asset merges
                          every zone that currently shares an offset into 62
                          representative polygons, so most cities would have no
                          boundary of their own.

  ne_50m_admin_0_countries.geojson
                          Natural Earth 1:50m Admin 0 Countries, public domain.
                          https://raw.githubusercontent.com/nvkelso/natural-earth-vector/master/geojson/ne_50m_admin_0_countries.geojson

  zone.tab                IANA tzdata 2026f zone.tab, for city coordinates.
                          https://data.iana.org/time-zones/releases/tzdata2026f.tar.gz
                          The tarball is read directly: zone.tab supplies the
                          city coordinates and the Link lines in backward,
                          etcetera and backzone resolve zone aliases. On a host
                          with tzdata installed /usr/share/zoneinfo/zone.tab
                          works too, but pin the release above for reproducibility.

Run:

  python3 web/scripts/generate-timezone-map.py \
      --tz /tmp/tz/combined.json \
      --countries /tmp/tz/ne_50m_admin_0_countries.geojson \
      --tzdata /tmp/tz/tzdata2026f.tar.gz

Output: an equirectangular 0.02-degree grid (viewBox 0 0 18000 9000) with
Douglas-Peucker-simplified rings, encoded as relative SVG path verbs so the
browser draws them without per-render simplification. Python stdlib only.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import tarfile

# Release pin, quoted in the generated header for provenance.
TZ_RELEASE = "2026f"
NE_SOURCE = "Natural Earth 1:50m Admin 0 Countries (public domain)"

UNITS_PER_DEGREE = 50          # viewBox units per degree: 1 unit = 0.02 deg
MAP_WIDTH = 360 * UNITS_PER_DEGREE   # 18000
MAP_HEIGHT = 180 * UNITS_PER_DEGREE  # 9000

ZONE_TOLERANCE = 0.03          # degrees, roughly 3 km at the equator
COUNTRY_TOLERANCE = 0.04
RADIAL_PREFILTER = 0.01        # degrees, drops vertices before Douglas-Peucker
MIN_RING_BBOX = 0.03           # degrees; smaller rings are islets, unless a zone
                               # would otherwise have no land at all

ZONE_TAB_COORD = re.compile(
    r"^([+-])(\d{2})(\d{2})(\d{2})?([+-])(\d{3})(\d{2})(\d{2})?$"
)


def project(lon: float, lat: float) -> tuple[int, int]:
    x = round((lon + 180.0) * UNITS_PER_DEGREE)
    y = round((90.0 - lat) * UNITS_PER_DEGREE)
    return (max(0, min(MAP_WIDTH, x)), max(0, min(MAP_HEIGHT, y)))


def number(value: int) -> str:
    return str(value)


def encode_rings(rings: list[list[tuple[int, int]]]) -> str:
    """Encode quantised rings as one SVG path.

    Closed rings become `M x y` followed by a single relative `l` run, which
    repeats its verb for every following pair. Negative numbers act as their
    own separator, so only a leading space is emitted when needed.
    """
    parts: list[str] = []
    for ring in rings:
        if len(ring) < 3:
            continue
        parts.append(f"M{ring[0][0]} {ring[0][1]}")
        deltas: list[str] = []
        prev = ring[0]
        for point in ring[1:]:
            dx, dy = point[0] - prev[0], point[1] - prev[1]
            if dx == 0 and dy == 0:
                continue
            deltas.append(number(dx))
            deltas.append(number(dy))
            prev = point
        if len(deltas) < 4:  # needs at least two distinct vertices
            continue
        body = deltas[0]
        for value in deltas[1:]:
            body += value if value.startswith("-") else " " + value
        # `l` accepts a repeating coordinate list; the final `Z` closes it.
        parts.append("l" + body + "z")
    return "".join(parts)


def ring_bbox(points: list[tuple[float, float]]) -> tuple[float, float]:
    xs = [point[0] for point in points]
    ys = [point[1] for point in points]
    return (max(xs) - min(xs), max(ys) - min(ys))


def dedupe(points: list[tuple[float, float]], minimum: float) -> list[tuple[float, float]]:
    """Drop repeated vertices and near-duplicates before simplification."""
    out: list[tuple[float, float]] = []
    for point in points:
        if not out:
            out.append(point)
            continue
        last = out[-1]
        if abs(point[0] - last[0]) < minimum and abs(point[1] - last[1]) < minimum:
            continue
        out.append(point)
    return out


def douglas_peucker(points: list[tuple[float, float]], tolerance: float) -> list[tuple[float, float]]:
    """Iterative Douglas-Peucker; rings arrive with hundreds of thousands of vertices."""
    count = len(points)
    if count < 4:
        return points
    keep = [False] * count
    keep[0] = keep[count - 1] = True
    tolerance2 = tolerance * tolerance
    stack = [(0, count - 1)]
    while stack:
        start, end = stack.pop()
        ax, ay = points[start]
        bx, by = points[end]
        dx, dy = bx - ax, by - ay
        segment2 = dx * dx + dy * dy
        farthest = -1.0
        farthest_index = -1
        for index in range(start + 1, end):
            px, py = points[index]
            if segment2 == 0.0:
                distance2 = (px - ax) ** 2 + (py - ay) ** 2
            else:
                t = ((px - ax) * dx + (py - ay) * dy) / segment2
                t = 0.0 if t < 0.0 else (1.0 if t > 1.0 else t)
                distance2 = (px - (ax + t * dx)) ** 2 + (py - (ay + t * dy)) ** 2
            if distance2 > farthest:
                farthest = distance2
                farthest_index = index
        if farthest > tolerance2:
            keep[farthest_index] = True
            stack.append((start, farthest_index))
            stack.append((farthest_index, end))
    return [point for point, flag in zip(points, keep) if flag]


def clean_ring(coords: list[list[float]]) -> list[tuple[float, float]]:
    ring: list[tuple[float, float]] = []
    for lon, lat in coords:
        point = (float(lon), float(lat))
        if not ring or ring[-1] != point:
            ring.append(point)
    if len(ring) > 1 and ring[0] == ring[-1]:
        ring.pop()
    return ring


def simplify_ring(
    ring: list[tuple[float, float]], tolerance: float
) -> list[tuple[int, int]]:
    if len(ring) < 3:
        return []
    closed = ring + [ring[0]]
    simplified = douglas_peucker(closed, tolerance)
    if len(simplified) > 1 and simplified[0] == simplified[-1]:
        simplified.pop()
    return [project(lon, lat) for lon, lat in simplified]


def geometry_rings(geometry: dict):
    """Yield exterior+holes rings from a Polygon or MultiPolygon."""
    kind = geometry.get("type")
    if kind == "Polygon":
        polygons = [geometry["coordinates"]]
    elif kind == "MultiPolygon":
        polygons = geometry["coordinates"]
    else:
        return
    for polygon in polygons:
        for ring in polygon:
            yield ring


def load_timezone_rings(path: str) -> dict[str, list[list[tuple[float, float]]]]:
    with open(path, "r", encoding="utf-8") as handle:
        data = json.load(handle)
    zones: dict[str, list[list[tuple[float, float]]]] = {}
    for feature in data["features"]:
        tzid = (feature.get("properties") or {}).get("tzid")
        if not tzid:
            continue
        bucket = zones.setdefault(tzid, [])
        for ring in geometry_rings(feature["geometry"]):
            cleaned = clean_ring(ring)
            if len(cleaned) >= 3:
                bucket.append(cleaned)
    return zones


def parse_zone_tab(text: str) -> list[dict]:
    cities: list[dict] = []
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        fields = line.split("\t")
        if len(fields) < 3:
            continue
        country, coordinates, zone = fields[0], fields[1], fields[2]
        match = ZONE_TAB_COORD.match(coordinates.strip())
        if not match:
            print(f"warning: unparsed coordinates for {zone}: {coordinates!r}", file=sys.stderr)
            continue
        sign_top, deg_lat, min_lat, sec_lat, sign_side, deg_lon, min_lon, sec_lon = match.groups()
        lat = int(deg_lat) + int(min_lat) / 60.0 + (int(sec_lat) / 3600.0 if sec_lat else 0.0)
        lon = int(deg_lon) + int(min_lon) / 60.0 + (int(sec_lon) / 3600.0 if sec_lon else 0.0)
        if sign_top == "-":
            lat = -lat
        if sign_side == "-":
            lon = -lon
        cities.append(
            {
                "zone": zone,
                "city": zone.split("/")[-1].replace("_", " "),
                "country": country,
                "lat": round(lat, 2),
                "lon": round(lon, 2),
            }
        )
    return cities


def load_links(tzdata_path: str) -> dict[str, str]:
    """Alias -> canonical zone map from the tzdata Link lines."""
    links: dict[str, str] = {}
    with tarfile.open(tzdata_path, "r:*") as archive:
        members = {member.name.split("/")[-1]: member for member in archive.getmembers()}
        for name in ("backward", "etcetera", "backzone"):
            member = members.get(name)
            if member is None:
                continue
            handle = archive.extractfile(member)
            if handle is None:
                continue
            for line in handle.read().decode("utf-8").splitlines():
                fields = line.split()
                if len(fields) >= 3 and fields[0] == "Link":
                    links[fields[2]] = fields[1]
    return links


def covers_point(
    rings: list[list[tuple[float, float]]], lat: float, lon: float, padding: float = 0.5
) -> bool:
    """Cheap sanity check that a borrowed boundary is in the city's neighbourhood.

    A zone alias is only useful when the city actually sits in the target zone
    (Vatican in Rome, Busingen in Zurich). Etc/UTC, for example, has Antarctic
    land, which must not be highlighted for the `UTC` city entry.
    """
    if not rings:
        return False
    lons = [point[0] for ring in rings for point in ring]
    lats = [point[1] for ring in rings for point in ring]
    return (
        min(lons) - padding <= lon <= max(lons) + padding
        and min(lats) - padding <= lat <= max(lats) + padding
    )


def canonical(zone: str, links: dict[str, str], known: set[str]) -> str:
    seen = set()
    while zone not in known and zone in links and zone not in seen:
        seen.add(zone)
        zone = links[zone]
    return zone


def load_tz_zone_tab(tzdata_path: str) -> str:
    with tarfile.open(tzdata_path, "r:*") as archive:
        for member in archive.getmembers():
            if member.name.split("/")[-1] == "zone.tab":
                handle = archive.extractfile(member)
                if handle is not None:
                    return handle.read().decode("utf-8")
    raise SystemExit(f"no zone.tab in {tzdata_path}")


def simplify_zones(
    zones: dict[str, list[list[tuple[float, float]]]],
    zone_tolerance: float,
    country_tolerance: float,
) -> tuple[dict[str, str], dict[str, int]]:
    paths: dict[str, str] = {}
    stats: dict[str, int] = {"rings_in": 0, "rings_out": 0, "points": 0}
    for zone in sorted(zones):
        rings = zones[zone]
        stats["rings_in"] += len(rings)
        kept: list[tuple[float, tuple[list[tuple[int, int]], float]]] = []
        for ring in rings:
            width, height = ring_bbox(ring)
            extent = max(width, height)
            simplified = simplify_ring(dedupe(ring, RADIAL_PREFILTER), zone_tolerance)
            if len(simplified) < 3:
                continue
            kept.append((width * height, (simplified, extent)))
        # Drop islets, but never leave a zone without any geometry: Monaco,
        # Singapore and similar microstates keep their single ring.
        sizeable = [item for item in kept if item[1][1] >= MIN_RING_BBOX]
        chosen = sizeable if sizeable else kept[:1]
        encoded = encode_rings([item[1][0] for item in chosen])
        if encoded:
            paths[zone] = encoded
            stats["rings_out"] += len(chosen)
            stats["points"] += sum(len(item[1][0]) for item in chosen)
    return paths, stats


def simplify_countries(path: str, country_tolerance: float) -> tuple[str, int]:
    with open(path, "r", encoding="utf-8") as handle:
        data = json.load(handle)
    rings: list[list[tuple[int, int]]] = []
    points = 0
    for feature in data["features"]:
        geometry = feature.get("geometry") or {}
        for ring in geometry_rings(geometry):
            cleaned = clean_ring(ring)
            cleaned = dedupe(cleaned, RADIAL_PREFILTER)
            if len(cleaned) < 3:
                continue
            width, height = ring_bbox(cleaned)
            if max(width, height) < MIN_RING_BBOX:
                continue
            simplified = simplify_ring(cleaned, country_tolerance)
            if len(simplified) < 3:
                continue
            rings.append(simplified)
            points += len(simplified)
    return encode_rings(rings), points


def js_string(value: str) -> str:
    return json.dumps(value, ensure_ascii=True)


def write_module(
    out_path: str,
    zone_paths: dict[str, str],
    aliases: dict[str, str],
    country_path: str,
    cities: list[dict],
    zone_tolerance: float,
    country_tolerance: float,
) -> None:
    lines: list[str] = []
    lines.append("// Time zone map geometry for the settings picker. Generated file, do not edit.")
    lines.append("//")
    lines.append(
        f"// Time zone boundaries: timezone-boundary-builder {TZ_RELEASE}"
    )
    lines.append("//   (combined.json, the per-zone land build),")
    lines.append("//   (c) timezone-boundary-builder contributors, ODbL 1.0,")
    lines.append("//   https://github.com/evansiroky/timezone-boundary-builder")
    lines.append(f"// Base map: {NE_SOURCE}, https://www.naturalearthdata.com/")
    lines.append(f"// City coordinates: IANA tzdata {TZ_RELEASE} zone.tab.")
    lines.append("// Generated by web/scripts/generate-timezone-map.py.")
    lines.append("//")
    lines.append(
        f"// Equirectangular projection, 1 unit = {1 / UNITS_PER_DEGREE:.2f} deg:"
    )
    lines.append(f"//   x = (lon + 180) * {UNITS_PER_DEGREE}, y = (90 - lat) * {UNITS_PER_DEGREE}")
    lines.append(
        f"// Rings are Douglas-Peucker simplified ({zone_tolerance} deg for zones,"
    )
    lines.append(
        f"// {country_tolerance} deg for the base map) and encoded as"
    )
    lines.append("// relative SVG path runs; render zone paths with fill-rule=\"evenodd\".")
    lines.append("")
    lines.append(f"export const MAP_WIDTH = {MAP_WIDTH}")
    lines.append(f"export const MAP_HEIGHT = {MAP_HEIGHT}")
    lines.append("")
    lines.append("export const COUNTRY_PATHS =")
    lines.append(f"  {js_string(country_path)}")
    lines.append("")
    lines.append("export const ZONE_PATHS = {")
    for zone in sorted(zone_paths):
        lines.append(f"  {js_string(zone)}: {js_string(zone_paths[zone])},")
    lines.append("}")
    lines.append("")
    lines.append("// City zones with no polygon of their own borrow the boundary of the")
    lines.append("// zone they follow; the selected city still pins at its own coordinates.")
    lines.append("export const ZONE_ALIASES = {")
    for zone in sorted(aliases):
        lines.append(f"  {js_string(zone)}: {js_string(aliases[zone])},")
    lines.append("}")
    lines.append("")
    lines.append("export const CITIES = [")
    for city in cities:
        lines.append(
            "  { zone: %s, city: %s, country: %s, lat: %s, lon: %s },"
            % (
                js_string(city["zone"]),
                js_string(city["city"]),
                js_string(city["country"]),
                repr(city["lat"]),
                repr(city["lon"]),
            )
        )
    lines.append("]")
    lines.append("")
    payload = "\n".join(lines)
    os.makedirs(os.path.dirname(out_path), exist_ok=True)
    with open(out_path, "w", encoding="utf-8") as handle:
        handle.write(payload)
    print(f"wrote {out_path} ({len(payload.encode('utf-8'))} bytes)")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--tz", required=True, help="combined.json from timezones.geojson.zip"
    )
    parser.add_argument("--countries", required=True, help="ne_50m_admin_0_countries.geojson")
    parser.add_argument("--tzdata", required=True, help="tzdata<release>.tar.gz (zone.tab + links)")
    parser.add_argument("--zone-tolerance", type=float, default=ZONE_TOLERANCE)
    parser.add_argument("--country-tolerance", type=float, default=COUNTRY_TOLERANCE)
    parser.add_argument(
        "--out",
        default=os.path.join("web", "src", "lib", "timezone-map-data.js"),
        help="output module (default: web/src/lib/timezone-map-data.js)",
    )
    args = parser.parse_args()

    links = load_links(args.tzdata)
    cities = parse_zone_tab(load_tz_zone_tab(args.tzdata))
    if not any(city["zone"] == "UTC" for city in cities):
        cities.append({"zone": "UTC", "city": "UTC", "country": "", "lat": 0.0, "lon": 0.0})
    cities.sort(key=lambda city: city["zone"])

    zones = load_timezone_rings(args.tz)
    known = set(zones)
    zone_paths, stats = simplify_zones(zones, args.zone_tolerance, args.country_tolerance)
    print(
        f"zones: {len(known)} with land, {len(zone_paths)} encoded, "
        f"rings {stats['rings_in']} -> {stats['rings_out']}, {stats['points']} path points"
    )

    # Zones too small to survive simplification (Vatican, Busingen) or named by
    # a link (UTC -> Etc/UTC) borrow the boundary of the zone whose rules they
    # actually follow. The pin still sits on the selected city's own coordinates.
    aliases: dict[str, str] = {}
    missing = []
    for city in cities:
        zone = city["zone"]
        if zone in zone_paths:
            continue
        target = canonical(zone, links, set(zone_paths))
        if target in zone_paths and covers_point(zones[target], city["lat"], city["lon"]):
            aliases[zone] = target
            continue
        missing.append(zone)
    if missing:
        print(f"{len(missing)} city zones have no boundary geometry:", file=sys.stderr)
        for zone in missing:
            print(f"  {zone}", file=sys.stderr)

    if aliases:
        print(f"boundary aliases: {aliases}", file=sys.stderr)

    country_path, country_points = simplify_countries(args.countries, args.country_tolerance)
    print(f"countries: {country_points} path points")
    write_module(
        args.out,
        zone_paths,
        aliases,
        country_path,
        cities,
        args.zone_tolerance,
        args.country_tolerance,
    )


if __name__ == "__main__":
    main()
