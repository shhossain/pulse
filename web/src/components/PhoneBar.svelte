<script>
  // Web UI only (a hub's tunnel): install as an app, and turn this device's
  // notifications on or off. Notifications come from the hub through the
  // browser's push service, end-to-end encrypted.
  import { getJSON } from '../lib/live.svelte.js'

  const supported = 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window
  let state = $state(supported ? 'loading' : 'unsupported') // unsupported | loading | off | on | blocked
  let busy = $state(false)
  let note = $state('')
  let installEvent = $state(null)

  window.addEventListener('beforeinstallprompt', (e) => {
    e.preventDefault()
    installEvent = e
  })
  window.addEventListener('appinstalled', () => (installEvent = null))

  async function registration() {
    return navigator.serviceWorker.register('/sw.js')
  }

  async function refresh() {
    if (!supported) return
    if (Notification.permission === 'denied') return (state = 'blocked')
    const reg = await registration()
    const sub = await reg.pushManager.getSubscription()
    if (!sub) return (state = 'off')
    const s = await getJSON(`/api/v1/web/push?endpoint=${encodeURIComponent(sub.endpoint)}`).catch(() => null)
    state = s?.subscribed ? 'on' : 'off'
  }
  refresh().catch(() => (state = 'off'))

  function key(b64) {
    const s = atob(b64.replace(/-/g, '+').replace(/_/g, '/') + '==='.slice((b64.length + 3) % 4))
    return Uint8Array.from(s, (c) => c.charCodeAt(0))
  }

  async function post(path, body) {
    const r = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body ?? {}),
    })
    if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error ?? `Error ${r.status}`)
    return r.json()
  }

  async function turnOn() {
    busy = true
    note = ''
    try {
      if ((await Notification.requestPermission()) !== 'granted') {
        state = Notification.permission === 'denied' ? 'blocked' : 'off'
        return
      }
      const { public_key } = await getJSON('/api/v1/web/push')
      const reg = await registration()
      let sub = await reg.pushManager.getSubscription()
      if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key(public_key) })
      await post('/api/v1/web/push', sub.toJSON())
      state = 'on'
      await post('/api/v1/web/push/test').catch(() => {})
    } catch (e) {
      note = e.message
    } finally {
      busy = false
    }
  }

  async function turnOff() {
    busy = true
    note = ''
    try {
      const reg = await registration()
      const sub = await reg.pushManager.getSubscription()
      if (sub) {
        await post('/api/v1/web/push/unsubscribe', { endpoint: sub.endpoint }).catch(() => {})
        await sub.unsubscribe()
      }
      state = 'off'
    } catch (e) {
      note = e.message
    } finally {
      busy = false
    }
  }

  async function install() {
    const e = installEvent
    installEvent = null
    await e?.prompt()
  }
</script>

<div class="bar">
  {#if installEvent}<button onclick={install}>Install app</button>{/if}
  {#if state === 'off'}
    <button class="primary" disabled={busy} onclick={turnOn}>Turn on notifications</button>
  {:else if state === 'on'}
    <span class="on">Notifications on</span>
    <button disabled={busy} onclick={turnOff}>Turn off</button>
  {:else if state === 'blocked'}
    <span class="muted">Notifications are blocked for this site in the browser's settings.</span>
  {:else if state === 'unsupported'}
    <span class="muted">This browser can't receive notifications{installEvent ? '' : '; install the app first'}.</span>
  {/if}
  {#if note}<span class="err">{note}</span>{/if}
</div>

<style>
  .bar { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; padding: 0 16px 8px; font-size: 12px; }
  .on { color: var(--green, #3a3); }
  .muted { color: var(--muted); }
  .err { color: var(--red, #c33); }
</style>
