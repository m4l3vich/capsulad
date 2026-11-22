import { t } from './i18n.js'

// Device identity for the overview headline. Board revision, SoC vendor and
// software target are three separate facts: the profile names the product, the
// SoC names the silicon manufacturer, and the platform enum names the userland
// the daemon was built for. The headline states all three rather than letting
// the model probe stand in for the product.

/** First token of the reported SoC, mapped to the vendor spelling people recognize. */
function socBrand(soc) {
  const token = String(soc || '').trim().split(/\s+/)[0]?.toLowerCase() || ''
  if (token === 'rockchip') return 'Rockchip'
  if (token === 'amlogic') return 'Amlogic'
  // The simulator's SoC is "virtual"; both spellings mean "not real silicon".
  if (token === 'simulator' || token === 'virtual') return t('device.host')
  if (token) return token
  return t('device.unknownSoc')
}

/**
 * Software target from the platform enum. A missing enum falls back to the
 * firmware description; the enum itself is authoritative when present.
 */
function softwareType(device) {
  const platform = String(device?.targetPlatform || '')
  if (platform === 'CAPSULA_MINI_ROCKCHIP_STOCK' || platform === 'CAPSULA_MINI_AMLOGIC_STOCK') {
    return t('device.stock')
  }
  if (platform === 'CAPSULA_MINI_AMLOGIC_ALPINE') return t('device.alpine')
  if (platform === 'TARGET_PLATFORM_UNSPECIFIED' || platform === 'simulator') return t('device.simulator')
  if (platform) return String(device?.firmware || '').trim() || platform
  return String(device?.firmware || '').trim() || t('device.simulator')
}

/**
 * One-line headline: `Capsula-mini-4 (Rockchip, stock)`. The revision prefers
 * the profile's product name, then the probed model, then a placeholder so the
 * header is never blank.
 */
export function deviceHeadline(device) {
  const revision = String(device?.boardRevision || device?.model || '').trim() || t('common.unknownDevice')
  return `${revision} (${socBrand(device?.soc)}, ${softwareType(device)})`
}
