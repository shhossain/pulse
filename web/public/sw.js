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
  e.waitUntil(
    self.registration.showNotification(String(d.title || 'Pulse'), {
      body: String(d.body || ''),
      tag: d.tag ? String(d.tag) : undefined,
      renotify: !!d.tag,
      icon: '/pwa/icon-192.png',
      requireInteraction: d.level === 'error',
      data: { url: typeof d.url === 'string' ? d.url : '/popover' },
    }),
  )
})

self.addEventListener('notificationclick', (e) => {
  e.notification.close()
  const url = new URL(e.notification.data?.url || '/popover', self.location.origin)
  if (url.origin !== self.location.origin) return // only ever open this site
  e.waitUntil(
    (async () => {
      const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true })
      for (const w of wins) {
        if (w.url === url.href && 'focus' in w) return w.focus()
      }
      return self.clients.openWindow(url.href)
    })(),
  )
})
