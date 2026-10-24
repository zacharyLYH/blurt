// Blurt service worker: installability plus push delivery.
// Deliberately caches nothing. The shell is always network-first, /api and
// /health always bypass, and updates ship as soon as a new build replaces
// this file (nginx serves it no-cache). Never add runtime caching here
// without an explicit version bump and cleanup step.
self.addEventListener('install', () => {
  self.skipWaiting()
})

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim())
})

self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url)
  // Cross-origin, API, and health traffic: never touched, always network.
  if (url.origin !== self.location.origin) return
  if (url.pathname.startsWith('/api') || url.pathname.startsWith('/health')) return
  // Everything else falls through to the network too.
})

// Same-origin guard, mirrored by the click-target rule in the app.
function clickTarget(navigate) {
  try {
    const url = new URL(navigate, self.location.origin)
    return url.origin === self.location.origin ? url.href : null
  } catch {
    return null
  }
}

self.addEventListener('push', (event) => {
  let payload
  try {
    payload = event.data?.json()
  } catch {
    return
  }
  const notification = payload?.notification
  if (!notification || typeof notification.title !== 'string' || typeof notification.body !== 'string') {
    return
  }
  event.waitUntil(self.registration.showNotification(notification.title, {
    body: notification.body,
    tag: typeof notification.tag === 'string' ? notification.tag : undefined,
    icon: '/icons/icon-192.png',
    badge: '/icons/icon-192.png',
    data: {
      navigate: typeof notification.navigate === 'string'
        ? notification.navigate
        : self.location.origin + '/home',
      // The Trace row id of this firing attempt, for notification feedback.
      notificationTraceId: payload?.notificationTraceId ?? null,
    },
  }))
})

self.addEventListener('notificationclick', (event) => {
  event.notification.close()
  const target = clickTarget(event.notification.data?.navigate)
  if (!target) return
  event.waitUntil((async () => {
    const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true })
    for (const client of windows) {
      if (client.url.startsWith(self.location.origin) && 'focus' in client) {
        client.navigate(target)
        return client.focus()
      }
    }
    return self.clients.openWindow(target)
  })())
})

// Browser-initiated subscription rotation: re-subscribe with the server's
// current VAPID key and hand the new endpoint back. Best effort; if it
// fails the app shows setup again on the next load.
self.addEventListener('pushsubscriptionchange', (event) => {
  event.waitUntil((async () => {
    const res = await fetch('/api/push/key', { credentials: 'include' })
    if (!res.ok) return
    const { publicKey } = await res.json()

    const padded = (publicKey + '='.repeat((4 - (publicKey.length % 4)) % 4))
      .replace(/-/g, '+')
      .replace(/_/g, '/')
    const raw = atob(padded)
    const key = Uint8Array.from(raw, (char) => char.charCodeAt(0))

    const subscription = event.newSubscription ?? await self.registration.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: key,
    })
    await fetch('/api/push/subscribe', {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(subscription.toJSON()),
    })
  })())
})
