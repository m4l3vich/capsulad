// Time zone map data for the settings picker.
//
// Each entry is a representative city marker placed by latitude/longitude on
// an equirectangular world map. The value the device stores is always the full
// IANA identifier (`Europe/Moscow`), never the city label.
//
// Projection: x = (lon + 180) / 360 * MAP_WIDTH, y = (90 - lat) / 180 * MAP_HEIGHT

export const MAP_WIDTH = 360
export const MAP_HEIGHT = 180

export function projectPoint(lat, lon) {
  return {
    x: ((lon + 180) / 360) * MAP_WIDTH,
    y: ((90 - lat) / 180) * MAP_HEIGHT
  }
}

// "America/Argentina/Buenos_Aires" -> "Buenos Aires"
export function cityOf(zone) {
  if (!zone) return ''
  const parts = String(zone).split('/')
  const city = parts[parts.length - 1] || zone
  return city.replaceAll('_', ' ')
}

// "America/Argentina/Buenos_Aires" -> "America · Argentina"
export function regionOf(zone) {
  if (!zone) return ''
  const parts = String(zone).split('/')
  if (parts.length < 2) return ''
  return parts.slice(0, -1).join(' · ').replaceAll('_', ' ')
}

export const MARKERS = [
  { zone: 'UTC', lat: 0, lon: 0 },

  // Europe
  { zone: 'Europe/London', lat: 51.51, lon: -0.13 },
  { zone: 'Europe/Lisbon', lat: 38.72, lon: -9.14 },
  { zone: 'Europe/Madrid', lat: 40.42, lon: -3.7 },
  { zone: 'Europe/Paris', lat: 48.86, lon: 2.35 },
  { zone: 'Europe/Amsterdam', lat: 52.37, lon: 4.9 },
  { zone: 'Europe/Berlin', lat: 52.52, lon: 13.4 },
  { zone: 'Europe/Rome', lat: 41.9, lon: 12.5 },
  { zone: 'Europe/Warsaw', lat: 52.23, lon: 21.01 },
  { zone: 'Europe/Stockholm', lat: 59.33, lon: 18.07 },
  { zone: 'Europe/Helsinki', lat: 60.17, lon: 24.94 },
  { zone: 'Europe/Athens', lat: 37.98, lon: 23.73 },
  { zone: 'Europe/Bucharest', lat: 44.43, lon: 26.1 },
  { zone: 'Europe/Kyiv', lat: 50.45, lon: 30.52 },
  { zone: 'Europe/Istanbul', lat: 41.01, lon: 28.98 },
  { zone: 'Europe/Moscow', lat: 55.76, lon: 37.62 },
  { zone: 'Atlantic/Reykjavik', lat: 64.15, lon: -21.94 },

  // Africa
  { zone: 'Africa/Casablanca', lat: 33.57, lon: -7.59 },
  { zone: 'Africa/Algiers', lat: 36.75, lon: 3.06 },
  { zone: 'Africa/Cairo', lat: 30.04, lon: 31.24 },
  { zone: 'Africa/Dakar', lat: 14.72, lon: -17.47 },
  { zone: 'Africa/Abidjan', lat: 5.36, lon: -4.01 },
  { zone: 'Africa/Lagos', lat: 6.52, lon: 3.38 },
  { zone: 'Africa/Luanda', lat: -8.84, lon: 13.23 },
  { zone: 'Africa/Kinshasa', lat: -4.44, lon: 15.27 },
  { zone: 'Africa/Nairobi', lat: -1.29, lon: 36.82 },
  { zone: 'Africa/Johannesburg', lat: -26.2, lon: 28.05 },

  // Middle East and Central Asia
  { zone: 'Asia/Jerusalem', lat: 31.77, lon: 35.21 },
  { zone: 'Asia/Riyadh', lat: 24.71, lon: 46.68 },
  { zone: 'Asia/Tehran', lat: 35.69, lon: 51.39 },
  { zone: 'Asia/Dubai', lat: 25.2, lon: 55.27 },
  { zone: 'Asia/Karachi', lat: 24.86, lon: 67.01 },
  { zone: 'Asia/Yekaterinburg', lat: 56.84, lon: 60.61 },
  { zone: 'Asia/Tashkent', lat: 41.3, lon: 69.24 },
  { zone: 'Asia/Almaty', lat: 43.24, lon: 76.95 },
  { zone: 'Asia/Novosibirsk', lat: 55.03, lon: 82.92 },

  // South and East Asia
  { zone: 'Asia/Kolkata', lat: 22.57, lon: 88.36 },
  { zone: 'Asia/Kathmandu', lat: 27.72, lon: 85.32 },
  { zone: 'Asia/Dhaka', lat: 23.81, lon: 90.41 },
  { zone: 'Asia/Yangon', lat: 16.87, lon: 96.2 },
  { zone: 'Asia/Bangkok', lat: 13.76, lon: 100.5 },
  { zone: 'Asia/Jakarta', lat: -6.21, lon: 106.85 },
  { zone: 'Asia/Singapore', lat: 1.35, lon: 103.82 },
  { zone: 'Asia/Manila', lat: 14.6, lon: 120.98 },
  { zone: 'Asia/Hong_Kong', lat: 22.32, lon: 114.17 },
  { zone: 'Asia/Shanghai', lat: 31.23, lon: 121.47 },
  { zone: 'Asia/Taipei', lat: 25.03, lon: 121.57 },
  { zone: 'Asia/Seoul', lat: 37.57, lon: 126.98 },
  { zone: 'Asia/Tokyo', lat: 35.68, lon: 139.69 },
  { zone: 'Asia/Vladivostok', lat: 43.12, lon: 131.89 },

  // Oceania
  { zone: 'Australia/Perth', lat: -31.95, lon: 115.86 },
  { zone: 'Australia/Darwin', lat: -12.46, lon: 130.84 },
  { zone: 'Australia/Adelaide', lat: -34.93, lon: 138.6 },
  { zone: 'Australia/Brisbane', lat: -27.47, lon: 153.03 },
  { zone: 'Australia/Sydney', lat: -33.87, lon: 151.21 },
  { zone: 'Australia/Melbourne', lat: -37.81, lon: 144.96 },
  { zone: 'Pacific/Auckland', lat: -36.85, lon: 174.76 },

  // North America
  { zone: 'America/Anchorage', lat: 61.22, lon: -149.9 },
  { zone: 'America/Vancouver', lat: 49.28, lon: -123.12 },
  { zone: 'America/Los_Angeles', lat: 34.05, lon: -118.24 },
  { zone: 'America/Denver', lat: 39.74, lon: -104.99 },
  { zone: 'America/Phoenix', lat: 33.45, lon: -112.07 },
  { zone: 'America/Chicago', lat: 41.88, lon: -87.63 },
  { zone: 'America/Mexico_City', lat: 19.43, lon: -99.13 },
  { zone: 'America/New_York', lat: 40.71, lon: -74.01 },
  { zone: 'America/Toronto', lat: 43.65, lon: -79.38 },
  { zone: 'America/Halifax', lat: 44.65, lon: -63.58 },
  { zone: 'America/Havana', lat: 23.11, lon: -82.37 },
  { zone: 'America/Panama', lat: 8.98, lon: -79.52 },

  // South America
  { zone: 'America/Bogota', lat: 4.71, lon: -74.07 },
  { zone: 'America/Caracas', lat: 10.49, lon: -66.88 },
  { zone: 'America/Lima', lat: -12.05, lon: -77.04 },
  { zone: 'America/La_Paz', lat: -16.5, lon: -68.15 },
  { zone: 'America/Santiago', lat: -33.45, lon: -70.67 },
  { zone: 'America/Asuncion', lat: -25.26, lon: -57.58 },
  { zone: 'America/Sao_Paulo', lat: -23.55, lon: -46.63 },
  { zone: 'America/Argentina/Buenos_Aires', lat: -34.6, lon: -58.38 },
  { zone: 'America/Montevideo', lat: -34.9, lon: -56.16 }
]

const markerIndex = new Map(MARKERS.map((marker) => [marker.zone, marker]))

/** Marker entries with derived labels, ordered by zone for browsing. */
export const MARKER_ENTRIES = MARKERS
  .map((marker) => entryFor(marker.zone))
  .sort((a, b) => a.zone.localeCompare(b.zone))

/** Full IANA identifier and derived labels for any zone string. */
export function entryFor(zone) {
  const mark = markerIndex.get(zone)
  return {
    zone,
    city: cityOf(zone),
    region: regionOf(zone),
    lat: mark ? mark.lat : null,
    lon: mark ? mark.lon : null
  }
}

/**
 * The browser's zone catalogue plus the curated map markers, so a marker never
 * points at a zone the list cannot offer. This is a client-side suggestion
 * list, not the device's authority: the device validates a zone when the
 * setting is applied. Entries are plain objects; only the curated markers have
 * coordinates.
 */
export function zoneCatalogue() {
  const zones = new Set()
  try {
    const supported = Intl.supportedValuesOf?.('timeZone')
    if (Array.isArray(supported)) for (const zone of supported) zones.add(zone)
  } catch (_) {
    // Older engines have no catalogue; the curated markers still cover offsets.
  }
  zones.add('UTC')
  for (const marker of MARKERS) zones.add(marker.zone)
  return [...zones].sort((a, b) => a.localeCompare(b)).map(entryFor)
}

/** Markers projected into viewBox coordinates. */
export const PROJECTED_MARKERS = MARKER_ENTRIES
  .filter((entry) => entry.lat !== null)
  .map((entry) => ({ ...entry, ...projectPoint(entry.lat, entry.lon) }))

// Simplified continents as [lon, lat] vertices. The shapes are deliberately
// coarse: they orient the user, the markers carry the information.
const LANDMASSES = [
  // North America
  [
    [-168, 66], [-165, 60], [-155, 58], [-145, 60], [-135, 57], [-125, 49],
    [-124, 40], [-117, 32], [-113, 29], [-110, 24], [-105, 20], [-97, 16],
    [-92, 14], [-88, 16], [-87, 21], [-90, 25], [-97, 26], [-93, 30],
    [-89, 29], [-82, 25], [-80, 27], [-76, 35], [-70, 42], [-66, 45],
    [-55, 50], [-57, 55], [-64, 60], [-70, 68], [-82, 73], [-95, 74],
    [-110, 73], [-125, 71], [-140, 70], [-160, 71]
  ],
  // Greenland
  [
    [-45, 60], [-25, 68], [-20, 74], [-25, 80], [-40, 83], [-58, 82],
    [-55, 72], [-50, 64]
  ],
  // Nova Scotia
  [[-66, 45], [-62, 46], [-60, 45], [-63, 43.5], [-66, 44]],
  // Central America
  [[-88, 18], [-84, 15], [-80, 9.5], [-77, 8], [-80, 8], [-87, 12.5], [-92, 15.5]],
  // South America
  [
    [-81, 8], [-76, 11], [-70, 12], [-62, 11], [-52, 5], [-44, 0],
    [-35, -6], [-38, -13], [-40, -22], [-48, -26], [-53, -33], [-56, -35.2],
    [-58, -35], [-62, -40], [-65, -47], [-68, -55], [-75, -52], [-73, -44],
    [-71, -33], [-70, -23], [-71, -18], [-76, -14], [-81, -5], [-80, 0]
  ],
  // Eurasia
  [
    [-10, 44], [-9, 37], [-6, 36], [0, 39], [3, 42], [7, 44], [12, 44],
    [15, 40], [18, 40], [23, 38], [26, 40], [29, 41], [33, 36], [36, 36],
    [35, 32], [34, 29], [43, 12], [48, 15], [52, 17], [57, 23], [60, 25],
    [67, 24], [72, 21], [77, 8], [80, 13], [85, 20], [90, 22], [94, 16],
    [98, 8], [104, 1], [107, 10], [110, 20], [117, 23], [122, 31],
    [122, 40], [127, 35], [129, 43], [133, 42], [136, 45], [142, 54], [140, 60],
    [160, 60], [170, 66], [180, 66], [180, 70], [160, 72], [140, 75],
    [120, 77], [100, 78], [80, 76], [60, 72], [45, 70], [30, 70],
    [25, 70], [18, 69], [12, 64], [5, 61], [0, 58], [-5, 58], [-5, 53],
    [-8, 52], [-10, 46]
  ],
  // Africa
  [
    [-17.6, 14.7], [-16.5, 20.5], [-13, 28], [-9, 30], [-8.5, 33], [-5, 36],
    [0, 37], [10, 37], [11, 33], [20, 32], [25, 32], [32, 31], [34, 29],
    [43, 12], [51, 12], [41, -1], [40, -15], [35, -24], [32, -29],
    [25, -34], [18, -34], [12, -18], [8, -1], [9, 4], [5, 5],
    [-4, 5], [-8, 4], [-13, 8], [-16, 12]
  ],
  // Italy and Greece
  [[7, 44], [12, 45], [14, 42], [18, 40], [16.5, 38.5], [12.5, 41.4], [9, 44]],
  [[20, 40], [23, 41], [26, 40], [24, 36.5], [22, 36.5], [20, 38.5]],
  // Iceland
  [[-24, 65], [-18, 66.5], [-14, 65.5], [-18, 63.5], [-22, 63.8]],
  // Madagascar
  [[43, -12], [50, -15], [48, -25], [44, -25], [43, -19]],
  // British Isles
  [[-6, 50], [-5, 55], [-3, 58], [0, 54], [1, 51]],
  [[-10, 52], [-9, 55], [-6, 55], [-6, 52]],
  // Japan
  [
    [131, 31.5], [135, 33.5], [139, 34.2], [141.5, 38], [142, 41],
    [145, 43], [143, 44], [140, 40.5], [137, 36.8], [133.5, 35.3], [131.5, 33.5]
  ],
  // Taiwan, the Philippines, Sumatra, Java, Borneo
  [[120, 25.5], [122, 25.5], [122, 22], [120, 22]],
  [[119, 19], [123, 19], [126, 8], [122, 6], [119, 10]],
  [[95, 5], [100, 0], [105, -5], [104, -6], [97, -1], [93, 3]],
  [[105, -5.5], [114, -6], [114, -9], [105, -8.5]],
  [[109, 2], [117, 4], [119, -1], [116, -4], [110, -3]],
  // Cuba
  [[-85, 23.2], [-80, 23.3], [-74, 20.5], [-77, 20], [-84, 21.5]],
  // Australia
  [
    [114, -22], [113, -26], [115, -34], [118, -35], [126, -32], [133, -32],
    [138, -35], [141, -38], [146, -39], [150, -37], [152, -33], [153.5, -28],
    [152.5, -24], [149, -21], [145, -15], [142, -11], [136, -12], [130, -12],
    [126, -14], [122, -18], [115, -21]
  ],
  // Tasmania
  [[145, -41], [148, -41], [148, -43], [145, -43]],
  // New Zealand
  [[173, -35], [178, -38], [176, -41], [171, -44], [167, -46], [170, -43], [172, -39]],
  // Antarctica
  [
    [-180, -70], [-140, -72], [-100, -73], [-60, -70], [-20, -70], [20, -68],
    [60, -67], [100, -70], [140, -68], [180, -70], [180, -90], [-180, -90]
  ]
]

function outlinePath(points) {
  return points
    .map(([lon, lat], index) => {
      const { x, y } = projectPoint(lat, lon)
      return `${index === 0 ? 'M' : 'L'}${x.toFixed(2)} ${y.toFixed(2)}`
    })
    .join(' ') + ' Z'
}

export const LAND_PATHS = LANDMASSES.map(outlinePath)
