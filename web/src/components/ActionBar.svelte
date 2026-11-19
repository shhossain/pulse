<script>
  // Buttons for a job's actions (pulse.yaml or `pulse action`), with an
  // inline confirm when the action asks for one and an output panel.
  import { actionTip, humanize } from '../lib/format.js'

  let { run } = $props()
  let busy = $state('')
  let confirming = $state('')
  let result = $state(null) // {name, ok, text}

  const running = $derived(run.status === 'running' || run.status === 'idle')
  const visible = $derived(
    (run.actions ?? []).filter((a) => !a.when || (a.when === 'running') === running),
  )

  async function trigger(a) {
    if (a.confirm && confirming !== a.name) {
      confirming = a.name
      setTimeout(() => confirming === a.name && (confirming = ''), 3500)
      return
    }
    confirming = ''
    busy = a.name
    result = null
    try {
      const r = await fetch(`/api/v1/runs/${encodeURIComponent(run.id)}/actions/${encodeURIComponent(a.name)}`, { method: 'POST' })
      const body = await r.json()
      if (!r.ok) result = { name: a.name, ok: false, text: body.error }
      else if (body.shell) {
        const s = body.shell
        const ok = s.exit_code === 0 && !s.timed_out
        if (a.output || !ok) {
          result = { name: a.name, ok, text: s.output || (ok ? 'Done.' : `Exited with ${s.exit_code}`), exit: s.exit_code, timedOut: s.timed_out }
        } else {
          result = { name: a.name, ok: true, text: 'Done.' }
        }
      } else if (body.sent) result = { name: a.name, ok: true, text: `Sent "${a.detail}" to the job.` }
      else if (body.url) result = null
    } catch (e) {
      result = { name: a.name, ok: false, text: String(e) }
    }
    busy = ''
  }
</script>

{#if visible.length || run.link}
  <div class="bar">
    {#if run.link}
      <a class="btn" data-tip={`Opens ${run.link}`} href={run.link} target="_blank" rel="noreferrer">Open link ↗</a>
    {/if}
    {#each visible as a (a.name)}
      <button class:confirming={confirming === a.name} disabled={busy !== '' || !!a.blocked}
        data-tip={confirming === a.name ? 'Click again to run' : actionTip(a)} onclick={() => trigger(a)}>
        {busy === a.name ? `${humanize(a.name)}…` : confirming === a.name ? `${humanize(a.name)}?` : humanize(a.name)}
      </button>
    {/each}
  </div>
  {#if result}
    <div class="result" class:bad={!result.ok}>
      <div class="head">
        <b>{humanize(result.name)}</b>
        {#if result.timedOut}<span>timed out</span>{:else if result.exit != null}<span>exit {result.exit}</span>{/if}
        <button class="x" aria-label="Close" data-tip="Close" onclick={() => (result = null)}>×</button>
      </div>
      <pre>{result.text}</pre>
    </div>
  {/if}
{/if}

<style>
  .bar { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 14px; }
  .btn, button {
    font: inherit; font-size: 12px; padding: 3px 10px; border-radius: 6px; text-decoration: none;
    border: 0.5px solid var(--border); background: var(--button); color: var(--fg);
  }
  button.confirming { background: var(--blue); color: white; border-color: transparent; }
  button:disabled { opacity: 0.6; }
  .result {
    margin-top: 10px; border-radius: 10px; padding: 8px 12px;
    background: var(--card-solid); border: 0.5px solid var(--border);
  }
  .result.bad { border-color: color-mix(in srgb, var(--red) 45%, transparent); }
  .head { display: flex; align-items: center; gap: 8px; font-size: 12px; }
  .head span { color: var(--muted); }
  .x { margin-left: auto; border: none; background: none; padding: 0 4px; font-size: 15px; color: var(--muted); }
  pre { margin: 6px 0 0; font: 11.5px/1.5 var(--mono); white-space: pre-wrap; max-height: 260px; overflow: auto; -webkit-user-select: text; user-select: text; }
</style>
