<script>
  import { getJSON } from '../lib/live.svelte.js'
  import { duration } from '../lib/format.js'

  let { job } = $props()
  let view = $state({ rules: [] })
  let expr = $state('')
  let then = $state('') // '' notify · ask · stop
  let error = $state('')

  const q = $derived(`?job=${encodeURIComponent(job)}`)
  const load = async () => (view = await getJSON(`/api/v1/alerts${q}`).catch(() => view))
  load()

  async function send(method, path, body) {
    const r = await fetch(path, {
      method,
      headers: body ? { 'Content-Type': 'application/json' } : {},
      body: body ? JSON.stringify(body) : undefined,
    })
    if (!r.ok) {
      error = (await r.json().catch(() => ({}))).error ?? `Error ${r.status}`
      return false
    }
    error = ''
    await load()
    return true
  }
  async function add(e) {
    e.preventDefault()
    if (!expr.trim()) return
    if (await send('POST', `/api/v1/alerts${q}`, { expr, action: then === 'stop' ? 'stop' : '', ask: then === 'ask' ? 'Stop the run?' : '' })) {
      expr = ''
      then = ''
    }
  }
  const toggle = (r) => send('PATCH', `/api/v1/alerts/${r.id}${q}`, { enabled: !r.enabled })
  const remove = (r) => send('DELETE', `/api/v1/alerts/${r.id}${q}`)
  const mute = (d) => fetch(`/api/v1/mute${q}&for=${d}`, { method: 'POST' }).then(load)
  const doText = (d) => !d ? 'nothing else' : d === 'stop' ? 'stops the run' : d.startsWith('send:') ? `sends “${d.slice(5)}”` : d.startsWith('action:') ? `runs “${d.slice(7)}”` : d
  function ruleTip(r) {
    const cool = r.once ? 'Fires once per run.' : r.cooldown_s ? `Then waits ${duration(r.cooldown_s * 1000)} before firing again.` : ''
    return [`Notifies (${r.level}) when: ${r.expr}`, cool, r.enabled ? '' : 'Paused.'].filter(Boolean).join('\n')
  }
  const mutedUntil = $derived(view.muted_until ? new Date(view.muted_until) : null)
</script>

<section class="card">
  <div class="always">Finished, failed and lost runs always notify, unless the job is muted.</div>
  <ul>
    {#each view.rules as r (r.id)}
      <li class:off={!r.enabled}>
        <label class="switch" data-tip={r.enabled ? 'On: click to pause this rule' : 'Off: click to turn this rule back on'}>
          <input type="checkbox" checked={r.enabled} onchange={() => toggle(r)} />
          <span></span>
        </label>
        <span class="expr" data-tip={ruleTip(r)}>{r.expr}</span>
        {#if r.ask}<span class="tag ask" data-tip={`Asks “${r.ask}”\n${(r.answers ?? []).map((a) => `${a.label}: ${doText(a.do)}`).join('\n')}`}>asks you</span>{/if}
        {#if r.action === 'stop'}<span class="tag" data-tip="Stops the run when it fires">then stop</span>{/if}
        {#if r.builtin}<span class="tag muted" data-tip="Pulse adds this rule to every job">built in</span>{:else if r.from_file}<span class="tag muted" data-tip="Defined in the job’s pulse.yaml; edit it there">pulse.yaml</span>{:else}
          <button class="x" data-tip="Remove this rule" aria-label="Remove" onclick={() => remove(r)}>×</button>
        {/if}
      </li>
    {/each}
  </ul>
  <form onsubmit={add}>
    <input class="field" bind:value={expr} placeholder="Add a rule: val_loss new min · loss > 5 · progress >= 90% · eta < 5m" />
    <select class="opt" bind:value={then} data-tip="What happens when the rule fires">
      <option value="">Notify me</option>
      <option value="ask">Ask me: stop?</option>
      <option value="stop">Stop the run</option>
    </select>
    <button type="submit" disabled={!expr.trim()}>Add</button>
  </form>
  {#if error}<p class="error">{error}</p>{/if}
  <div class="mute">
    {#if mutedUntil && mutedUntil > new Date()}
      Notifications muted until {mutedUntil.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}
      <button data-tip="Turn this job’s notifications back on" onclick={() => mute('off')}>Unmute</button>
    {:else}
      <button data-tip="Silence this job’s notifications for an hour. Questions still appear here." onclick={() => mute('1h')}>Mute for 1 hour</button>
    {/if}
  </div>
</section>

<style>
  .card { background: var(--card-solid); border: 0.5px solid var(--border); border-radius: 12px; padding: 10px 14px 12px; }
  .always { font-size: 12px; color: var(--muted); margin-bottom: 4px; }
  ul { list-style: none; margin: 0; padding: 0; }
  li { display: flex; align-items: center; gap: 10px; padding: 6px 0; border-bottom: 0.5px solid var(--border); font-size: 13px; }
  li.off .expr { color: var(--muted); }
  .expr { flex: 1; font-family: var(--mono); font-size: 12px; }
  .tag { font-size: 11px; padding: 1px 6px; border-radius: 4px; background: color-mix(in srgb, var(--red) 15%, transparent); }
  .tag.ask { background: color-mix(in srgb, var(--blue) 15%, transparent); }
  .tag.muted { background: var(--track); color: var(--muted); }
  .x { border: none; background: none; color: var(--muted); font-size: 16px; padding: 0 4px; }
  .x:hover { color: var(--red); }
  /* macOS-style switch */
  .switch { position: relative; width: 26px; height: 15px; flex: none; }
  .switch input { opacity: 0; width: 0; height: 0; position: absolute; }
  .switch span { position: absolute; inset: 0; border-radius: 8px; background: var(--track); transition: background 0.15s; }
  .switch span::after {
    content: ''; position: absolute; width: 13px; height: 13px; left: 1px; top: 1px; border-radius: 50%;
    background: white; box-shadow: 0 1px 2px rgba(0, 0, 0, 0.3); transition: transform 0.15s;
  }
  .switch input:checked + span { background: var(--green); }
  .switch input:checked + span::after { transform: translateX(11px); }
  form { display: flex; gap: 8px; align-items: center; margin-top: 10px; }
  .field {
    flex: 1; font: 12px var(--mono); padding: 5px 8px; border-radius: 6px;
    border: 0.5px solid var(--border); background: var(--page); color: var(--fg);
    -webkit-user-select: text; user-select: text;
  }
  .field:focus { outline: 2px solid color-mix(in srgb, var(--blue) 50%, transparent); outline-offset: -1px; }
  .opt { font: inherit; font-size: 12px; color: var(--fg); padding: 3px 4px; border-radius: 6px; border: 0.5px solid var(--border); background: var(--button); }
  .error { color: var(--red); font-size: 12px; margin: 6px 0 0; }
  .mute { margin-top: 10px; font-size: 12px; color: var(--muted); display: flex; align-items: center; gap: 8px; }
</style>
