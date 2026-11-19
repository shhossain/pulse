<script>
  import { frac, progressText, etaText, label, num } from '../lib/format.js'

  let { p, now, primary = false, tone = 'running', run = null } = $props()
  const pct = $derived(Math.round(frac(p) * 1000) / 10)
  const eta = $derived(primary ? etaText(p, now) : '')
  const tip = $derived.by(() => {
    const lines = [`${label(p.name, run)}: ${num(p.cur)} of ${num(p.total)} (${Math.round(pct)}%)`]
    if (p.eta && Date.parse(p.eta) > now) {
      lines.push(`Estimated finish ${new Date(p.eta).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}, from its recent pace`)
    } else if (tone === 'running' && pct < 100) {
      lines.push('Estimate appears once it has moved a couple of times')
    }
    if (label(p.name, run) !== p.name && p.name !== 'main') lines.push(`Key: ${p.name}`)
    return lines.join('\n')
  })
</script>

<div class="bar" class:primary data-tip={tip}>
  <div class="row">
    <span class="name">{label(p.name, run)}</span>
    <span class="val">
      {#if eta}<span class="eta">{eta}</span>{/if}
      {progressText(p)}{#if !(p.name === 'main' && p.total === 100)}<span class="pct">{Math.round(pct)}%</span>{/if}
    </span>
  </div>
  <div class="track"><div class="fill {tone}" style:width="{pct}%"></div></div>
</div>

<style>
  .bar { margin-top: 8px; }
  .row { display: flex; justify-content: space-between; align-items: baseline; gap: 8px; font-size: 11px; color: var(--muted); margin-bottom: 4px; }
  .name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .val { font-variant-numeric: tabular-nums; white-space: nowrap; color: var(--fg-2); }
  .pct { color: var(--muted); margin-left: 6px; }
  .eta { color: var(--muted); margin-right: 8px; }
  .track { height: 3px; border-radius: 2px; background: var(--track); overflow: hidden; }
  .primary .track { height: 6px; border-radius: 3px; }
  .primary .row { font-size: 12px; }
  .fill { height: 100%; border-radius: inherit; background: var(--blue); transition: width 0.4s ease; }
  .fill.done { background: var(--green); }
  .fill.failed { background: var(--red); }
  .fill.lost, .fill.idle { background: var(--gray); }
</style>
