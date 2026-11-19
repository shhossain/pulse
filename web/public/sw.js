// Pulse's service worker: shows the hub's push notifications and opens the
// run they're about. It caches nothing (every page comes fresh from the hub).
self.addEventListener('install', () => self.skipWaiting())
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()))

self.addEventListener('push', (e) => {
  let d = {}
  try {
    d = e.data ? e.data.json() : {}
  } catch {
    d = { title: 'Pulse', body: e.data ? e.data.text() : '' }
  }
  // A question an operator can answer right here (one or two options).
  const ask = d.ask && typeof d.ask.id === 'string' && Array.isArray(d.ask.options) ? d.ask : null
  const options = ask ? ask.options.slice(0, 2).map(String) : []
  e.waitUntil(
    self.registration.showNotification(String(d.title || 'Pulse'), {
      body: String(d.body || ''),
      tag: ask ? `ask-${ask.id}` : d.tag ? String(d.tag) : undefined,
      renotify: !!(ask || d.tag),
      icon: '/pwa/icon-192.png',
      requireInteraction: d.level === 'error' || !!ask,
      actions: options.map((o, i) => ({ action: `answer:${i}`, title: o })),
      data: { url: typeof d.url === 'string' ? d.url : '/popover', ask: ask ? { id: ask.id, options } : null },
    }),
  )
})

// Open (or focus) url. If the browser won't open a window now (the tap's
// moment has passed), leave a notification that opens it when tapped.
async function show(url, title, body) {
  const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true })
  for (const w of wins) {
    if (w.url === url.href && 'focus' in w) return w.focus()
  }
  try {
    if (await self.clients.openWindow(url.href)) return
  } catch {
    // not allowed any more: fall through
  }
  await self.registration.showNotification(title, { body, icon: '/pwa/icon-192.png', data: { url: url.pathname } })
}

// Answer from a notification button. If it can't (signed out, offline,
// slow), open the question so it can be answered there.
async function answer(data, choice, url) {
  const done = (title, body) =>
    self.registration.showNotification(title, {
      body,
      tag: `ask-${data.ask.id}`,
      icon: '/pwa/icon-192.png',
      silent: true,
      data: { url: url.pathname },
    })
  try {
    const r = await fetch(`/api/v1/asks/${encodeURIComponent(data.ask.id)}/answer`, {
      method: 'POST',
      credentials: 'same-origin',
      redirect: 'error', // a sign-in page in the way is a failure, not an answer
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ answer: choice }),
      signal: AbortSignal.timeout(8000),
    })
    if (r.ok) return done('Answered', `“${choice}” sent.`)
    if (r.status === 409) return done('Already answered', 'Someone answered it first, or it timed out.')
  } catch {
    // fall through
  }
  await show(url, "Couldn't answer from here", 'Tap to open the question in Pulse.')
}

self.addEventListener('notificationclick', (e) => {
  e.notification.close()
  const data = e.notification.data || {}
  const url = new URL(data.url || '/popover', self.location.origin)
  if (url.origin !== self.location.origin) return // only ever open this site
  const m = /^answer:(\d)$/.exec(e.action || '')
  if (m && data.ask && data.ask.options[Number(m[1])] !== undefined) {
    e.waitUntil(answer(data, data.ask.options[Number(m[1])], url))
    return
  }
  e.waitUntil(show(url, 'Pulse', 'Tap to open.'))
})
