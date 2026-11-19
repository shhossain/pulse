<script>
  // Questions a job is waiting on (pulse.ask / pulse ask): answer in place.
  import { answerTip } from '../lib/format.js'

  let { asks = [], readOnly = false } = $props()
  let text = $state({})
  let error = $state('')
  let busy = $state('')

  async function answer(a, value) {
    if (!String(value ?? '').trim()) return
    busy = a.id
    const r = await fetch(`/api/v1/asks/${encodeURIComponent(a.id)}/answer`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ answer: value, by: 'ui' }),
    })
    busy = ''
    error = r.ok ? '' : ((await r.json().catch(() => ({}))).error ?? `Error ${r.status}`)
  }
</script>

{#each asks as a (a.id)}
  <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
  <div class="ask" onclick={(e) => e.stopPropagation()}>
    <div class="q">{a.question}</div>
    {#if a.context}<div class="ctx">{a.context}</div>{/if}
    {#if readOnly}
      <div class="ctx">Answer it where you can act on this run (↗){a.options?.length ? `: ${a.options.join(' / ')}` : ''}.</div>
    {:else if a.options?.length}
      <div class="opts">
        {#each a.options as o, i (o)}
          <button class:primary={i === 0} disabled={busy === a.id} data-tip={answerTip(a, o)} onclick={() => answer(a, o)}>{o}</button>
        {/each}
      </div>
    {:else}
      <form class="opts" onsubmit={(e) => { e.preventDefault(); answer(a, text[a.id]) }}>
        <input bind:value={text[a.id]} placeholder="Your answer" />
        <button class="primary" type="submit" disabled={busy === a.id}>Answer</button>
      </form>
    {/if}
    {#if a.deadline}<div class="due" data-tip="If nobody answers by then, this answer is used">Default “{a.default || 'timeout'}” at {new Date(a.deadline).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}</div>{/if}
  </div>
{/each}
{#if error}<p class="err">{error}</p>{/if}

<style>
  .ask { margin-top: 8px; padding: 9px 10px; border-radius: 8px; background: color-mix(in srgb, var(--blue) 9%, transparent); }
  .q { font-size: 13px; font-weight: 500; }
  .opts { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 8px; }
  button {
    font: inherit; font-size: 12px; padding: 3px 12px; border-radius: 6px;
    border: 0.5px solid var(--border); background: var(--button); color: var(--fg);
  }
  button.primary { background: var(--blue); border-color: transparent; color: white; }
  input {
    flex: 1; font: inherit; font-size: 12px; padding: 4px 8px; border-radius: 6px;
    border: 0.5px solid var(--border); background: var(--card-solid); color: var(--fg);
    -webkit-user-select: text; user-select: text;
  }
  .ctx { margin-top: 3px; font-size: 11.5px; color: var(--fg-2); }
  .due { margin-top: 6px; font-size: 11px; color: var(--muted); }
  .err { color: var(--red); font-size: 12px; margin: 6px 0 0; }
</style>
