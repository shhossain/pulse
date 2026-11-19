<script>
  // Line · Dots · Both, as a small segmented control.
  let { value = 'line', onpick } = $props()
  const OPTIONS = [
    { v: 'line', tip: 'Line', d: 'M1 9 L5 5 L9 7 L13 2' },
    { v: 'points', tip: 'Dots (scatter)', dots: [[1, 9], [5, 5], [9, 7], [13, 2]] },
    { v: 'both', tip: 'Dots joined by a line', d: 'M1 9 L5 5 L9 7 L13 2', dots: [[1, 9], [5, 5], [9, 7], [13, 2]] },
  ]
</script>

<div class="seg" role="radiogroup" aria-label="Chart style">
  {#each OPTIONS as o (o.v)}
    <button role="radio" aria-checked={value === o.v} aria-label={o.tip} class:on={value === o.v}
      data-tip={o.tip} onclick={() => onpick?.(o.v)}>
      <svg width="14" height="11" viewBox="0 0 14 11" aria-hidden="true">
        {#if o.d}<path d={o.d} fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round" stroke-linecap="round" />{/if}
        {#each o.dots ?? [] as [x, y]}<circle cx={x} cy={y} r="1.5" fill="currentColor" />{/each}
      </svg>
    </button>
  {/each}
</div>

<style>
  .seg { display: inline-flex; background: var(--track); border-radius: 6px; padding: 1px; gap: 1px; }
  button { border: none; background: none; padding: 2px 5px; border-radius: 5px; color: var(--muted); display: flex; align-items: center; }
  button:hover { color: var(--fg); }
  button.on { background: var(--card-solid); color: var(--fg); box-shadow: 0 0.5px 1.5px rgba(0, 0, 0, 0.15); }
</style>
