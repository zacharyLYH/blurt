import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { chromium, expect, test, type BrowserContext, type Page } from '@playwright/test'

import { BASE_URL, ciTimeout } from './env'
import { completeQuestionnaire, signIn, uniqueEmail } from './helpers'

// watchPushes installs a listener in the worker's own scope and returns a
// reader for it. A missing notification and a push that never arrived are
// different bugs, and only the worker knows which one happened. It never reads
// event.data, which would disturb the payload before the worker's own handler.
async function watchPushes(context: BrowserContext) {
  const worker = context.serviceWorkers()[0] ?? (await context.waitForEvent('serviceworker'))
  await worker.evaluate(() => {
    const seen: boolean[] = []
    ;(globalThis as unknown as { seenPushes: boolean[] }).seenPushes = seen
    ;(globalThis as unknown as EventTarget).addEventListener('push', (event) => {
      seen.push(Boolean((event as PushEvent).data))
    })
  })
  return () => worker.evaluate(() => (globalThis as unknown as { seenPushes: boolean[] }).seenPushes)
}

// shownNotifications reads what the worker is currently displaying: the only
// proof that the backend's message made it all the way into the client.
function shownNotifications(page: Page) {
  return page.evaluate(async () =>
    (await (await navigator.serviceWorker.ready).getNotifications()).map((notification) => ({
      title: notification.title,
      body: notification.body,
    })),
  )
}

// A real push service is shared infrastructure: it can rate limit or drop a
// delivery, so one retry is allowed. Without it a real end-to-end test is too
// noisy to keep around.
test.describe.configure({ retries: 1 })

// The whole flow, with nothing played by the test: blurt's backend sends, the
// browser's own push service (FCM, Apple, Mozilla) relays the encrypted
// message, and the service worker turns it into a notification. It is the only
// test where the client actually receives what the backend sent.
//
// Opt-in, because it needs outbound network and a real push service can rate
// limit a shared runner:
//
//   BLURT_E2E_REAL_PUSH=1 npm run test:e2e
//
// Two pieces are mandatory. A persistent context, because Playwright's default
// contexts are incognito and the Push API is unavailable there. A headed
// browser, because a headless one cannot even subscribe: measured here, it
// never reaches the push service and the app reports notifications pending.
// macOS has a display; a Linux host runs `make e2e-real`, which wraps this in
// xvfb-run.
test('the backend notification reaches the browser through the push service', async () => {
  test.skip(process.env.BLURT_E2E_REAL_PUSH !== '1', 'opt-in: needs a real push service')
  test.setTimeout(ciTimeout(240_000))

  const profile = await mkdtemp(join(tmpdir(), 'blurt-push-'))
  const context = await chromium.launchPersistentContext(profile, {
    channel: process.env.CI ? 'chromium' : 'chrome',
    headless: false,
    baseURL: BASE_URL,
  })
  try {
    // Grant first: the app asks for permission inside the login gesture, so a
    // granted permission makes that call a no-op instead of a prompt nobody
    // can answer. Then the app's own setupPush stores the subscription.
    await context.grantPermissions(['notifications'], { origin: BASE_URL })
    const page = await context.newPage()

    await signIn(page, { name: 'Push', email: uniqueEmail('realpush') })
    await completeQuestionnaire(page)
    // A subscription only exists once this browser talked to the push service.
    await expect(page.getByTestId('push-status')).toContainText('Notifications on', {
      timeout: ciTimeout(60_000),
    })
    // Watch the worker before anything is sent, so a push that arrives with an
    // unreadable payload is not mistaken for one that never arrived.
    const pushed = await watchPushes(context)

    // Sent from the page, so the session cookie and the CSRF Origin are the
    // app's own. The backend now encrypts for this browser and POSTs it.
    const sent = await page.evaluate(async () => {
      const res = await fetch('/api/push', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          title: 'Buy glue',
          body: 'On the way home. 6:00 PM SGT',
          tag: 'real-1',
          navigate: '/home',
        }),
      })
      return { status: res.status, body: (await res.json()) as Record<string, number> }
    })
    expect(sent.status).toBe(200)
    expect(sent.body).toEqual({ sent: 1, failed: 0, pruned: 0 })

    // The notification can only appear if the message travelled through the
    // push service and the worker decoded both fields.
    try {
      await expect
        .poll(() => shownNotifications(page), { timeout: ciTimeout(120_000) })
        .toContainEqual({ title: 'Buy glue', body: 'On the way home. 6:00 PM SGT' })
    } catch (error) {
      // A missing notification and a missing push are different bugs, so say
      // which one this was. Each entry is whether that push had a readable
      // payload.
      throw new Error(`${error}\nworker saw pushes: ${JSON.stringify(await pushed())}`, {
        cause: error,
      })
    }
  } finally {
    await context.close()
    await rm(profile, { recursive: true, force: true })
  }
})