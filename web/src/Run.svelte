<script>
  import Bar from './components/Bar.svelte'
  import Chart from './components/Chart.svelte'
  import Scatter from './components/Scatter.svelte'
  import StopButton from './components/StopButton.svelte'
  import Alerts from './components/Alerts.svelte'
  import ActionBar from './components/ActionBar.svelte'
  import Asks from './components/Asks.svelte'
  import { untrack } from 'svelte'
  import { live, connect, getJSON, copy, act, session } from './lib/live.svelte.js'
  import { num, duration, ago, label, withUnit, statusTip } from './lib/format.js'

  const runId = decodeURIComponent(location.pathname.split('/').pop())
  const inApp = '_wails' in window // native window: leave room for the traffic lights

  let run = $state(null)
  let error = $state('')
  let history = $state([]) // other runs of this job
  let compareIds = $state([])
  let showCompare = $state(false)
  let smoothing = $state(0)
  let logScale = $state(false)
  let chartVersion = $state(0)
  let logLines = $state([])
  let follow = $state(true)
  let logEl = $state()
  let copied = $state(false)

  connect()

  async function loadRun() {
    try {
      run = await getJSON(`/api/v1/runs/${encodeURIComponent(runId)}`)
      document.title = `${run.job} #${run.seq} — Pulse`
    } catch {
      error = 'This run no longer exists.'
    }
  }
  async function loadHistory() {
    if (!run) return
    const res = await getJSON(`/api/v1/runs?job=${encodeURIComponent(run.job)}`).catch(() => ({ runs: [] }))
    history = res.runs.filter((r) => r.id !== runId)
  }
  async function loadLog() {
    const res = await getJSON(`/api/v1/runs/${encodeURIComponent(runId)}/log?tail=500`).catch(() => null)
    if (!res) return
    logLines = res.lines
    if (follow && logEl) requestAnimationFrame(() => (logEl.scrollTop = logEl.scrollHeight))
  }

  loadRun().then(() => {
    loadHistory()
    loadLog()
  })

  // Live: the SSE snapshot carries running runs. Refresh charts/log at most
  // every 2s while this run is updating.
  let lastRefresh = 0
  let pending = null
  $effect(() => {
    const fresh = live.runs.find((r) => r.id === runId)
    const prev = untrack(() => run)
    if (!fresh || !prev) return
    const changed = fresh.updates !== prev.updates || fresh.sys_updates !== prev.sys_updates || fresh.status !== prev.status || fresh.log_lines !== prev.log_lines
    run = fresh
    if (!changed) return
    const wait = Math.max(0, 2000 - (Date.now() - lastRefresh))
    clearTimeout(pending)
    pending = setTimeout(() => {
      lastRefresh = Date.now()
      chartVersion++
      loadLog()
    }, wait)
  })

  const compare = $derived(history.filter((r) => compareIds.includes(r.id)).map((r) => ({ id: r.id, seq: r.seq })))
  const elapsed = $derived(
    run ? (run.ended_at ? Date.parse(run.ended_at) : live.now) - Date.parse(run.started_at) : 0,
  )
  const statusText = $derived.by(() => {
    if (!run) return ''
    switch (run.status) {
      case 'running': return `Running · ${duration(elapsed)}`
      case 'done': return `Finished in ${duration(elapsed)} · ${ago(live.now - Date.parse(run.ended_at ?? run.updated_at))}`
      case 'failed': return `Failed${run.exit_code != null ? ` (exit ${run.exit_code})` : ''} after ${duration(elapsed)}`
      case 'lost': return `Lost contact ${ago(live.now - Date.parse(run.updated_at))}`
      case 'stopped': return `Stopped after ${duration(elapsed)}`
      default: return `Idle · updated ${ago(live.now - Date.parse(run.updated_at))}`
    }
  })
  const charted = $derived(new Set((run?.charts ?? []).flatMap((c) => c.keys)))

  function toggleCompare(id) {
    compareIds = compareIds.includes(id) ? compareIds.filter((x) => x !== id) : [...compareIds, id].slice(-3)
  }
  async function copyCommand() {
    if (await copy(run.cmd)) {
      copied = true
      setTimeout(() => (copied = false), 1200)
    }
  }
  function onLogScroll() {
    follow = logEl.scrollTop + logEl.clientHeight >= logEl.scrollHeight - 8
  }
</script>

<div class="page" class:inApp>
  {#if live.ended}
    <p class="empty">Pulse restarted. Reopen this page from the popover or with <code>pulse open {run?.job ?? ''}</code>.</p>
  {:else if error}
    <p class="empty">{error}</p>
  {:else if run}
    <header class="top">
      <div class="title">
        <span class="dot {run.status}" data-tip={statusTip(run)}></span>
        <h1>{run.job}<span class="seq">#{run.seq}</span></h1>
        <span class="status">{statusText}</span>
      </div>
      <div class="actions">
        {#if history.length}
          <div class="menu-wrap">
            <button data-tip="Overlay earlier runs of this job on the charts (dashed)" onclick={() => (showCompare = !showCompare)} class:active={compareIds.length}>
              Compare{compareIds.length ? ` (${compareIds.length})` : ''}
            </button>
            {#if showCompare}
              <div class="menu">
                <div class="menu-title">Overlay earlier runs</div>
                {#each history.slice(0, 12) as h (h.id)}
                  <label>
                    <input type="checkbox" checked={compareIds.includes(h.id)} onchange={() => toggleCompare(h.id)} />
                    <span class="dot small {h.status}"></span>
                    #{h.seq}
                    <span class="muted">{h.status} · {ago(live.now - Date.parse(h.updated_at))}</span>
                  </label>
                {/each}
              </div>
            {/if}
          </div>
        {/if}
        {#if run.cmd}<button data-tip="Copy the command that started this run, to run it again" onclick={copyCommand}>{copied ? 'Copied' : 'Copy command'}</button>{/if}
        {#if (run.status === 'running' || run.status === 'idle') && !session.readOnly}<StopButton {run} />{/if}
        {#if (run.status === 'failed' || run.status === 'lost') && !run.seen && !session.readOnly}
          <button class="primary" data-tip="Mark as seen: clears the menu bar badge for this run" onclick={() => act(run.id, 'seen')}>Dismiss</button>
        {/if}
      </div>
    </header>

    {#if run.cmd || run.cwd || run.host}
      <div class="meta">
        {#if run.cmd}<code data-tip="Command">{run.cmd}</code>{/if}
        {#if run.cwd}<span data-tip="Folder it ran in">{run.cwd}</span>{/if}
        {#if run.host}<span data-tip="Machine">{run.host}</span>{/if}
      </div>
    {/if}

    {#if run.fields?.length}
      <div class="fields">
        {#each run.fields as f (f.name)}<span data-tip={label(f.name, run) !== f.name ? `Key: ${f.name}` : null}><b>{label(f.name, run)}</b> {f.value}</span>{/each}
      </div>
    {/if}

    {#if run.asks?.length}<Asks asks={run.asks} readOnly={session.readOnly} />{/if}

    {#if !session.remote}<ActionBar {run} />{/if}

    {#if run.status === 'failed' && run.message}
      <pre class="error">{run.message}</pre>
    {:else if run.message}
      <p class="message">{run.message}</p>
    {/if}

    {#if run.progress.length}
      <section class="card progress">
        {#each run.progress as p, i (p.name)}
          <Bar {p} {run} now={live.now} primary={i === 0} tone={run.status} />
        {/each}
      </section>
    {/if}

    {#if run.charts?.some((c) => c.group !== 'system')}
      <div class="toolbar">
        <h2>Charts</h2>
        <label class="ctl" data-tip="Averages out noise (exponential moving average). The raw line stays faint behind.">
          Smoothing
          <input type="range" min="0" max="0.99" step="0.01" bind:value={smoothing} />
          <span class="val">{smoothing.toFixed(2)}</span>
        </label>
        <label class="ctl" data-tip="Logarithmic y axis: shows small changes in values that span orders of magnitude"><input type="checkbox" bind:checked={logScale} /> Log scale</label>
        <span class="hint">Drag to zoom · double-click to reset</span>
      </div>
      <div class="grid">
        {#each run.charts.filter((c) => c.group !== 'system') as chart (chart.name)}
          {#if chart.x}
            <Scatter {run} runId={run.id} seq={run.seq} {chart} {compare} {logScale} version={chartVersion} />
          {:else}
            <Chart {run} runId={run.id} seq={run.seq} {chart} {compare} {smoothing} {logScale} version={chartVersion} />
          {/if}
        {/each}
      </div>
    {/if}

    {#if run.charts?.some((c) => c.group === 'system')}
      <div class="toolbar">
        <h2 data-tip="What the job uses of the machine, recorded by pulse run every few seconds. Turn off with --no-system.">System</h2>
      </div>
      <div class="grid system">
        {#each run.charts.filter((c) => c.group === 'system') as chart (chart.name)}
          <Chart {run} runId={run.id} seq={run.seq} {chart} {compare} {smoothing} {logScale} version={chartVersion} />
        {/each}
      </div>
    {/if}

    {#if run.metrics.length}
      <h2>Metrics</h2>
      <section class="card table">
        <table>
          <thead><tr><th>Name</th><th>Latest</th><th data-tip="How many values were recorded">Values</th><th></th></tr></thead>
          <tbody>
            {#each run.metrics as m (m.name)}
              <tr>
                <td data-tip={label(m.name, run) !== m.name ? `Key: ${m.name}` : null}>{label(m.name, run)}</td>
                <td class="num" class:bad={typeof m.value === 'string'}>{withUnit(m.value, m.name, run)}</td>
                <td class="num muted">{m.n}</td>
                <td class="muted">{charted.has(m.name) ? 'charted' : ''}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </section>
    {/if}

    {#if !session.remote}
      <h2>Alerts</h2>
      <Alerts job={run.job} />
    {/if}

    <div class="toolbar">
      <h2>Output</h2>
      {#if !follow}<button data-tip="Scroll to the newest output and keep following it" onclick={() => { follow = true; logEl.scrollTop = logEl.scrollHeight }}>Jump to end</button>{/if}
    </div>
    <section class="card log" bind:this={logEl} onscroll={onLogScroll}>
      {#if logLines.length}
        <pre>{logLines.join('\n')}</pre>
      {:else}
        <p class="muted">{run.source === 'run' ? 'No output yet.' : 'Output is captured for jobs started with pulse run.'}</p>
      {/if}
    </section>
  {:else}
    <p class="empty">Loading…</p>
  {/if}
</div>

<style>
  :global(body) { background: var(--page); }
  .page { max-width: 1200px; margin: 0 auto; padding: 18px 24px 40px; }
  .page.inApp { padding-top: 14px; }
  .top {
    display: flex; justify-content: space-between; align-items: center; gap: 16px;
    --wails-draggable: drag;
  }
  .inApp .top { padding-left: 64px; min-height: 30px; }
  .title { display: flex; align-items: baseline; gap: 10px; min-width: 0; }
  h1 { margin: 0; font-size: 20px; font-weight: 700; letter-spacing: -0.02em; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .seq { color: var(--muted); font-weight: 500; margin-left: 6px; }
  .status { color: var(--muted); font-size: 13px; white-space: nowrap; font-variant-numeric: tabular-nums; }
  .dot { width: 10px; height: 10px; border-radius: 50%; background: var(--gray); flex: none; align-self: center; }
  .dot.small { width: 7px; height: 7px; display: inline-block; }
  .dot.running { background: var(--blue); animation: pulse 1.6s ease-in-out infinite; }
  .dot.done { background: var(--green); }
  .dot.failed { background: var(--red); }
  .dot.lost { background: var(--orange); }
  @keyframes pulse { 50% { opacity: 0.35; } }
  .actions { display: flex; gap: 8px; --wails-draggable: no-drag; }
  .actions button.active { border-color: var(--blue); color: var(--blue); }
  .menu-wrap { position: relative; }
  .menu {
    position: absolute; right: 0; top: calc(100% + 6px); z-index: 10; min-width: 240px;
    background: var(--card-solid); border: 0.5px solid var(--border); border-radius: 10px;
    padding: 6px; box-shadow: 0 8px 30px rgba(0, 0, 0, 0.18);
  }
  .menu-title { font-size: 11px; color: var(--muted); padding: 4px 8px 6px; }
  .menu label { display: flex; align-items: center; gap: 8px; padding: 5px 8px; border-radius: 6px; font-size: 12px; }
  .menu label:hover { background: var(--track); }
  .meta {
    display: flex; flex-wrap: wrap; gap: 6px 16px; margin: 8px 0 0; font-size: 12px; color: var(--muted);
  }
  .inApp .meta { padding-left: 64px; }
  .meta code { font: 12px var(--mono); color: var(--fg-2); -webkit-user-select: text; user-select: text; }
  .fields { display: flex; flex-wrap: wrap; gap: 4px 18px; margin-top: 10px; font-size: 12px; color: var(--fg-2); }
  .inApp .fields { padding-left: 64px; }
  .fields b { font-weight: 500; color: var(--muted); margin-right: 4px; }
  .message { margin: 12px 0 0; font-size: 13px; color: var(--fg-2); }
  .error {
    margin: 16px 0 0; padding: 10px 12px; border-radius: 10px; white-space: pre-wrap;
    font: 12px/1.5 var(--mono); color: var(--fg);
    background: color-mix(in srgb, var(--red) 10%, transparent);
    border: 0.5px solid color-mix(in srgb, var(--red) 35%, transparent);
    -webkit-user-select: text; user-select: text;
  }
  h2 { font-size: 13px; font-weight: 600; margin: 0; }
  .card { background: var(--card-solid); border: 0.5px solid var(--border); border-radius: 12px; padding: 12px 14px; }
  .progress { margin-top: 18px; padding-top: 6px; }
  .toolbar { display: flex; align-items: center; gap: 18px; margin: 26px 0 10px; }
  .ctl { display: flex; align-items: center; gap: 6px; font-size: 12px; color: var(--fg-2); }
  .ctl input[type='range'] { width: 110px; }
  .ctl .val { width: 30px; color: var(--muted); font-variant-numeric: tabular-nums; }
  .hint { margin-left: auto; font-size: 11px; color: var(--muted); }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(420px, 1fr)); gap: 12px; }
  .grid.system { grid-template-columns: repeat(auto-fill, minmax(300px, 1fr)); }
  .page > h2 { margin: 26px 0 10px; }
  .table { padding: 4px 14px; }
  table { width: 100%; border-collapse: collapse; font-size: 12px; }
  th { text-align: left; font-weight: 500; color: var(--muted); padding: 6px 0; border-bottom: 0.5px solid var(--border); }
  td { padding: 5px 0; border-bottom: 0.5px solid var(--border); }
  tr:last-child td { border-bottom: none; }
  .num { font-variant-numeric: tabular-nums; }
  .bad { color: var(--red); }
  .muted { color: var(--muted); }
  .log { max-height: 340px; overflow: auto; padding: 10px 12px; }
  .log pre { margin: 0; font: 11.5px/1.5 var(--mono); white-space: pre-wrap; word-break: break-word; -webkit-user-select: text; user-select: text; }
  .empty { color: var(--muted); text-align: center; margin-top: 80px; }
  @media (max-width: 600px) {
    .page { padding: 14px 16px 32px; }
    .grid { grid-template-columns: 1fr; }
    .top { flex-wrap: wrap; }
    .hint { display: none; }
  }
</style>
