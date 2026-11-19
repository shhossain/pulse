<script>
  import RunCard from './components/RunCard.svelte'
  import FinishedRow from './components/FinishedRow.svelte'
  import PhoneBar from './components/PhoneBar.svelte'
  import { live, connect, session } from './lib/live.svelte.js'

  connect()

  const DAY = 86400e3
  const needsYou = $derived(
    live.runs.filter((r) => r.asks?.length || ((r.status === 'failed' || r.status === 'lost') && !r.seen)),
  )
  const running = $derived(live.runs.filter((r) => r.status === 'running' && !needsYou.includes(r)))
  const finished = $derived(
    live.runs.filter(
      (r) => r.status !== 'running' && !needsYou.includes(r) && live.now - Date.parse(r.updated_at) < DAY,
    ),
  )
  const empty = $derived(!needsYou.length && !running.length && !finished.length)
</script>

<main>
  <header>
    <span class="title">Pulse{#if session.readOnly}<span class="ro" title={session.email}> · view only</span>{/if}</span>
    <span class="status">
      {#if live.ended}
        Session ended
      {:else if !live.connected}
        Reconnecting…
      {:else if running.length}
        {running.length} running
      {:else}
        All quiet
      {/if}
    </span>
  </header>
  {#if session.remote}<PhoneBar />{/if}

  <div class="scroll">
    {#if live.ended && session.remote}
      <div class="empty"><p>Disconnected.</p><p class="hint">Your sign-in may have expired.</p><button onclick={() => location.reload()}>Reload</button></div>
    {:else if live.ended}
      <div class="empty"><p>Pulse restarted.</p><p class="hint">Close this and open Pulse again from the menubar.</p></div>
    {/if}
    {#if needsYou.length}
      <h2>Needs you</h2>
      {#each needsYou as run (run.id)}<RunCard {run} now={live.now} />{/each}
    {/if}

    {#if running.length}
      <h2>Running</h2>
      {#each running as run (run.id)}<RunCard {run} now={live.now} />{/each}
    {/if}

    {#if finished.length}
      <h2>Finished</h2>
      <div class="list">
        {#each finished as run (run.id)}<FinishedRow {run} now={live.now} />{/each}
      </div>
    {/if}

    {#if empty && live.connected}
      <div class="empty">
        <div class="ring"></div>
        <p>Nothing running.</p>
        <p class="hint">Start a job from any terminal:</p>
        <code>pulse run -- python train.py</code>
      </div>
    {/if}
  </div>
</main>

<style>
  :global(html), :global(body), :global(#app) { height: 100%; overflow: hidden; }
  main { height: 100%; display: flex; flex-direction: column; }
  header {
    display: flex; justify-content: space-between; align-items: baseline;
    padding: 12px 16px 8px;
  }
  .title { font-weight: 700; font-size: 14px; letter-spacing: -0.01em; }
  .ro { font-weight: 400; color: var(--muted); }
  .status { font-size: 12px; color: var(--muted); }
  .scroll { flex: 1; overflow-y: auto; padding: 0 12px 12px; }
  h2 {
    font-size: 11px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.06em;
    color: var(--muted); margin: 10px 4px 6px;
  }
  .list { margin: 0 -4px; }
  .empty { text-align: center; color: var(--muted); padding: 56px 16px 24px; }
  .empty p { margin: 4px 0; }
  .empty .hint { font-size: 12px; margin-top: 10px; }
  .empty code {
    display: inline-block; margin-top: 6px; padding: 4px 9px; border-radius: 6px;
    font: 12px ui-monospace, Menlo, monospace; background: var(--track); color: var(--fg);
    -webkit-user-select: text; user-select: text;
  }
  .ring {
    width: 34px; height: 34px; margin: 0 auto 12px; border-radius: 50%;
    border: 4px solid var(--track);
  }
</style>
