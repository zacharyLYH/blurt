// The clock is the one thing an emitter cannot be exercised without: a reminder a day out
// only fires when a day passes. Rather than wait, this restarts a server with the emitter's
// clock (BLURT_FAKE_CLOCK_FAST_FORWARD_AMOUNT) set past that horizon — which is also the only
// way to prove the wiring, because a fresh process has to pick up what the last one left due
// and must not ring what it already rang.
//
// A restart takes its stack with it, so this spec runs on a stack of its own (clock-stack.ts)
// and can run beside the rest of the suite instead of ahead of it.
import { expect, request, test, type APIRequestContext } from '@playwright/test'

import { createReminder, subscribeDevice, waitForReminder } from './api'
import { recreate, startClockStack, stopClockStack } from './clock-stack'
import { BASE_URL, CLOCK_URL, ciTimeout } from './env'
import { DEV_PIN, OWNER, uniqueEmail } from './helpers'
import { decryptPush, deviceKeys } from './push-crypto'
import { withRelay } from './relay'

test.beforeAll(async () => {
  test.setTimeout(ciTimeout(300_000))
  await startClockStack()
})

test.afterAll(async () => {
  test.setTimeout(ciTimeout(120_000))
  await stopClockStack()
})

// signIn walks the app's own door on this stack: the owner signs in, approves
// the application, and the applicant verifies a PIN. The returned context keeps
// the applicant's cookie, so everything after this is that user's own.
async function signIn(email: string): Promise<APIRequestContext> {
  const owner = await request.newContext({ baseURL: CLOCK_URL, extraHTTPHeaders: { Origin: BASE_URL } })
  try {
    await owner.post('/api/auth/request-pin', { data: { email: OWNER } })
    await owner.post('/api/auth/verify-pin', { data: { email: OWNER, pin: DEV_PIN } })
    await owner.post('/api/auth/apply', { data: { name: 'Clock', email } })
    const { users } = (await (await owner.get('/api/admin/users')).json()) as {
      users: { userId: number; email: string }[]
    }
    const applicant = users.find((user) => user.email === email)
    if (!applicant) throw new Error(`no application row for ${email}`)
    expect((await owner.post(`/api/admin/users/${applicant.userId}/approve`)).status()).toBe(200)
  } finally {
    await owner.dispose()
  }
  const user = await request.newContext({ baseURL: CLOCK_URL, extraHTTPHeaders: { Origin: BASE_URL } })
  await user.post('/api/auth/request-pin', { data: { email } })
  expect((await user.post('/api/auth/verify-pin', { data: { email, pin: DEV_PIN } })).status()).toBe(200)
  return user
}

test('a reminder a day out fires when the clock catches up, and never twice', async () => {
  test.setTimeout(ciTimeout(240_000))
  await withRelay(201, async (endpoint, captured) => {
    const keys = deviceKeys()
    const user = await signIn(uniqueEmail('clock'))
    try {
      // The new device is answered once for the subscription itself; the relay
      // holds that confirmation until the restart below makes the reminder fire.
      await subscribeDevice(user, endpoint, keys, captured)

      // "at 6pm tomorrow" is at least eighteen hours out, whatever time of day the run happens
      // to be: the fake resolves it against the real clock, so nothing short of a restart can
      // fire it.
      expect((await createReminder(user, 'pay the rent at 6pm tomorrow')).status()).toBe(201)
      const scheduled = await waitForReminder(user, 'pay the rent', 'upcoming', ciTimeout(20_000))
      expect(new Date(scheduled.emitAtUtc as string).getTime() - Date.now()).toBeGreaterThan(60 * 60 * 1000)

      // The first sweep after boot sees a Reminder that arrived while the server was down, so
      // the long horizon is crossed without waiting for it. It lands behind the confirmation.
      await recreate('48h')
      await expect.poll(() => captured.length, { timeout: ciTimeout(60_000) }).toBe(2)
      expect(JSON.parse(decryptPush(captured[1].body, keys)).notification.title).toBe('pay the rent')
      expect((await waitForReminder(user, 'pay the rent', 'past', ciTimeout(20_000))).status).toBe('fired')

      // A restart also proves the other half of a one-off's lifecycle: it is already fired, so a
      // process that boots after the firing stays quiet.
      await recreate('90s')
      await new Promise((resolve) => setTimeout(resolve, ciTimeout(5_000)))
      expect(captured).toHaveLength(2)
    } finally {
      await user.dispose()
    }
  })
})
