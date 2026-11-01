import { expect, test, type BrowserContext, type Page } from '@playwright/test'
import { BASE_URL } from './env'
import { fillQuestionnaire, passPermissions, signIn } from './helpers'

// Visual proof for the everyday screens: login, the recording boom, the
// calendar Home, the inbox over it, and a thread opened from a row. Each
// viewport gets its own fixed account, and the e2e volume is wiped per run, so
// the data behind the shots is stable.

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
  await page.clock.install({ time: new Date('2030-03-05T09:00:00+08:00') })
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
  await page.clock.resume()

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
  await expect(page).toHaveURL(/\/home$/)
  await expect(page.getByTestId('job-thread')).toHaveCount(0)

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
  test.use({ viewport: { width: 390, height: 844 }, timezoneId: 'Asia/Singapore' })

  test('login, mobile', async ({ page }) => {
    await page.goto('/login')
    await expect(page.getByTestId('login-submit')).toBeVisible()
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
  test.use({ viewport: { width: 1280, height: 900 }, timezoneId: 'Asia/Singapore' })

  test('login, desktop', async ({ page }) => {
    await page.goto('/login')
    await expect(page.getByTestId('login-submit')).toBeVisible()
    await expect(page).toHaveScreenshot('login-desktop.png', {
      fullPage: true,
      animations: 'disabled',
    })
  })

  test('voice, home and chat, desktop', async ({ page, context }) => {
    await captureEverydayScreens(page, context, 'desktop')
  })
})
