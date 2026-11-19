<script>
  import { summary, ago, duration, statusTip } from '../lib/format.js'
  import { act, openPage, session } from '../lib/live.svelte.js'

  let { run, now } = $props()
  const took = $derived(run.ended_at ? duration(Date.parse(run.ended_at) - Date.parse(run.started_at)) : '')
</script>

<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
<div class="row {run.status}" data-tip={`${statusTip(run)}${run.cmd ? `\n${run.cmd}` : ''}\nClick to open its page.`} onclick={() => openPage(`/r/${encodeURIComponent(run.id)}`)}>
  <span class="dot"></span>
  <div class="text">
    <div class="line1">
      <span class="name">{run.job}<span class="seq">#{run.seq}</span></span>
      <span class="when">{ago(now - Date.parse(run.updated_at))}</span>
    </div>
    <div class="line2">
      {run.status === 'idle' ? 'idle' : run.status}{took ? ` in ${took}` : ''}{summary(run) ? ` · ${summary(run)}` : ''}
    </div>
  </div>
  {#if !session.readOnly && !run.hub}<button class="x" aria-label="Hide" data-tip="Hide from this list (the run stays in history)" onclick={(e) => { e.stopPropagation(); act(run.id, 'archive') }}>×</button>{/if}
</div>

<style>
  .row { display: flex; align-items: center; gap: 9px; padding: 6px 8px; border-radius: 8px; }
  .row:hover { background: var(--track); }
  .dot { width: 7px; height: 7px; border-radius: 50%; flex: none; background: var(--gray); }
  .done .dot { background: var(--green); }
  .failed .dot { background: var(--red); }
  .lost .dot { background: var(--orange); }
  .stopped .dot { background: var(--gray); }
  .text { flex: 1; min-width: 0; }
  .line1 { display: flex; justify-content: space-between; gap: 8px; }
  .name { font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .seq { color: var(--muted); font-weight: 400; margin-left: 4px; }
  .when { color: var(--muted); font-size: 11px; white-space: nowrap; }
  .line2 { color: var(--muted); font-size: 11px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-variant-numeric: tabular-nums; }
  .x {
    visibility: hidden; border: none; background: none; color: var(--muted);
    font-size: 15px; line-height: 1; padding: 0 2px; cursor: default;
  }
  .row:hover .x { visibility: visible; }
</style>
