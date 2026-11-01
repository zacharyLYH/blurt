import { expect, test, type Page } from '@playwright/test'
import { completeQuestionnaire, signIn, uniqueEmail } from './helpers'

// Visual proof for the everyday screens: login, the recording boom, the idle
// home, and a clarification thread. Everything a run cannot reproduce — the
// account email, the push status, and the recording countdown — is masked.

async function signInFresh(page: Page): Promise<void> {
  await signIn(page, { name: 'Visual', email: uniqueEmail('visual') })
  await completeQuestionnaire(page)
}

// The boom's ring breathes with the live mic, and its countdown ticks. Both are
// motion rather than layout, so pause the clock and pin the ring before the shot.
async function pauseBoom(page: Page): Promise<void> {
  await page.clock.pauseAt(await page.evaluate(() => Date.now() + 5_000))
  await page.evaluate(() => {
    for (const ring of document.querySelectorAll<HTMLElement>('[data-testid="boom"] > span')) {
      ring.style.setProperty('transform', 'translate(-50%, -50%)', 'important')
    }
  })
}

function dynamic(page: Page) {
  return [page.getByTestId('home-email'), page.getByTestId('push-status')]
}

async function captureEverydayScreens(page: Page, suffix: string): Promise<void> {
  // Fake timers run normally through setup; only the recording shot pauses them.
  await page.clock.install({ time: new Date('2030-03-05T09:00:00+08:00') })
  await signInFresh(page)
  await expect(page.getByTestId('mic-status')).toHaveText('Mic ready')
  await expect(page.getByTestId('boom-countdown')).toBeVisible()
  await pauseBoom(page)
  await expect(page).toHaveScreenshot(`voice-${suffix}.png`, {
    fullPage: true,
    animations: 'disabled',
    mask: [page.getByTestId('boom-countdown'), ...dynamic(page)],
  })
  await page.clock.resume()

  // The same screen once a capture is not running: the idle boom and composer.
  await page.getByTestId('cancel-audio').click()
  await expect(page.getByTestId('boom-countdown')).toHaveCount(0)
  await expect(page).toHaveScreenshot(`home-${suffix}.png`, {
    fullPage: true,
    animations: 'disabled',
    mask: dynamic(page),
  })

  // A capture with no clock becomes a question, which is the chat state.
  await page.getByTestId('capture-text').fill('buy glue')
  await page.getByTestId('send-text').click()
  await expect(page.getByTestId('job-message').first()).toHaveText('buy glue')
  await expect(page.getByTestId('job-message').nth(1)).toHaveText('What should I remind you about, and when?')
  await expect(page.getByTestId('capture-status')).toHaveCount(0)
  await expect(page.getByTestId('open-job')).toContainText('Needs your answer')
  await expect(page).toHaveScreenshot(`chat-${suffix}.png`, {
    fullPage: true,
    animations: 'disabled',
    mask: dynamic(page),
  })
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

  test('voice, home and chat, mobile', async ({ page }) => {
    await captureEverydayScreens(page, 'mobile')
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

  test('voice, home and chat, desktop', async ({ page }) => {
    await captureEverydayScreens(page, 'desktop')
  })
})
