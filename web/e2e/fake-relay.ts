// Fake browser push transport. In production the Web Push service (FCM, Apple,
// Mozilla) hands an encrypted payload to the browser's service worker. The Go
// sender is exercised for real in push.spec.ts against a relay on the host;
// this helper covers the other half, the worker's own push handler, by
// delivering the same JSON shape straight into the registered worker.
import type { BrowserContext, Worker } from '@playwright/test'

export interface PushPayload {
  notification: { title: string; body: string; tag?: string; navigate?: string }
  reminderId?: number
}

export interface SeenNotification {
  title: string
  body: string
  tag: string
  navigate: string | null
  reminderId: number | null
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
    const read = async () => self.registration.getNotifications()
    // A synthetic event dispatched at the instant the worker activates can be
    // lost before its listeners are live, and a lost event is silent. Retry: the
    // payload's tag is per reminder, so a repeat replaces rather than stacks.
    let shown: Notification[] = []
    for (let attempt = 0; attempt < 3 && shown.length === 0; attempt++) {
      self.dispatchEvent(new PushEvent('push', { data }))
      const deadline = Date.now() + 1_000
      while (Date.now() < deadline) {
        shown = await read()
        if (shown.length > 0) break
        await new Promise((resolve) => setTimeout(resolve, 50))
      }
    }
    return shown.map((notification) => ({
      title: notification.title,
      body: notification.body,
      tag: notification.tag,
      navigate: (notification.data as { navigate?: string } | null)?.navigate ?? null,
      reminderId: (notification.data as { reminderId?: number } | null)?.reminderId ?? null,
    }))
  }, JSON.stringify(payload))
}

// clickNotification fires the worker's own notificationclick handler for the shown
// notification. The service worker then focuses an app window and navigates it to the
// payload's target, which is exactly what a user's tap does in production.
export async function clickNotification(context: BrowserContext, reminderId: number | null = null): Promise<string | null> {
  const scope = await worker(context)
  return scope.evaluate(async (id) => {
    const self = globalThis as unknown as {
      registration: ServiceWorkerRegistration
      dispatchEvent(event: Event): boolean
    }
    const shown = await self.registration.getNotifications()
    const target = shown.find(
      (notification) => id === null || (notification.data as { reminderId?: number } | null)?.reminderId === id,
    )
    if (!target) return null
    const NotificationClick = (globalThis as unknown as {
      NotificationEvent: new (type: string, init: { notification: Notification }) => Event
    }).NotificationEvent
    self.dispatchEvent(new NotificationClick('notificationclick', { notification: target }))
    return (target.data as { navigate?: string } | null)?.navigate ?? null
  }, reminderId)
}
