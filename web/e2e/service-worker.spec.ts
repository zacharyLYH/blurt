import { expect, test } from '@playwright/test'
import { BASE_URL } from './env'
import { deliverPush } from './fake-relay'
import { completeQuestionnaire, signIn, uniqueEmail } from './helpers'

// Runs against the real docker stack (nginx + Go server), with only the push
// transport played by the test. These are the guarantees the PWA depends on:
// the worker installs and takes control, first login completes, and the push
// handler produces a notification that points back into the app.
test.describe('service worker', () => {
  test('registers, activates, and controls the app', async ({ page, request }) => {
    await page.goto('/login')

    // ready resolves before activation finishes; wait for the real state.
    await expect
      .poll(() => page.evaluate(() => navigator.serviceWorker.getRegistration().then((reg) => reg?.active?.state ?? null)))
      .toBe('activated')
    const scope = await page.evaluate(() => navigator.serviceWorker.getRegistration().then((reg) => reg?.scope ?? null))
    expect(scope).toBe(`${BASE_URL}/`)

    await page.reload()
    await expect.poll(() => page.evaluate(() => Boolean(navigator.serviceWorker.controller))).toBe(true)

    const manifest = await (await request.get('/manifest.webmanifest')).json()
    expect(manifest.start_url).toBe('/')
    expect(manifest.icons.length).toBeGreaterThan(0)
    for (const icon of manifest.icons as { src: string }[]) {
      expect((await request.get(icon.src)).status()).toBe(200)
    }
  })

  test('first login runs the questionnaire then lands on /home under the worker', async ({ page }) => {
    await signIn(page, { name: 'E2E', email: uniqueEmail('sw') })
    await completeQuestionnaire(page)

    await expect(page.getByTestId('home-heading')).toContainText('Hi E2E')
    await expect(page.getByTestId('push-status')).toBeVisible()

    await expect
      .poll(() => page.evaluate(() => navigator.serviceWorker.getRegistration().then((reg) => reg?.active?.state ?? null)))
      .toBe('activated')
    await expect.poll(() => page.evaluate(() => Boolean(navigator.serviceWorker.controller))).toBe(true)

    // Setup is done: the gate stays open when the app is loaded again.
    await page.goto('/home')
    await expect(page).toHaveURL(/\/home$/)
    await expect(page.getByTestId('home-heading')).toContainText('Hi E2E')
  })

  test('push payload becomes a notification aimed at /home', async ({ page, context }) => {
    await context.grantPermissions(['notifications'], { origin: BASE_URL })
    await page.goto('/login')
    await expect
      .poll(() => page.evaluate(() => navigator.serviceWorker.getRegistration().then((reg) => reg?.active?.state ?? null)))
      .toBe('activated')

    const shown = await deliverPush(context, {
      notification: {
        title: 'Buy glue',
        body: 'On the way home from work. 6:00 PM SGT',
        tag: 'reminder-42',
        navigate: '/home',
      },
      notificationTraceId: 42,
    })

    expect(shown).toHaveLength(1)
    expect(shown[0].title).toBe('Buy glue')
    expect(shown[0].body).toContain('6:00 PM')
    expect(shown[0].tag).toBe('reminder-42')
    expect(shown[0].navigate).toBe('/home')
  })
})