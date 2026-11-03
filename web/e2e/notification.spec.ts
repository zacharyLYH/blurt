import { expect, test, type BrowserContext, type Page } from '@playwright/test'
import { BASE_URL, ciTimeout, visualProofs } from './env'
import { deliverPush, clickNotification } from './fake-relay'
import { completeQuestionnaire, passPermissions, signIn, uniqueEmail } from './helpers'
import { createReminder, subscribeDevice, waitForReminder } from './api'
import { decryptPush, deviceKeys } from './push-crypto'
import { withRelay } from './relay'

// The emitter's real output, all the way into the browser: the Go sweep finds a due
// Reminder, encrypts a push at a relay the test runs, and the payload is decrypted and
// delivered as a real PushEvent to the app's registered service worker. The tap runs the
// worker's own notificationclick handler, which opens the page a user would land on.

// pushOpenNotification runs the whole pipeline and returns the reminder it opened.
async function pushOpenNotification(page: Page, context: BrowserContext): Promise<number> {
  let opened = 0
  await withRelay(201, async (endpoint, captured) => {
    const keys = deviceKeys()
    await context.grantPermissions(['notifications'], { origin: BASE_URL })
    // The app's own subscribe handoff is blocked so the API subscription below is
    // the one the emitter uses. page.request bypasses page routes, so it still lands.
    await page.route('**/api/push/subscribe', (route) => route.abort())
    await signIn(page, { name: 'Feedback', email: uniqueEmail('feedback-push') })
    // Setup comes before any inside route, a notification deep link included.
    await completeQuestionnaire(page)
    await expect
      .poll(() => page.evaluate(() => navigator.serviceWorker.getRegistration().then((reg) => reg?.active?.state ?? null)))
      .toBe('activated')

    // The subscription itself is answered once, so the relay already holds that
    // confirmation when the reminder below comes due.
    await subscribeDevice(page.request, endpoint, keys, captured)
    expect((await createReminder(page.request, 'buy glue soon')).status()).toBe(201)

    await expect.poll(() => captured.length, { timeout: ciTimeout(45_000) }).toBe(2)
    const payload = JSON.parse(decryptPush(captured[1].body, keys)) as {
      notification: { title: string; body: string; navigate: string }
      reminderId: number
    }
    expect(payload.reminderId).toBeGreaterThan(0)
    expect(payload.notification.navigate).toBe(`/notifications/${payload.reminderId}`)

    const shown = await deliverPush(context, payload)
    expect(shown).toHaveLength(1)
    expect(shown[0].title).toBe('buy glue')
    expect(shown[0].reminderId).toBe(payload.reminderId)
    expect(shown[0].navigate).toBe(`/notifications/${payload.reminderId}`)

    await clickNotification(context, payload.reminderId)
    // The worker navigates the window, which is a fresh app entry: the permission gate
    // runs again and returns to the path it was asked for.
    await passPermissions(page)
    await expect(page).toHaveURL(new RegExp(`/notifications/${payload.reminderId}$`))
    opened = payload.reminderId
  })
  return opened
}

// openNotification opens a pinned-date reminder directly, which keeps the screenshot's
// "Scheduled" line static. The push pipeline itself is covered above.
async function openNotification(page: Page, capture: string, text: string): Promise<number> {
  await signIn(page, { name: 'Feedback', email: uniqueEmail('feedback') })
  await completeQuestionnaire(page)
  expect((await createReminder(page.request, capture)).status()).toBe(201)
  const reminder = await waitForReminder(page.request, text, 'upcoming', ciTimeout(45_000))
  await page.goto(`/notifications/${reminder.reminderId}`)
  await passPermissions(page)
  return reminder.reminderId
}

test('a real emitter push lands in the real service worker and opens the notification page', async ({ page, context }) => {
  test.setTimeout(ciTimeout(90_000))
  await pushOpenNotification(page, context)
  await expect(page.getByTestId('notification-text')).toHaveText('buy glue')
})

test('feedback submitted from the real push path is stored, and a revisit offers it again', async ({ page, context }) => {
  test.setTimeout(ciTimeout(90_000))
  await pushOpenNotification(page, context)
  await expect(page.getByTestId('notification-text')).toHaveText('buy glue')
  // Nothing to report is the common case: the form is not on screen until asked for.
  await expect(page.getByTestId('feedback-submit')).toHaveCount(0)
  await page.getByTestId('feedback-toggle').click()
  await expect(page.getByTestId('feedback-submit')).toBeDisabled()

  await page.getByTestId('feedback-wrong_date_time_zone').click()
  await page.getByTestId('feedback-comment').fill('an hour early')
  await page.getByTestId('feedback-submit').click()

  await expect(page.getByTestId('feedback-done')).toBeVisible()
  await expect(page.getByTestId('feedback-submit')).toHaveCount(0)

  // A recurring reminder reports each firing: coming back offers the form again
  // rather than a permanent "done".
  await page.reload()
  await passPermissions(page)
  await page.getByTestId('feedback-toggle').click()
  await expect(page.getByTestId('feedback-submit')).toBeVisible()
  await page.getByTestId('feedback-wrong_reminder').click()
  await page.getByTestId('feedback-submit').click()
  await expect(page.getByTestId('feedback-done')).toBeVisible()
})

test('a missing or foreign notification has no form', async ({ page }) => {
  await signIn(page, { name: 'Gone', email: uniqueEmail('gone') })
  await completeQuestionnaire(page)
  await page.goto('/notifications/9999999')
  await passPermissions(page)

  await expect(page.getByTestId('notification-gone')).toBeVisible()
  await expect(page.getByTestId('feedback-submit')).toHaveCount(0)
})

test.describe('visual proof', () => {
  test.skip(!visualProofs, 'visual proofs run where the baselines live: make e2e-visual')
  test.use({ viewport: { width: 390, height: 844 } })

  test('notification page, mobile', async ({ page }) => {
    await openNotification(page, 'buy glue at 9am on 2030-03-05', 'buy glue')
    await expect(page.getByTestId('notification-text')).toHaveText('buy glue')
    // The pinned date makes the scheduled line static; nothing is redacted.
    await expect(page).toHaveScreenshot('notification-mobile.png', {
      fullPage: true,
      animations: 'disabled',
    })
  })
})

test.describe('visual proof, desktop', () => {
  test.skip(!visualProofs, 'visual proofs run where the baselines live: make e2e-visual')
  test.use({ viewport: { width: 1280, height: 800 } })

  test('notification page, desktop', async ({ page }) => {
    await openNotification(page, 'buy glue at 9am on 2030-03-05', 'buy glue')
    await expect(page.getByTestId('notification-text')).toHaveText('buy glue')
    await expect(page).toHaveScreenshot('notification-desktop.png', {
      fullPage: true,
      animations: 'disabled',
    })
  })
})
