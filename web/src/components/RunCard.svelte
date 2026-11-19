<script>
  import Bar from './Bar.svelte'
  import Sparkline from './Sparkline.svelte'
  import StopButton from './StopButton.svelte'
  import Asks from './Asks.svelte'
  import { num, duration, ago, orderedMetrics, label, withUnit, statusTip, systemStats } from '../lib/format.js'
  import { act, copy, openPage, getJSON, session } from '../lib/live.svelte.js'

  let { run, now } = $props()
  // A followed hub's run: shown here, acted on at the hub (↗ opens it there).
  const canAct = $derived(!session.readOnly && !run.hub)
  let expanded = $state(false)
  let copied = $state(false)

  const attention = $derived(run.status === 'failed' || run.status === 'lost')
  const bars = $derived(expanded ? run.progress : run.progress.slice(0, 3))
  const chips = $derived(expanded ? orderedMetrics(run) : orderedMetrics(run).slice(0, 3))
  const hidden = $derived(Math.max(0, run.progress.length - 3) + Math.max(0, orderedMetrics(run).length - 3))
  const sys = $derived(systemStats(run))
  const when = $derived.by(() => {
    if (run.status === 'running') return duration(now - Date.parse(run.started_at))
    if (run.status === 'failed' && run.exit_code != null) return `exit ${run.exit_code} · ${ago(now - Date.parse(run.updated_at))}`
    if (run.status === 'lost') return `lost ${ago(now - Date.parse(run.updated_at))}`
    return ago(now - Date.parse(run.updated_at))
  })

  // Sparkline of the headline metric (first key of the first chart),
  // refreshed at most every 3s while the run updates.
  let spark = $state([])
  const sparkKey = $derived(run.charts?.find((c) => c.group !== 'system' && !c.x)?.keys[0])
  let lastFetch = 0
  let timer
  $effect(() => {
    run.updates
    const key = sparkKey
    if (!key || run.hub) return // a hub's series stay on the hub
    clearTimeout(timer)
    timer = setTimeout(async () => {
      lastFetch = Date.now()
      const set = await getJSON(`/api/v1/runs/${encodeURIComponent(run.id)}/series?key=${encodeURIComponent(key)}&points=60`).catch(() => null)
      if (set?.series?.[0]) spark = set.series[0].y
    }, Math.max(0, 3000 - (Date.now() - lastFetch)))
    return () => clearTimeout(timer)
  })

  function open(e) {
    e.stopPropagation()
    openPage(`/r/${encodeURIComponent(run.id)}`)
  }

  async function copyCommand(e) {
    e.stopPropagation()
    if (await copy(run.cmd)) {
      copied = true
      setTimeout(() => (copied = false), 1200)
    }
  }
  function dismiss(e) {
    e.stopPropagation()
    act(run.id, 'seen')
  }
</script>

<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
<div class="card {run.status}" class:attention onclick={() => (expanded = !expanded)}>
  <div class="head">
    <span class="dot" data-tip={statusTip(run)}></span>
    <span class="name" data-tip={`${run.job}, run #${run.seq}${run.hub ? ` on ${run.hub}` : ''}. Click to expand; ↗ opens the full page.`}>{run.job}<span class="seq">#{run.seq}</span>{#if run.hub}<span class="hubtag">hub</span>{/if}</span>
    <span class="when" data-tip={run.status === 'running' ? `Running for ${when}, since ${new Date(run.started_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}` : `Last update ${new Date(run.updated_at).toLocaleString()}`}>{run.stop_requested ? 'stopping…' : when}</span>
    {#if run.status === 'running' && canAct}<span class="hover-only"><StopButton {run} compact /></span>{/if}
    <button class="open" data-tip="Open the run’s page: charts, log and actions" aria-label="Open details" onclick={open}>
      <svg width="11" height="11" viewBox="0 0 11 11" aria-hidden="true"><path d="M3.5 1.5h6v6M9.5 1.5 1.5 9.5" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/></svg>
    </button>
  </div>

  {#if run.asks?.length}
    <Asks asks={run.asks} readOnly={!canAct} />
  {/if}

  {#if attention && run.status === 'lost'}
    <div class="msg">Stopped reporting. The terminal may have closed, or the machine went to sleep.</div>
  {:else if attention && run.message}
    <div class="msg mono">{run.message}</div>
  {/if}

  {#each bars as p, i (p.name)}
    <Bar {p} {now} {run} primary={i === 0} tone={run.status} />
  {/each}

  {#if run.fields?.length && !attention}
    <div class="fields">
      {#each (expanded ? run.fields : run.fields.slice(0, 2)) as f (f.name)}
        <div data-tip={`${label(f.name, run)}: ${f.value}`}><span>{label(f.name, run)}</span>{f.value}</div>
      {/each}
    </div>
  {/if}

  {#if spark.length > 1 && !expanded}
    <Sparkline tip={`${label(sparkKey, run)}, last ${spark.length} points`} ys={spark} color={attention ? 'var(--red)' : 'var(--blue)'} />
  {/if}

  {#if chips.length}
    <div class="chips">
      {#each chips as m (m.name)}
        <span class="chip" class:bad={typeof m.value === 'string'} data-tip={label(m.name, run) === m.name ? `${m.name} = ${num(m.value)}` : `${label(m.name, run)} = ${withUnit(m.value, m.name, run)}\nKey: ${m.name}`}><b>{label(m.name, run)}</b>{withUnit(m.value, m.name, run)}</span>
      {/each}
    </div>
  {/if}

  {#if sys.length && run.status === 'running'}
    <div class="sys">
      {#each sys as s (s.key)}<span data-tip={s.tip}>{s.label} <b>{s.text}</b></span>{/each}
    </div>
  {/if}

  {#if !expanded && hidden > 0}
    <div class="more" data-tip="Click the card to see everything">+{hidden} more</div>
  {/if}

  {#if expanded}
    <div class="details">
      {#if run.cmd}<div><span>command</span><code>{run.cmd}</code></div>{/if}
      {#if run.cwd}<div><span>folder</span><code>{run.cwd}</code></div>{/if}
      {#if run.host}<div><span>host</span><code>{run.host}</code></div>{/if}
      <div><span>source</span><code>{run.source}</code></div>
    </div>
  {/if}

  {#if attention || expanded}
    <div class="actions">
      {#if run.cmd}<button data-tip="Copy the command that started this run, to run it again" onclick={copyCommand}>{copied ? 'Copied' : 'Copy command'}</button>{/if}
      {#if attention && canAct}<button class="primary" data-tip="Mark as seen: clears the menu bar badge for this run" onclick={dismiss}>Dismiss</button>{/if}
    </div>
  {/if}
</div>

<style>
  .card {
    background: var(--card);
    border: 0.5px solid var(--border);
    border-radius: 10px;
    padding: 10px 12px 11px;
    margin-bottom: 8px;
    cursor: default;
  }
  .card.attention { box-shadow: inset 3px 0 0 var(--accent); }
  .card.failed { --accent: var(--red); }
  .card.lost { --accent: var(--orange); }
  .head { display: flex; align-items: center; gap: 8px; }
  .dot { width: 8px; height: 8px; border-radius: 50%; flex: none; background: var(--gray); }
  .running .dot { background: var(--blue); animation: pulse 1.6s ease-in-out infinite; }
  .done .dot { background: var(--green); }
  .failed .dot { background: var(--red); }
  .lost .dot { background: var(--orange); }
  @keyframes pulse { 50% { opacity: 0.35; } }
  .name { font-weight: 600; flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .seq { color: var(--muted); font-weight: 400; margin-left: 4px; }
  .when { color: var(--muted); font-size: 12px; font-variant-numeric: tabular-nums; white-space: nowrap; }
  .msg { margin-top: 6px; font-size: 12px; color: var(--fg-2); }
  .msg.mono {
    font: 11px/1.45 ui-monospace, SFMono-Regular, Menlo, monospace;
    white-space: pre-wrap; word-break: break-word;
    max-height: 5.8em; overflow: hidden;
    display: -webkit-box; -webkit-line-clamp: 4; line-clamp: 4; -webkit-box-orient: vertical;
  }
  .chips { display: flex; flex-wrap: wrap; gap: 5px; margin-top: 9px; }
  .chip { background: var(--track); border-radius: 5px; padding: 2px 7px; font-size: 12px; font-variant-numeric: tabular-nums; }
  .chip b { font-weight: 500; color: var(--muted); margin-right: 5px; }
  .chip.bad { background: color-mix(in srgb, var(--red) 18%, transparent); }
  .fields { margin-top: 7px; font-size: 11px; color: var(--fg-2); display: grid; gap: 1px; }
  .fields div { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .fields span { color: var(--muted); margin-right: 6px; }
  .sys { display: flex; flex-wrap: wrap; gap: 2px 12px; margin-top: 8px; font-size: 11px; color: var(--muted); font-variant-numeric: tabular-nums; }
  .sys b { font-weight: 500; color: var(--fg-2); }
  .more { margin-top: 6px; font-size: 11px; color: var(--muted); }
  .details { margin-top: 10px; display: grid; gap: 4px; font-size: 11px; }
  .details div { display: grid; grid-template-columns: 52px 1fr; gap: 8px; }
  .details span { color: var(--muted); }
  .details code { font: 11px ui-monospace, Menlo, monospace; word-break: break-all; -webkit-user-select: text; user-select: text; }
  .actions { display: flex; justify-content: flex-end; gap: 6px; margin-top: 10px; }
  button {
    font: inherit; font-size: 12px; padding: 3px 10px; border-radius: 6px;
    border: 0.5px solid var(--border); background: var(--button); color: var(--fg);
  }
  button:active { filter: brightness(0.92); }
  button.primary { background: var(--blue); border-color: transparent; color: white; }
  .open {
    border: none; background: none; padding: 2px 2px 2px 4px; color: var(--muted);
    display: flex; align-items: center; opacity: 0; transition: opacity 0.15s;
  }
  .card:hover .open { opacity: 1; }
  .hover-only { opacity: 0; transition: opacity 0.15s; display: flex; }
  .card:hover .hover-only, .hover-only:focus-within { opacity: 1; }
  .open:hover { color: var(--blue); }
  .hubtag {
    margin-left: 6px; padding: 0 5px; border-radius: 4px; font-size: 10px; font-weight: 600;
    text-transform: uppercase; letter-spacing: 0.04em; color: var(--muted); background: var(--track);
  }
</style>
