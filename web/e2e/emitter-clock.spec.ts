// The clock is the one thing an emitter cannot be exercised without: a reminder a day out
// only fires when a day passes. Rather than wait, this restarts the server with the emitter's
// clock (BLURT_FAKE_CLOCK) set past that horizon — which is also the only way to prove the
// wiring, because a fresh process has to pick up what the last one left due and must not ring
// what it already rang.
//
// It runs against the same stack as every other spec and puts the clock back before it ends,
// so the run's own file order does not matter.
//
// Adapted from Blurt's e2e conventions: the relay lives in this process (see relay.ts), which
// is why the restart is asynchronous — a blocking child would leave the relay unable to answer
// the very push the restart causes.
import { execFile } from 'node:child_process'

import { expect, test } from '@playwright/test'

import { createReminder, signInRequest, subscribeRequest, waitForReminder } from './api'
import { BASE_URL, COMPOSE_PROJECT, WEB_PORT, ciTimeout } from './env'
import { uniqueEmail } from './helpers'
import { decryptPush, deviceKeys } from './push-crypto'
import { withRelay } from './relay'

// The two compose files playwright.config.ts boots the stack with, so a restart lands on the
// same project, port and throwaway settings.
const COMPOSE = ['-f', '../docker-compose.yml', '-f', '../docker-compose.e2e.yml', '-p', COMPOSE_PROJECT]

// recreate rebuilds the server and the proxy in place, with the emitter's clock set to clock.
// The data volume is untouched, so the session, the subscription and the Reminder survive —
// which is the restart this spec is really about. The proxy is rebuilt with the server because
// nginx resolves the server's address when it loads its config.
function recreate(clock: string): Promise<void> {
  return new Promise((resolve, reject) => {
    execFile('docker', ['compose', ...COMPOSE, 'up', '-d', '--force-recreate', 'server', 'web'], {
      env: { ...process.env, BLURT_FAKE_CLOCK: clock, BLURT_WEB_PORT: String(WEB_PORT) },
    }, (err, _stdout, stderr) => {
      if (err) reject(new Error(`${err.message}\n${stderr}`))
      else resolve()
    })
  })
}

// waitForHealth waits on the same endpoint playwright.config.ts waits on before the first test.
async function waitForHealth(timeoutMs: number): Promise<void> {
  await expect
    .poll(async () => {
      try {
        return (await fetch(`${BASE_URL}/health`)).status
      } catch {
        return 0
      }
    }, { timeout: timeoutMs })
    .toBe(200)
}

// restoreClock is the default the other specs run on. It is called on the way out either way,
// so a failure in here cannot leave the stack two days ahead for the rest of the run.
async function restoreClock(): Promise<void> {
  await recreate('90s').catch(() => undefined)
  await waitForHealth(ciTimeout(60_000)).catch(() => undefined)
}

test('a reminder a day out fires when the clock catches up, and never twice', async ({ request }) => {
  test.setTimeout(ciTimeout(180_000))
  await withRelay(201, async (endpoint, captured) => {
    const keys = deviceKeys()
    await signInRequest(request, uniqueEmail('clock'))
    expect((await subscribeRequest(request, endpoint, keys)).status()).toBe(204)

    // "at 6pm tomorrow" is at least eighteen hours out, whatever time of day the run happens to
    // be: the fake resolves it against the real clock, so nothing short of a restart can fire it.
    expect((await createReminder(request, 'pay the rent at 6pm tomorrow')).status()).toBe(201)
    const scheduled = await waitForReminder(request, 'pay the rent', 'upcoming', ciTimeout(20_000))
    expect(new Date(scheduled.emitAtUtc as string).getTime() - Date.now()).toBeGreaterThan(60 * 60 * 1000)

    try {
      await recreate('48h')
      await waitForHealth(ciTimeout(60_000))

      // The first sweep after boot sees a Reminder that arrived while the server was down, so
      // the long horizon is crossed without waiting for it.
      await expect.poll(() => captured.length, { timeout: ciTimeout(60_000) }).toBe(1)
      expect(JSON.parse(decryptPush(captured[0].body, keys)).notification.title).toBe('pay the rent')
      expect((await waitForReminder(request, 'pay the rent', 'past', ciTimeout(20_000))).status).toBe('fired')

      // Back on the run's own clock, the restart also proves the other half of a one-off's
      // lifecycle: it is already fired, so a process that boots after the firing stays quiet.
      await recreate('90s')
      await waitForHealth(ciTimeout(60_000))
      await new Promise((resolve) => setTimeout(resolve, ciTimeout(5_000)))
      expect(captured).toHaveLength(1)
    } finally {
      await restoreClock()
    }
  })
})