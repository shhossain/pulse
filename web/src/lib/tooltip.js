// Hover explanations for anything with a data-tip attribute. A small custom
// layer instead of title="": the popover is a background window, where the
// system tooltips mostly don't appear. Positioned to stay on screen.
let el, timer, current

function place(target) {
  const r = target.getBoundingClientRect()
  el.style.left = '0px'
  el.style.top = '0px'
  el.classList.add('show')
  const t = el.getBoundingClientRect()
  const pad = 6
  let left = r.left + r.width / 2 - t.width / 2
  left = Math.max(pad, Math.min(left, window.innerWidth - t.width - pad))
  let top = r.bottom + 6
  if (top + t.height > window.innerHeight - pad) top = r.top - t.height - 6 // flip above
  el.style.left = `${left}px`
  el.style.top = `${Math.max(pad, top)}px`
}

function hide() {
  clearTimeout(timer)
  current = null
  el?.classList.remove('show')
}

export function installTooltips() {
  if (el) return
  el = document.createElement('div')
  el.className = 'pulse-tip'
  el.setAttribute('role', 'tooltip')
  document.body.appendChild(el)
  document.addEventListener('mouseover', (e) => {
    const target = e.target.closest?.('[data-tip]')
    if (target === current) return
    hide()
    if (!target || !target.dataset.tip) return
    current = target
    timer = setTimeout(() => {
      if (current !== target || !document.body.contains(target)) return
      el.textContent = target.dataset.tip
      place(target)
    }, 350)
  })
  document.addEventListener('mousedown', hide, true)
  document.addEventListener('scroll', hide, true)
  window.addEventListener('blur', hide)
}
