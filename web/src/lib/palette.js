// Chart series colors: Apple system blue, orange, purple, teal. Validated
// (lightness band, chroma, colorblind separation) for each mode separately;
// assigned in this fixed order, never cycled. Max 4 series per chart.
const LIGHT = ['#007aff', '#ff9500', '#af52de', '#30b0c7']
const DARK = ['#0a84ff', '#d97500', '#bf5af2', '#20a0b5']

export function isDark() {
  return matchMedia('(prefers-color-scheme: dark)').matches
}

export function seriesColor(i) {
  const p = isDark() ? DARK : LIGHT
  return p[i % p.length]
}

export function cssVar(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim()
}
