// Fake browser push transport. In production the Web Push service (FCM, Apple,
// Mozilla) hands an encrypted payload to the browser's service worker. The Go
// sender is exercised for real in push.spec.ts against a relay on the host;
// this helper covers the other half, the worker's own push handler, by
// delivering the same JSON shape straight into the registered worker.
import type { BrowserContext, Worker } from '@playwright/test'

export interface PushPayload {
  notification: { title: string; body: string; tag?: string; navigate?: string }
  notificationTraceId?: number
}

export interface SeenNotification {
  title: string
  body: string
  tag: string
  navigate: string | null
}

async function worker(context: BrowserContext): Promise<Worker> {
  const existing = context.serviceWorkers()[0]
  if (existing) return existing
  return context.waitForEvent('serviceworker')
}

// The wait happens inside the worker: showNotification settles on the
// worker's own event loop, and reading from a separate evaluate races it.
export async function deliverPush(context: BrowserContext, payload: PushPayload): Promise<SeenNotification[]> {
  const scope = await worker(context)
  return scope.evaluate(async (data) => {
    const self = globalThis as unknown as {
      registration: ServiceWorkerRegistration
      dispatchEvent(event: Event): boolean
    }
    self.dispatchEvent(new PushEvent('push', { data }))

    const deadline = Date.now() + 3_000
    let shown: Notification[] = []
    while (Date.now() < deadline) {
      shown = await self.registration.getNotifications()
      if (shown.length > 0) break
      await new Promise((resolve) => setTimeout(resolve, 50))
    }
    return shown.map((notification) => ({
      title: notification.title,
      body: notification.body,
      tag: notification.tag,
      navigate: (notification.data as { navigate?: string } | null)?.navigate ?? null,
    }))
  }, JSON.stringify(payload))
}
