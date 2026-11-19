<script>
  // Two-step stop: first click asks "Stop?", second click stops. Resets
  // after a few seconds so a stray click can't stop a long job.
  import { act } from '../lib/live.svelte.js'

  let { run, compact = false } = $props()
  let confirming = $state(false)
  let busy = $state(false)
  let timer

  async function click(e) {
    e.stopPropagation()
    if (!confirming) {
      confirming = true
      clearTimeout(timer)
      timer = setTimeout(() => (confirming = false), 3500)
      return
    }
    clearTimeout(timer)
    confirming = false
    busy = true
    await act(run.id, 'stop')
    busy = false
  }
</script>

{#if run.stop_requested}
  <span class="stopping" class:compact>Stopping…</span>
{:else}
  <button class="stop" class:confirming class:compact disabled={busy} onclick={click}
    data-tip={confirming ? 'Click again to stop' : run.heartbeat ? 'Stop this run: sends Ctrl-C to its process' : 'Mark this run as stopped (Pulse can’t signal a pushed job)'}>
    {#if compact && !confirming}
      <svg width="9" height="9" viewBox="0 0 9 9" aria-hidden="true"><rect width="9" height="9" rx="2" fill="currentColor" /></svg>
    {:else}
      {confirming ? 'Stop?' : 'Stop'}
    {/if}
  </button>
{/if}

<style>
  .stop {
    font: inherit; font-size: 12px; padding: 3px 10px; border-radius: 6px;
    border: 0.5px solid var(--border); background: var(--button); color: var(--fg);
  }
  .stop.compact { padding: 2px 4px; border: none; background: none; color: var(--muted); display: flex; align-items: center; }
  .stop.compact:hover { color: var(--red); }
  .stop.confirming { background: var(--red); border-color: transparent; color: white; padding: 2px 8px; border-radius: 6px; font-size: 11px; font-weight: 600; }
  .stopping { font-size: 12px; color: var(--muted); }
  .stopping.compact { font-size: 11px; }
</style>
