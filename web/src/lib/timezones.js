// Time zone map data and lookups for the settings picker.
//
// The map is drawn from real geometry, not hand-placed markers:
//
//  - Time zone boundaries: timezone-boundary-builder 2026f (combined.json from
//    timezones.geojson.zip), (c) timezone-boundary-builder contributors,
//    ODbL 1.0.
//  - Base map: Natural Earth 1:50m Admin 0 Countries, public domain.
//  - City coordinates: IANA tzdata 2026f zone.tab.
//
// The generated module stores every ring already projected, simplified and
// encoded as an SVG path, so the picker never simplifies on load. The stored
// value is always the full IANA identifier (`Europe/Moscow`); the city label
// is only a readable presentation of it.

import {
  CITIES,
  COUNTRY_PATHS,
  MAP_HEIGHT,
  MAP_WIDTH,
  ZONE_ALIASES,
  ZONE_PATHS
} from './timezone-map-data.js'

export { COUNTRY_PATHS, MAP_HEIGHT, MAP_WIDTH }

const RADIANS = Math.PI / 180
const EARTH_RADIUS_KM = 6371.0088

/** Equirectangular projection into the map's viewBox coordinates. */
export function projectPoint(lat, lon) {
  return {
    x: ((lon + 180) / 360) * MAP_WIDTH,
    y: ((90 - lat) / 180) * MAP_HEIGHT
  }
}

/** "America/Argentina/Buenos_Aires" -> "Buenos Aires" */
export function cityOf(zone) {
  if (!zone) return ''
  const parts = String(zone).split('/')
  const city = parts[parts.length - 1] || zone
  return city.replaceAll('_', ' ')
}

/** "America/Argentina/Buenos_Aires" -> "America · Argentina" */
export function regionOf(zone) {
  if (!zone) return ''
  const parts = String(zone).split('/')
  if (parts.length < 2) return ''
  return parts.slice(0, -1).join(' · ').replaceAll('_', ' ')
}

const cityIndex = new Map(CITIES.map((city) => [city.zone, city]))

/** Full IANA identifier plus its labels and zone.tab coordinates. */
export function entryFor(zone) {
  const city = cityIndex.get(zone)
  return {
    zone,
    city: city ? city.city : cityOf(zone),
    region: regionOf(zone),
    country: city ? city.country : '',
    lat: city ? city.lat : null,
    lon: city ? city.lon : null
  }
}

/**
 * Every selectable city, ordered by zone. This is the picker's whole
 * catalogue: zone.tab is the set of places the tz database gives coordinates
 * for, so a user can always pick a real place rather than a bare offset.
 */
export const CITY_ENTRIES = CITIES.map((city) => entryFor(city.zone)).sort(
  (a, b) => (a.zone < b.zone ? -1 : a.zone > b.zone ? 1 : 0)
)

/**
 * SVG path for a zone's boundary rings. Boundaries are stored once per zone;
 * tiny enclaves (Vatican, Busingen) borrow the surrounding zone's polygon, and
 * `UTC` has none at all because it owns no land.
 */
export function zonePathFor(zone) {
  return ZONE_PATHS[zone] || ZONE_PATHS[ZONE_ALIASES[zone]] || ''
}

function distanceKm(lat1, lon1, lat2, lon2) {
  const dLat = (lat2 - lat1) * RADIANS
  const dLon = (lon2 - lon1) * RADIANS
  const a =
    Math.sin(dLat / 2) ** 2 +
    Math.cos(lat1 * RADIANS) * Math.cos(lat2 * RADIANS) * Math.sin(dLon / 2) ** 2
  return 2 * EARTH_RADIUS_KM * Math.asin(Math.min(1, Math.sqrt(a)))
}

/**
 * Nearest zone.tab city to a longitude/latitude pair, by great-circle
 * distance. `null` for a non-finite point. Used by map clicks: Ubuntu's
 * picker asks for the rough location and resolves it to a real city.
 */
export function nearestCity(lon, lat) {
  if (!Number.isFinite(lon) || !Number.isFinite(lat)) return null
  let best = null
  let bestDistance = Infinity
  for (const city of CITIES) {
    const distance = distanceKm(lat, lon, city.lat, city.lon)
    if (distance < bestDistance) {
      bestDistance = distance
      best = city
    }
  }
  if (!best) return null
  return { ...entryFor(best.zone), distanceKm: bestDistance }
}
