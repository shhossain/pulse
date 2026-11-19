<script>
  // One metric against another (chart.x), e.g. loss against learning rate.
  // Each dot is one moment of the run; "both" joins them in the order they
  // happened, so the path shows where the run went. SVG, not uPlot: uPlot
  // needs x to go up, and a scatter's x doesn't.
  import { getJSON } from '../lib/live.svelte.js'
  import { seriesColor } from '../lib/palette.js'
  import { num, duration, label, withUnit } from '../lib/format.js'
  import { savedStyle, saveStyle } from '../lib/chartstyle.js'
  import StyleSwitch from './StyleSwitch.svelte'
  import { untrack } from 'svelte'

  let { run, runId, seq, chart, compare = [], logScale = false, version = 0 } = $props()

  // pulse.yaml's style is the default; the switch's choice (kept in this browser) wins.
  let chosen = $state(untrack(() => savedStyle(run?.job, chart.name)))
  const style = $derived(chosen ?? (chart.style || 'points'))
  function pick(v) {
    chosen = v
    saveStyle(run?.job, chart.name, v, chart.style || 'points')
  }

  let sets = $state([]) // [{run, seq, key, keyIndex, x, y, t}]
  let tKind = $state('step')
  let width = $state(0)
  let hover = $state(null) // {set, i}
  const H = 200
  const M = { l: 48, r: 8, t: 6, b: 20 }

  async function load() {
    const q = chart.keys.map((k) => `key=${encodeURIComponent(k)}`).join('&')
    const runs = [{ id: runId, seq }, ...compare]
    const results = await Promise.all(
      runs.map((r) =>
        getJSON(`/api/v1/runs/${encodeURIComponent(r.id)}/series?x=${encodeURIComponent(chart.x)}&${q}&points=1500`)
          .then((set) => ({ r, set }))
          .catch(() => null),
      ),
    )
    const out = []
    for (const res of results) {
      if (!res) continue
      if (res.r.id === runId) tKind = res.set.x_kind
      for (const s of res.set.series) {
        out.push({ run: res.r.id, seq: res.r.seq, key: s.key, keyIndex: chart.keys.indexOf(s.key), x: s.x, y: s.y, t: s.t ?? [] })
      }
    }
    // The current run draws last, on top.
    sets = out.sort((a, b) => (a.run === runId ? 1 : 0) - (b.run === runId ? 1 : 0) || a.keyIndex - b.keyIndex)
  }
  $effect(() => {
    version; compare; chart.keys.join(); chart.x
    load()
  })

  // Nice round tick values (1, 2, 5 × 10^n) across [lo, hi].
  function ticks(lo, hi, count) {
    if (!(hi > lo)) return [lo]
    const step0 = (hi - lo) / count
    const mag = Math.pow(10, Math.floor(Math.log10(step0)))
    const step = [1, 2, 5, 10].map((m) => m * mag).find((s) => s >= step0)
    const out = []
    for (let v = Math.ceil(lo / step) * step; v <= hi + step * 1e-9; v += step) out.push(+v.toPrecision(12))
    return out
  }
  function logTicks(lo, hi) {
    const out = []
    for (let e = Math.floor(Math.log10(lo)); e <= Math.ceil(Math.log10(hi)); e++) {
      const v = Math.pow(10, e)
      if (v >= lo * 0.999 && v <= hi * 1.001) out.push(v)
    }
    return out.length >= 2 ? out : null
  }

  function axis(values, log, px0, px1) {
    let lo = Math.min(...values), hi = Math.max(...values)
    if (lo === hi) { lo -= Math.abs(lo) * 0.1 || 1; hi += Math.abs(hi) * 0.1 || 1 }
    if (log) {
      const [a, b] = [Math.log10(lo), Math.log10(hi)]
      const pad = (b - a) * 0.04
      const s = (v) => px0 + ((Math.log10(v) - (a - pad)) / (b - a + 2 * pad)) * (px1 - px0)
      return { s, ticks: logTicks(lo, hi) ?? ticks(lo, hi, 4) }
    }
    const pad = (hi - lo) * 0.04
    const s = (v) => px0 + ((v - (lo - pad)) / (hi - lo + 2 * pad)) * (px1 - px0)
    return { s, ticks: ticks(lo - pad, hi + pad, px1 > px0 ? Math.max(2, Math.floor(Math.abs(px1 - px0) / 80)) : 4) }
  }

  const geom = $derived.by(() => {
    const xs = sets.flatMap((s) => s.x), ys = sets.flatMap((s) => s.y)
    if (!xs.length || !width) return null
    // x spanning two orders of magnitude or more (learning rates, batch sizes): log it.
    const xMin = Math.min(...xs), xMax = Math.max(...xs)
    const xLog = xMin > 0 && xMax / xMin >= 100
    const yLog = logScale && ys.every((v) => v > 0)
    const X = axis(xs, xLog, M.l, width - M.r)
    const Y = axis(ys, yLog, H - M.b, M.t)
    const n = Math.max(...sets.map((s) => s.x.length))
    const r = n > 400 ? 2 : n > 100 ? 2.5 : 3.5
    const shapes = sets.map((s) => {
      const pts = s.x.map((x, i) => [X.s(x), Y.s(s.y[i])])
      return { ...s, pts, path: pts.map(([x, y], i) => `${i ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)}`).join(''), color: seriesColor(s.keyIndex), current: s.run === runId }
    })
    return { X, Y, xLog, yLog, r, shapes }
  })

  function onMove(e) {
    if (!geom) return
    const box = e.currentTarget.getBoundingClientRect()
    const mx = e.clientX - box.left, my = e.clientY - box.top
    let best = null, bestD = 24 * 24 // within 24px
    geom.shapes.forEach((s, si) => s.pts.forEach(([x, y], i) => {
      const d = (x - mx) ** 2 + (y - my) ** 2
      if (d < bestD) { bestD = d; best = { si, i } }
    }))
    hover = best
  }

  const hp = $derived(hover && geom ? geom.shapes[hover.si] : null)
  const tText = (t) => (tKind === 'time' ? duration(t * 1000) : `step ${num(t)}`)
  const fmtTick = (v) => (Math.abs(v) >= 1e4 || (v !== 0 && Math.abs(v) < 1e-3) ? v.toExponential(0) : num(v))
</script>

<section class="card">
  <header>
    <h3 data-tip={`Each dot pairs ${chart.keys.map((k) => label(k, run)).join(', ')} with ${label(chart.x, run)} at the same ${tKind === 'time' ? 'moment' : 'step'}.${style === 'both' ? '\nThe line joins them in the order they happened.' : ''}${geom?.xLog ? `\n${label(chart.x, run)} is on a log scale.` : ''}`}>
      {chart.title || `${chart.keys.map((k) => label(k, run)).join(', ')} vs ${label(chart.x, run)}`}
    </h3>
    <span class="right">
      {#if hp}<span class="at">{tText(hp.t[hover.i])}{hp.current ? '' : ` · #${hp.seq}`}</span>{/if}
      <StyleSwitch value={style} onpick={pick} />
    </span>
  </header>
  <div class="legend">
    {#each geom?.shapes.filter((s) => s.current) ?? [] as s (s.key)}
      <span class="item">
        <svg width="10" height="10" aria-hidden="true"><circle cx="5" cy="5" r="3.5" fill={s.color} /></svg>
        {label(s.key, run)}
        <b>{hp && hp.key === s.key && hp.current ? withUnit(hp.y[hover.i], s.key, run) : s.y.length ? withUnit(s.y[s.y.length - 1], s.key, run) : '–'}</b>
      </span>
    {/each}
    {#if compare.length}<span class="item dim" data-tip="Earlier runs are drawn as rings">○ earlier runs</span>{/if}
    <span class="item dim">{label(chart.x, run)} <b>{hp ? withUnit(hp.x[hover.i], chart.x, run) : ''}</b></span>
  </div>
  <div class="plot" bind:clientWidth={width}>
    {#if geom}
      <svg {width} height={H} onmousemove={onMove} onmouseleave={() => (hover = null)} role="img"
        aria-label={`${chart.keys.join(', ')} against ${chart.x}`}>
        {#each geom.Y.ticks as v (v)}
          <line class="grid" x1={M.l} x2={width - M.r} y1={geom.Y.s(v)} y2={geom.Y.s(v)} />
          <text class="tick" x={M.l - 6} y={geom.Y.s(v)} text-anchor="end" dominant-baseline="middle">{fmtTick(v)}</text>
        {/each}
        {#each geom.X.ticks as v (v)}
          <line class="grid" x1={geom.X.s(v)} x2={geom.X.s(v)} y1={M.t} y2={H - M.b} />
          <text class="tick" x={geom.X.s(v)} y={H - 4} text-anchor="middle">{fmtTick(v)}</text>
        {/each}
        {#each geom.shapes as s (s.run + s.key)}
          {#if style !== 'points'}
            <path d={s.path} fill="none" stroke={s.color} stroke-width={s.current ? 1.5 : 1} opacity={s.current ? 0.9 : 0.45}
              stroke-linejoin="round" stroke-dasharray={s.current ? null : '4 3'} />
          {/if}
          {#if style !== 'line'}
            {#each s.pts as [x, y], i (i)}
              {#if s.current}
                <circle cx={x} cy={y} r={geom.r} fill={s.color} stroke="var(--card-solid)" stroke-width="0.75" />
              {:else}
                <circle cx={x} cy={y} r={geom.r} fill="none" stroke={s.color} stroke-width="1" opacity="0.6" />
              {/if}
            {/each}
          {/if}
        {/each}
        {#if hp}
          {@const [x, y] = hp.pts[hover.i]}
          <circle cx={x} cy={y} r={geom.r + 3} fill="none" stroke={hp.color} stroke-width="2" />
          <circle cx={x} cy={y} r={geom.r + 4.5} fill="none" stroke="var(--card-solid)" stroke-width="1.5" />
        {/if}
      </svg>
    {:else if sets.length && !sets.some((s) => s.x.length)}
      <p class="note">No points yet: {label(chart.x, run)} hasn't been logged alongside {chart.keys.map((k) => label(k, run)).join(', ')}.</p>
    {/if}
  </div>
</section>

<style>
  .card {
    background: var(--card-solid);
    border: 0.5px solid var(--border);
    border-radius: 12px;
    padding: 12px 14px 8px;
    min-width: 0;
  }
  header { display: flex; justify-content: space-between; align-items: center; gap: 8px; }
  .right { display: flex; align-items: center; gap: 10px; }
  h3 { margin: 0; font-size: 13px; font-weight: 600; }
  .at { font-size: 11px; color: var(--muted); font-variant-numeric: tabular-nums; }
  .legend { display: flex; flex-wrap: wrap; gap: 4px 14px; margin: 6px 0 4px; font-size: 12px; color: var(--fg-2); }
  .item { display: inline-flex; align-items: center; gap: 6px; }
  .item.dim { color: var(--muted); }
  .item b { font-weight: 500; font-variant-numeric: tabular-nums; color: var(--fg); }
  .plot { width: 100%; min-height: 200px; }
  svg { display: block; overflow: visible; }
  .grid { stroke: var(--grid); stroke-width: 1; }
  .tick { fill: var(--muted); font: 11px -apple-system, system-ui, sans-serif; font-variant-numeric: tabular-nums; }
  .note { margin: 70px 0 0; text-align: center; font-size: 12px; color: var(--muted); }
</style>
