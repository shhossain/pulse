<script>
  // A tiny trend line for the popover card (no axes; the chip shows the value).
  import { num } from '../lib/format.js'

  let { ys = [], height = 26, color = 'var(--blue)', tip = '' } = $props()
  const W = 100

  const geom = $derived.by(() => {
    const pts = ys.filter((y) => Number.isFinite(y))
    if (pts.length < 2) return null
    let lo = Math.min(...pts), hi = Math.max(...pts)
    if (hi === lo) { hi += 1; lo -= 1 }
    const pad = 2
    const xy = pts.map((y, i) => [
      (i / (pts.length - 1)) * W,
      pad + (1 - (y - lo) / (hi - lo)) * (height - pad * 2),
    ])
    const line = xy.map(([x, y], i) => `${i ? 'L' : 'M'}${x.toFixed(2)},${y.toFixed(2)}`).join('')
    return { line, area: `${line}L${W},${height}L0,${height}Z`, last: xy[xy.length - 1], lo: Math.min(...pts), hi: Math.max(...pts) }
  })
</script>

{#if geom}
  <svg data-tip={tip ? `${tip}\nRange ${num(geom.lo)} – ${num(geom.hi)}` : null} viewBox="0 0 {W} {height}" preserveAspectRatio="none" style:height="{height}px" aria-hidden="true">
    <path d={geom.area} fill={color} opacity="0.1" />
    <path d={geom.line} fill="none" stroke={color} stroke-width="1.5" vector-effect="non-scaling-stroke" stroke-linejoin="round" />
  </svg>
{/if}

<style>
  svg { display: block; width: 100%; margin-top: 8px; overflow: visible; }
</style>
