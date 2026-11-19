// Live state from the app's SSE stream, shared by all components.
export const live = $state({
  runs: [],
  connected: false,
  ended: false, // the session is gone (app restarted): a reload can't sign in
  now: Date.now(),
})

let started = false

export function connect() {
  if (started) return
  started = true
  const open = () => {
    const es = new EventSource('/api/v1/stream')
    es.addEventListener('runs', (e) => {
      live.runs = JSON.parse(e.data).runs ?? []
      live.connected = true
    })
    es.onerror = () => {
      live.connected = false
      // EventSource retries by itself while the app is briefly away. CLOSED
      // means the session was refused (the app restarted with a new one):
      // reloading can't sign in, so say how to reopen instead.
      if (es.readyState === EventSource.CLOSED) live.ended = true
    }
  }
  open()
  setInterval(() => (live.now = Date.now()), 1000)
}

export async function openPage(path) {
  await fetch(`/api/v1/ui/open?path=${encodeURIComponent(path)}`, { method: 'POST' })
}

export async function getJSON(path) {
  const r = await fetch(path, { cache: 'no-store' })
  if (!r.ok) throw new Error(`${r.status} ${path}`)
  return r.json()
}

export async function act(runId, action) {
  await fetch(`/api/v1/runs/${encodeURIComponent(runId)}/${action}`, { method: 'POST' })
}

export async function copy(text) {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    const ta = document.createElement('textarea')
    ta.value = text
    document.body.appendChild(ta)
    ta.select()
    const ok = document.execCommand('copy')
    ta.remove()
    return ok
  }
}
