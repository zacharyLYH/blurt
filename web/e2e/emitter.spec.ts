import { expect, test } from '@playwright/test'
import { createReminder, signInRequest, subscribeRequest, waitForReminder } from './api'
import { ciTimeout } from './env'
import { uniqueEmail } from './helpers'
import { decryptPush, deviceKeys } from './push-crypto'
import { withRelay } from './relay'

// The emitter, end to end and for real: a capture commits a Reminder through the agent, the
// server's own sweep finds it due, and an encrypted push lands on a relay the test runs on
// the host — where it is decrypted with the device's own key, so these are the words the
// user would read.
//
// The stack runs with BLURT_FAKE_CLOCK=90s (docker-compose.e2e.yml), so the emitter's clock
// is a minute and a half ahead: a reminder the fake resolves to the minute ahead ("soon") is
// already due when the sweep looks, and a spec does not wait out a real minute.

test('a capture becomes a real push through the emitter', async ({ request }) => {
  test.setTimeout(ciTimeout(90_000))
  await withRelay(201, async (endpoint, captured) => {
    const keys = deviceKeys()
    await signInRequest(request, uniqueEmail('emit'))
    expect((await subscribeRequest(request, endpoint, keys)).status()).toBe(204)

    expect((await createReminder(request, 'buy glue soon')).status()).toBe(201)

    await expect.poll(() => captured.length, { timeout: ciTimeout(45_000) }).toBe(1)
    const payload = JSON.parse(decryptPush(captured[0].body, keys))
    expect(payload.notification.title).toBe('buy glue')
    // The instant is rendered in the zone the reminder was scheduled in, with a label beside
    // it; the host's own tzdata decides whether that label reads "SGT" or "+08".
    expect(payload.notification.body).toMatch(/^\d{1,2}:\d{2} [AP]M \S+$/)
    expect(payload.notification.navigate).toBe('/reminders')
    // The attempt's Trace row, which the service worker keeps for notification feedback.
    expect(payload.notificationTraceId).toBeGreaterThan(0)

    // Firing is durable, and the page's own API is where the user sees it: a one-off that
    // fired belongs to the past.
    const fired = await waitForReminder(request, 'buy glue', 'past', ciTimeout(20_000))
    expect(fired.status).toBe('fired')
  })
})

test('a reminder days out is scheduled, not sent', async ({ request }) => {
  test.setTimeout(ciTimeout(60_000))
  await withRelay(201, async (endpoint, captured) => {
    const keys = deviceKeys()
    await signInRequest(request, uniqueEmail('later'))
    expect((await subscribeRequest(request, endpoint, keys)).status()).toBe(204)
    expect((await createReminder(request, 'take pills every day at 6pm')).status()).toBe(201)

    const scheduled = await waitForReminder(request, 'take pills', 'upcoming', ciTimeout(20_000))
    expect(scheduled.rule?.frequency).toBe('daily')
    // A repeat fires on its next occurrence, and that occurrence is what the sweep reads: it
    // is still ahead, so nothing can fire while this spec watches.
    expect(new Date(scheduled.nextEmitAtUtc as string).getTime()).toBeGreaterThan(Date.now())
    await new Promise((resolve) => setTimeout(resolve, ciTimeout(5_000)))

    // The stack's clock runs 90s fast, so this holds unless a run lands in the last 90
    // seconds before the daily 18:00. The exhaustive horizon cases are Go tests.
    expect(captured).toHaveLength(0)
  })
})

test('a relay that refuses is recorded, and the reminder stays fired', async ({ request }) => {
  test.setTimeout(ciTimeout(90_000))
  await withRelay(500, async (endpoint, captured) => {
    const keys = deviceKeys()
    await signInRequest(request, uniqueEmail('refused'))
    expect((await subscribeRequest(request, endpoint, keys)).status()).toBe(204)
    expect((await createReminder(request, 'post letter soon')).status()).toBe(201)

    // One attempt, then silence: the reminder fired either way, so the next sweep has nothing
    // left to send. A retry is the duplicate the PRD ranks as its own failure mode.
    await expect.poll(() => captured.length, { timeout: ciTimeout(45_000) }).toBe(1)
    expect((await waitForReminder(request, 'post letter', 'past', ciTimeout(20_000))).status).toBe('fired')
    await new Promise((resolve) => setTimeout(resolve, ciTimeout(4_000)))
    expect(captured).toHaveLength(1)
  })
})

test('a dead endpoint is forgotten, and the firing still happens', async ({ request }) => {
  test.setTimeout(ciTimeout(90_000))
  await withRelay(410, async (endpoint, captured) => {
    const keys = deviceKeys()
    await signInRequest(request, uniqueEmail('dead'))
    expect((await subscribeRequest(request, endpoint, keys)).status()).toBe(204)
    expect((await createReminder(request, 'post letter soon')).status()).toBe(201)
    await expect.poll(() => captured.length, { timeout: ciTimeout(45_000) }).toBe(1)
    expect((await waitForReminder(request, 'post letter', 'past', ciTimeout(20_000))).status).toBe('fired')

    // The endpoint was pruned, so a second reminder fires with nobody to reach — and it is
    // still recorded as fired, because the Reminder's own lifecycle does not depend on the
    // relay answering.
    expect((await createReminder(request, 'buy stamps soon')).status()).toBe(201)
    expect((await waitForReminder(request, 'buy stamps', 'past', ciTimeout(20_000))).status).toBe('fired')
    expect(captured).toHaveLength(1)
  })
})