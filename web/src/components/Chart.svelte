<script>
  import uPlot from 'uplot'
  import 'uplot/dist/uPlot.min.css'
  import { onDestroy, untrack } from 'svelte'
  import { getJSON } from '../lib/live.svelte.js'
  import { seriesColor, cssVar } from '../lib/palette.js'
  import { num, duration, label, withUnit } from '../lib/format.js'
  import { savedStyle, saveStyle } from '../lib/chartstyle.js'
  import StyleSwitch from './StyleSwitch.svelte'

  // chart: {name, keys}; compare: [{id, seq}] other runs overlaid (dashed).
  let { runId, seq, chart, compare = [], smoothing = 0, logScale = false, version = 0, run = null } = $props()

  let el = $state()
  let plot = null
  let data = $state(null) // {xKind, sets: [{run, seq, key, keyIndex, x, y}]}
  let hoverIdx = $state(null)
  let canLog = $state(true)

  // pulse.yaml's style is the default; the switch's choice (kept in this browser) wins.
  let chosen = $state(untrack(() => savedStyle(run?.job, chart.name)))
  const style = $derived(chosen ?? (chart.style || 'line'))
  function pick(v) {
    chosen = v
    saveStyle(run?.job, chart.name, v, chart.style || 'line')
  }

  let skipped = $state([]) // compared runs on a different x axis
  async function load() {
    const keys = chart.keys.map((k) => `key=${encodeURIComponent(k)}`).join('&')
    const runs = [{ id: runId, seq }, ...compare]
    const results = await Promise.all(
      runs.map((r) =>
        getJSON(`/api/v1/runs/${encodeURIComponent(r.id)}/series?${keys}&points=1500`)
          .then((set) => ({ r, set }))
          .catch(() => null),
      ),
    )
    const sets = []
    const mine = results.find((res) => res?.r.id === runId)
    const xKind = mine?.set.x_kind ?? 'step'
    const off = []
    for (const res of results) {
      if (!res) continue
      if (res.set.x_kind !== xKind) {
        off.push(res.r.seq) // steps and seconds can't share an axis
        continue
      }
      res.set.series.forEach((s) => {
        sets.push({ run: res.r.id, seq: res.r.seq, key: s.key, keyIndex: chart.keys.indexOf(s.key), x: s.x, y: s.y })
      })
    }
    skipped = off
    data = { xKind, sets }
  }

  // Refetch when the run updates (throttled by the parent) or compare changes.
  $effect(() => {
    version; compare; chart.keys.join()
    load()
  })

  // Each compared run gets its own dash pattern (identity is never color-alone).
  const DASHES = [[6, 4], [2, 3], [10, 3, 2, 3]]
  const dashFor = (s) => (s.run === runId ? undefined : DASHES[compare.findIndex((c) => c.id === s.run) % DASHES.length])

  function ema(ys, w) {
    if (!w) return ys
    let last = 0, n = 0
    return ys.map((y) => {
      if (y == null || !Number.isFinite(y)) return y
      last = last * w + (1 - w) * y
      n++
      return last / (1 - Math.pow(w, n)) // debiased, like TensorBoard
    })
  }

  // Ordered so the current run's series come first (legend + colors).
  const ordered = $derived(
    data ? [...data.sets].sort((a, b) => (a.run === runId ? 0 : 1) - (b.run === runId ? 0 : 1) || a.keyIndex - b.keyIndex) : [],
  )

  let zoomed = false // the user dragged to zoom; keep that view across refreshes
  let shape = ''

  function build() {
    if (!el || !data) return
    const nextShape = `${smoothing}|${logScale}|${style}|${ordered.map((s) => s.run + s.key).join(',')}`
    if (plot && nextShape === shape && plot._refresh) {
      plot._refresh() // same series: swap the data, keep zoom and hover
      return
    }
    shape = nextShape
    plot?.destroy()
    plot = null
    if (!ordered.length || ordered.every((s) => !s.x.length)) return

    const aligned = uPlot.join(ordered.map((s) => [s.x, s.y]))
    canLog = ordered.every((s) => s.y.every((v) => v > 0))
    const useLog = logScale && canLog
    const raw = aligned.slice(1)
    const smooth = raw.map((ys) => ema(ys, smoothing))

    const series = [{}]
    const values = [aligned[0]]
    const n = Math.max(...ordered.map((s) => s.x.length))
    const dotSize = n > 400 ? 3 : n > 100 ? 4 : 6
    const dots = style !== 'line'
    const look = (color, width) => ({
      ...(style === 'points' ? { paths: () => null } : {}),
      points: dots ? { show: true, size: dotSize, width: 0, fill: color, stroke: color } : { show: false },
      width,
    })
    ordered.forEach((s, i) => {
      const color = seriesColor(s.keyIndex)
      const current = s.run === runId
      const smoothed = smoothing > 0 && current
      if (smoothed) {
        // Raw values under the smoothed line (current run only, to keep comparisons
        // readable): a faint line, or the dots when the chart shows dots.
        series.push({ stroke: color + (dots ? '90' : '40'), spanGaps: true, ...look(color + '90', 1) })
        values.push(raw[i])
      }
      const c = current ? color : color + 'b0'
      series.push({
        label: current ? s.key : `${s.key} #${s.seq}`,
        stroke: c,
        dash: dashFor(s),
        spanGaps: true,
        ...(smoothed ? { width: 2, points: { show: false } } : look(c, current ? (style === 'both' ? 1.5 : 2) : 1.5)),
      })
      values.push(smooth[i])
    })

    const axis = {
      stroke: cssVar('--muted'),
      grid: { stroke: cssVar('--grid'), width: 1 },
      ticks: { show: false },
      font: '11px -apple-system, system-ui, sans-serif',
    }
    // System charts start at zero (a busy CPU shouldn't look jumpy); percentages run to 100.
    const pct = chart.keys.some((k) => k === 'sys.cpu' || k === 'sys.gpu')
    const systemRange = (u, min, max) => [0, Math.max(pct ? 100 : 0, (max || 1) * 1.1)]
    const xFmt = data.xKind === 'time' ? (v) => duration(v * 1000) : (v) => num(v)
    plot = new uPlot(
      {
        width: el.clientWidth,
        height: 200,
        legend: { show: false },
        cursor: { y: false, points: { size: 7 }, drag: { x: true, y: false } },
        scales: { x: { time: false }, y: { distr: useLog ? 3 : 1, ...(chart.group === 'system' && !useLog ? { range: systemRange } : {}) } },
        axes: [
          { ...axis, values: (u, vals) => vals.map(xFmt) },
          { ...axis, size: 48, values: (u, vals) => vals.map((v) => (v == null ? '' : num(v))) },
        ],
        series,
        hooks: {
          setCursor: [(u) => (hoverIdx = u.cursor.idx)],
          setSelect: [(u) => { if (u.select.width > 0) zoomed = true }],
        },
      },
      values,
      el,
    )
    plot._values = values
    plot._ordered = ordered
    // Re-run the data part of this build for new points (same series).
    plot._refresh = () => {
      const fresh = uPlot.join(ordered.map((s) => [s.x, s.y]))
      const fr = fresh.slice(1)
      const vals = [fresh[0]]
      ordered.forEach((s, i) => {
        if (smoothing > 0 && s.run === runId) vals.push(fr[i])
        vals.push(ema(fr[i], smoothing))
      })
      plot._values = vals
      plot.setData(vals, !zoomed)
    }
    el.ondblclick = () => (zoomed = false)
  }

  $effect(() => {
    data; smoothing; logScale; style
    build()
  })

  // Resize with the container; rebuild when light/dark changes.
  $effect(() => {
    if (!el) return
    const ro = new ResizeObserver(() => plot?.setSize({ width: el.clientWidth, height: 200 }))
    ro.observe(el)
    const mq = matchMedia('(prefers-color-scheme: dark)')
    const onScheme = () => build()
    mq.addEventListener('change', onScheme)
    return () => {
      ro.disconnect()
      mq.removeEventListener('change', onScheme)
    }
  })
  onDestroy(() => plot?.destroy())

  // Legend rows: value under the cursor, else the latest value.
  const legend = $derived.by(() => {
    if (!data) return []
    return ordered.map((s, i) => {
      let v = null
      let x = null
      if (hoverIdx != null && plot?._values) {
        // Columns: x, then per series [raw (current run, when smoothing)], value.
        let col = 1
        for (let j = 0; j <= i; j++) {
          if (smoothing > 0 && ordered[j].run === runId) col++
          if (j < i) col++
        }
        v = plot._values[col]?.[hoverIdx]
        x = plot._values[0]?.[hoverIdx]
      } else if (s.y.length) {
        v = s.y[s.y.length - 1]
      }
      return { ...s, v, x, color: seriesColor(s.keyIndex), current: s.run === runId, dash: dashFor(s)?.join(' ') ?? '' }
    })
  })
  const hoverX = $derived(legend.find((l) => l.x != null)?.x)
</script>

<section class="card">
  <header>
    <h3 data-tip={chart.keys.length > 1 ? `Plots: ${chart.keys.map((k) => label(k, run)).join(', ')}` : null}>{chart.title || label(chart.name, run)}</h3>
    <span class="right">
      {#if hoverX != null}
        <span class="at">{data?.xKind === 'time' ? duration(hoverX * 1000) : `step ${num(hoverX)}`}</span>
      {/if}
      <StyleSwitch value={style} onpick={pick} />
    </span>
  </header>
  <div class="legend">
    {#each legend as l (l.run + l.key)}
      <span class="item" class:dim={!l.current} data-tip={`${l.current ? 'This run' : `Run #${l.seq}`}: ${label(l.key, run)}${label(l.key, run) !== l.key ? ` (${l.key})` : ''}\n${hoverX != null ? 'Value at the cursor' : 'Latest value'}${smoothing > 0 ? ', smoothed' : ''}`}>
        <svg width="18" height="8" aria-hidden="true">
          <line x1="0" y1="4" x2="18" y2="4" stroke={l.color} stroke-width="2" stroke-dasharray={l.dash} />
        </svg>
        {label(l.key, run)}{#if !l.current}<span class="seq">#{l.seq}</span>{/if}
        <b>{l.v == null ? '–' : withUnit(l.v, l.key, run)}</b>
      </span>
    {/each}
  </div>
  <div class="plot" bind:this={el}></div>
  {#if logScale && !canLog}<p class="note">Log scale needs positive values.</p>{/if}
  {#if skipped.length}<p class="note">Not shown: {skipped.map((n) => `#${n}`).join(', ')} (logged {data?.xKind === 'step' ? 'without steps' : 'by step'}, so the x axes don't match).</p>{/if}
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
  .seq { color: var(--muted); margin-left: 2px; }
  .plot { width: 100%; min-height: 200px; }
  .note { margin: 4px 0; font-size: 11px; color: var(--muted); }
  .plot :global(.u-select) { background: color-mix(in srgb, var(--blue) 12%, transparent); }
  .plot :global(.u-cursor-x), .plot :global(.u-cursor-y) { border-color: var(--muted) !important; }
</style>
