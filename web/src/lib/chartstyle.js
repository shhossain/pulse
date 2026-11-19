// The line / dots / both choice per chart, remembered in this browser.
// pulse.yaml's `style:` is the default; the switch on the chart overrides it.
const key = (job, chart) => `pulse.style.${job}.${chart}`

export function savedStyle(job, chart) {
  try {
    const v = localStorage.getItem(key(job, chart))
    return v === 'line' || v === 'points' || v === 'both' ? v : null
  } catch {
    return null
  }
}

export function saveStyle(job, chart, style, fallback) {
  try {
    if (style === fallback) localStorage.removeItem(key(job, chart))
    else localStorage.setItem(key(job, chart), style)
  } catch {
    // Private window or blocked storage: the choice lasts for this page only.
  }
}
