// Service worker + Web Push plumbing.
//
// The worker is registered as soon as the app boots, so the PWA is
// installable before login. On first login the app also asks for
// notification permission and registers a push subscription for this
// device; /home retries the server handoff on later loads.
import { api } from './api'

export type PushSetup = 'ready' | 'pending' | 'denied' | 'unsupported'

interface PushSubscriptionJSON {
  endpoint: string
  keys: { p256dh: string; auth: string }
}

const SW_URL = '/sw.js'

export async function ensureServiceWorker(): Promise<ServiceWorkerRegistration | null> {
  if (!('serviceWorker' in navigator)) return null
  try {
    const existing = await navigator.serviceWorker.getRegistration('/')
    if (existing) return existing
    return await navigator.serviceWorker.register(SW_URL, {
      scope: '/',
      updateViaCache: 'none',
    })
  } catch (err) {
    // Blocked or unsupported: the app still works, push just stays off.
    console.warn('Blurt: service worker registration failed', err)
    return null
  }
}

function keyToBase64(key?: ArrayBuffer | ArrayBufferView | null): string {
  if (!key) return ''
  const bytes = key instanceof ArrayBuffer
    ? new Uint8Array(key)
    : new Uint8Array(key.buffer, key.byteOffset, key.byteLength)
  let raw = ''
  for (const byte of bytes) raw += String.fromCharCode(byte)
  return btoa(raw).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

function decodeKey(base64: string): Uint8Array<ArrayBuffer> {
  const padded = (base64 + '='.repeat((4 - (base64.length % 4)) % 4))
    .replace(/-/g, '+')
    .replace(/_/g, '/')
  return Uint8Array.from(atob(padded), (char) => char.charCodeAt(0))
}

// setupPush is safe to call repeatedly. Pass prompt: true only from a user
// gesture (the login submit), because browsers and iOS reject a permission
// request that has no activation behind it.
export async function setupPush({ prompt }: { prompt: boolean }): Promise<PushSetup> {
  const worker = ensureServiceWorker()

  if (!('Notification' in window) || !('PushManager' in window)) {
    await worker
    return 'unsupported'
  }
  if (Notification.permission === 'denied') {
    await worker
    return 'denied'
  }
  if (Notification.permission === 'default') {
    if (!prompt) {
      await worker
      return 'pending'
    }
    // First await stays inside the gesture.
    const permission = await Notification.requestPermission()
    if (permission !== 'granted') {
      await worker
      return permission === 'denied' ? 'denied' : 'pending'
    }
  }

  const registration = await worker
  if (!registration) return 'pending'

  try {
    const { publicKey } = await api<{ publicKey: string }>('/api/push/key')
    let subscription = await registration.pushManager.getSubscription()
    if (subscription && keyToBase64(subscription.options.applicationServerKey) !== publicKey) {
      // Minted against a rotated key: drop it and subscribe again.
      await subscription.unsubscribe()
      subscription = null
    }
    if (!subscription) {
      subscription = await registration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: decodeKey(publicKey),
      })
    }
    await api('/api/push/subscribe', {
      method: 'POST',
      body: JSON.stringify(subscription.toJSON() as PushSubscriptionJSON),
    })
    return 'ready'
  } catch (err) {
    // The handoff can fail on a cold start or a dropped network while the
    // subscription itself is fine. The browser keeps it; the next /home load
    // retries, so the app stays usable with push merely pending.
    console.warn('Blurt: push subscription not stored', err)
    return 'pending'
  }
}
