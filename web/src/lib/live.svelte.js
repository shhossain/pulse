// Live state from the app's SSE stream, shared by all components.
export const live = $state({
  runs: [],
  connected: false,
  ended: false, // the session is gone (app restarted): a reload can't sign in
  now: Date.now(),
})

// Where this page runs: the Mac's own app, or the read-only web UI through
// a hub's tunnel (signed in by Cloudflare Access). Until the server says,
// a page that isn't on this machine assumes it's a read-only viewer.
const onThisMachine = ['127.0.0.1', 'localhost', '[::1]'].includes(location.hostname) || '_wails' in window
export const session = $state({ remote: !onThisMachine, readOnly: !onThisMachine, email: '' })

let started = false
let lostTimer

export function connect() {
  if (started) return
  started = true
  getJSON('/api/v1/session')
    .then((s) => {
      session.remote = !!s.remote
      session.readOnly = !!s.read_only
      session.email = s.email ?? ''
    })
    .catch(() => {})
  const open = () => {
    const es = new EventSource('/api/v1/stream')
    es.addEventListener('runs', (e) => {
      live.runs = JSON.parse(e.data).runs ?? []
      live.connected = true
      live.ended = false
      clearTimeout(lostTimer)
      lostTimer = null
    })
    es.onerror = () => {
      live.connected = false
      // Through a tunnel, an expired sign-in looks like a network error and
      // the browser retries forever: after a while, offer a reload instead.
      if (session.remote && !lostTimer) {
        // Armed once per outage: the browser retries every few seconds and
        // each retry lands here again.
        lostTimer = setTimeout(() => {
          lostTimer = null
          if (!live.connected) live.ended = true
        }, 20000)
      }
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
  if (session.remote) {
    location.href = path // a browser: just navigate
    return
  }
  await fetch(`/api/v1/ui/open?path=${encodeURIComponent(path)}`, { method: 'POST' })
}

export async function getJSON(path) {
  const r = await fetch(path, { cache: 'no-store' })
  if (!r.ok) throw new Error(`${r.status} ${path}`)
  return r.json()
}

export async function act(runId, action) {
  if (session.readOnly) return
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
