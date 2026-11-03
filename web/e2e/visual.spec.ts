import { expect, test, type BrowserContext, type Page } from '@playwright/test'
import { BASE_URL, visualProofs } from './env'
import { fillQuestionnaire, OWNER_STATE, passPermissions, signIn } from './helpers'

// Visual proof for the everyday screens: login, the recording boom, the
// calendar Home, the inbox over it, a thread opened from a row, and the owner's
// access queue. Each viewport gets its own fixed account, the e2e volume is
// wiped per run, and this file is its own project so it runs after every
// behaviour spec — the data behind the shots is what those specs left behind.

// The boom's wave is driven per frame and its countdown ticks. Pause the clock
// at a fixed point after recording starts, then pin the pulses to a still frame
// so the shot does not depend on which frame it landed on.
async function pauseBoom(page: Page): Promise<void> {
  await page.clock.pauseAt(await page.evaluate(() => Date.now() + 10_000))
  // Let React apply the tick the pause fired before pinning the wave.
  await page.waitForTimeout(100)
  await page.evaluate(() => {
    // Newest pulse nearest the button, the rest a step further out and dimmer.
    document.querySelectorAll<HTMLElement>('[data-testid="boom"] > span').forEach((ring, index) => {
      ring.style.setProperty('transform', `translate(-50%, -50%) scale(${0.45 + index * 0.2})`, 'important')
      ring.style.setProperty('opacity', `${Math.max(0.1, 0.5 - index * 0.1)}`, 'important')
    })
  })
}

async function signInFresh(page: Page, context: BrowserContext, suffix: string): Promise<void> {
  // Grant the permission but block the key fetch, so setupPush settles on the
  // same "pending" state in every browser instead of depending on a real relay.
  await context.grantPermissions(['notifications'], { origin: BASE_URL })
  await page.route('**/api/push/key', (route) => route.abort())
  await signIn(page, { name: 'Visual', email: `visual-${suffix}@example.com` })
  await passPermissions(page)
  await expect(page).toHaveURL(/\/questionnaire$/)
  await fillQuestionnaire(page)
  // Start the clock just before Home mounts, so the recording shot is placed.
  // It is paused at once — the countdown is a clock reading, so every later
  // advance has to be one this file chose. The target has to be ahead of the
  // clock (it is already ticking), and two seconds is a fixed, known cost.
  const at = new Date('2030-03-05T09:00:00+08:00')
  await page.clock.install({ time: at })
  await page.clock.pauseAt(await page.evaluate(() => Date.now() + 2_000))
  await page.getByTestId('questionnaire-submit').click()
  await expect(page).toHaveURL(/\/home$/)
}

async function captureEverydayScreens(page: Page, context: BrowserContext, suffix: string): Promise<void> {
  await signInFresh(page, context, suffix)

  await expect(page.getByTestId('mic-status')).toHaveText('Mic ready')
  await expect(page.getByTestId('boom-countdown')).toBeVisible()
  await pauseBoom(page)
  await expect(page).toHaveScreenshot(`voice-${suffix}.png`, {
    fullPage: true,
    animations: 'disabled',
  })
  // The clock stays paused from here on: the composer's countdown is a clock
  // reading, so a shot is only reproducible at a fixed one. Cancelling needs no
  // timers.
  //
  // Cancelling drops the user on the calendar, which is Home now: the month,
  // the halo, and the composer.
  await page.getByTestId('cancel-audio').click()
  await expect(page.getByTestId('boom-countdown')).toHaveCount(0)
  await expect(page.getByTestId('calendar-month')).toBeVisible()
  await expect(page).toHaveScreenshot(`home-${suffix}.png`, {
    fullPage: true,
    animations: 'disabled',
  })

  // A typed capture stays on the calendar; its question waits in the inbox.
  await page.getByTestId('capture-text').fill('buy glue')
  await page.getByTestId('send-text').click()
  await expect(page.getByTestId('toast')).toBeVisible()
  await expect(page).toHaveURL(/\/home$/)
  await expect(page.getByTestId('job-thread')).toHaveCount(0)

  // Five seconds of the fake clock is exactly one beat: the page re-asks on its
  // own, the receipt has been dismissed, and the countdown is back at 5s.
  await page.clock.runFor(5_000)
  await expect(page.getByTestId('toast')).toHaveCount(0)
  // The agent is a real worker on the other side of the API, so the beat can
  // land before the question exists. Give it wall-clock time, then open the
  // inbox — which re-asks on the way in.
  await page.waitForTimeout(2_000)
  await page.getByTestId('inbox-toggle').click()
  await expect(page.getByTestId('inbox-item')).toContainText('buy glue')
  await expect(page).toHaveScreenshot(`inbox-${suffix}.png`, {
    fullPage: true,
    animations: 'disabled',
  })

  // Only an inbox row opens the thread, and the thread exits back to the calendar.
  await page.getByTestId('inbox-item').first().click()
  await expect(page.getByTestId('job-message').first()).toHaveText('buy glue')
  await expect(page.getByTestId('job-message').nth(1)).toHaveText('What should I remind you about, and when?')
  await expect(page).toHaveScreenshot(`chat-${suffix}.png`, {
    fullPage: true,
    animations: 'disabled',
  })

  await page.getByTestId('exit-chat').click()
  await expect(page.getByTestId('calendar-month')).toBeVisible()
}

test.describe('visual proof, mobile', () => {
  test.skip(!visualProofs, 'visual proofs run where the baselines live: make e2e-visual')
  test.use({ viewport: { width: 390, height: 844 }, timezoneId: 'Asia/Singapore' })

  test('login, mobile', async ({ page }) => {
    // The sign-in step, which is what a returning user lands on.
    await page.goto('/login')
    await page.getByTestId('login-have-account').click()
    await page.getByTestId('login-email').fill('lam@example.com')
    await expect(page.getByTestId('login-submit')).toBeEnabled()
    await expect(page).toHaveScreenshot('login-mobile.png', {
      fullPage: true,
      animations: 'disabled',
    })
  })

  test('voice, home and chat, mobile', async ({ page, context }) => {
    await captureEverydayScreens(page, context, 'mobile')
  })
})

test.describe('visual proof, desktop', () => {
  test.skip(!visualProofs, 'visual proofs run where the baselines live: make e2e-visual')
  test.use({ viewport: { width: 1280, height: 900 }, timezoneId: 'Asia/Singapore' })

  test('login, desktop', async ({ page }) => {
    // The sign-in step, which is what a returning user lands on.
    await page.goto('/login')
    await page.getByTestId('login-have-account').click()
    await page.getByTestId('login-email').fill('lam@example.com')
    await expect(page.getByTestId('login-submit')).toBeEnabled()
    await expect(page).toHaveScreenshot('login-desktop.png', {
      fullPage: true,
      animations: 'disabled',
    })
  })

  test('voice, home and chat, desktop', async ({ page, context }) => {
    await captureEverydayScreens(page, context, 'desktop')
  })
})

// The owner's verdict has to be reachable on the day it matters, so the portal
// gets the same pixel proof as the rest of the product.
test.describe('visual proof: access requests', () => {
  test.skip(!visualProofs, 'visual proofs run where the baselines live: make e2e-visual')
  // The owner's own session, so the shot is the portal and not the login form.
  test.use({ storageState: OWNER_STATE })

  for (const [suffix, viewport] of [
    ['mobile', { width: 390, height: 844 }],
    ['desktop', { width: 1280, height: 900 }],
  ] as const) {
    test(`access requests, ${suffix}`, async ({ page, request }) => {
      await page.setViewportSize(viewport)
      // Two applications waiting, oldest first, so the queue is not empty.
      const wanted = [
        { name: 'Ada Lovelace', email: 'ada@example.com' },
        { name: 'Grace Hopper', email: 'grace@example.com' },
      ]
      for (const applicant of wanted) {
        const applied = await request.post('/api/auth/apply', {
          data: applicant,
          headers: { Origin: BASE_URL },
        })
        expect(applied.ok()).toBeTruthy()
      }

      // The volume is shared with every other spec, so the shot waits for the
      // queue to be exactly these two: a neighbour's application is approved a
      // moment after it is made, and the poll simply outlasts that window
      // without ever touching a row this test does not own.
      const wantedEmails = wanted.map((want) => want.email).sort()
      await expect
        .poll(
          async () => {
            const queue = (await (await request.get('/api/admin/users')).json()) as {
              users: { email: string; status: string }[]
            }
            return queue.users
              .filter((user) => user.status === 'pending')
              .map((user) => user.email)
              .sort()
          },
          { timeout: 30_000 },
        )
        .toEqual(wantedEmails)

      await page.goto('/admin')
      // The queue is the page's decision surface, and the only part of it this
      // test owns: the approved list is every account the whole run created, so
      // it is not something a baseline can promise.
      const queue = page.getByTestId('admin-queue')
      await expect(queue.getByTestId('admin-user')).toHaveCount(2)
      await expect(queue).toHaveScreenshot(`access-requests-${suffix}.png`, { animations: 'disabled' })
    })
  }
})
