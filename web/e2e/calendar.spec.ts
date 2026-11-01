import { expect, test, type Page } from '@playwright/test'
import { createReminder, waitForReminder } from './api'
import { ciTimeout } from './env'
import { passPermissions, signIn, uniqueEmail } from './helpers'

// The calendar is Reminder-only and month-scoped. These specs seed reminders on fixed
// days (the dev model accepts a pinned date) so the month grid, and its screenshots,
// do not move with the run date. react-day-picker owns the month math; the page owns
// the dots and the selected-day list.

// Seed a month of fixed reminders over the page's own session, then open that month.
async function openMonth(page: Page) {
  await signIn(page, { name: 'Calendar', email: uniqueEmail('calendar') })
  await passPermissions(page)
  const captures = ['buy glue at 9am on 2030-03-05', 'post the letter at 6pm on 2030-03-19']
  for (const capture of captures) {
    expect((await createReminder(page.request, capture)).status()).toBe(201)
  }
  for (const text of ['buy glue', 'post the letter']) {
    await waitForReminder(page.request, text, 'upcoming', ciTimeout(45_000))
  }

  await page.goto('/calendar?month=2030-03')
  await passPermissions(page)
}

test('a month shows its reminders, and changing month refetches', async ({ page }) => {
  await openMonth(page)

  await expect(page.getByTestId('calendar-month')).toHaveText('March 2030')
  // The first reminder day is selected, so its list is open on arrival.
  await expect(page.getByTestId('calendar-day-list')).toContainText('buy glue')
  await expect(page.getByTestId('calendar-entry')).toHaveCount(1)

  // Both days carry a dot; only the selected day has rows.
  await expect(page.getByTestId('calendar-day-2030-03-05')).toBeVisible()
  await page.getByTestId('calendar-day-2030-03-19').click()
  await expect(page.getByTestId('calendar-day-list')).toContainText('post the letter')
  await expect(page.getByTestId('calendar-entry')).toHaveCount(1)

  // Loading is a real state on a month change, not a blank grid.
  await page.route('**/api/notifications?*', async (route) => {
    await new Promise((resolve) => setTimeout(resolve, 250))
    await route.continue()
  })
  await page.getByTestId('calendar-next').click()
  await expect(page.getByTestId('calendar-loading')).toBeVisible()
  await expect(page.getByTestId('calendar-month')).toHaveText('April 2030')
  await expect(page.getByTestId('calendar-day-list')).toBeVisible()
  await expect(page.getByTestId('calendar-entry')).toHaveCount(0)
  await page.unroute('**/api/notifications?*')

  // Back to the seeded month: the grid is refetched, not cached.
  await page.getByTestId('calendar-prev').click()
  await expect(page.getByTestId('calendar-entry')).toHaveCount(1)
  await expect(page.getByTestId('calendar-day-list')).toContainText('buy glue')
})

test('an entry opens its reminder, which is the notification', async ({ page }) => {
  await openMonth(page)
  await page.getByTestId('calendar-entry').first().click()

  await expect(page).toHaveURL(/\/notifications\/\d+$/)
  await expect(page.getByTestId('notification-text')).toHaveText('buy glue')
  // Feedback is offered, not pushed; the page still opens before the reminder fires.
  await expect(page.getByTestId('feedback-toggle')).toBeVisible()
  await expect(page.getByTestId('feedback-submit')).toHaveCount(0)
})

test('one user never sees another user’s month', async ({ page }) => {
  await signIn(page, { name: 'Other', email: uniqueEmail('calendar-other') })
  await passPermissions(page)
  await page.goto('/calendar?month=2030-03')
  await passPermissions(page)
  await expect(page.getByTestId('calendar-entry')).toHaveCount(0)
})

test.describe('visual proof, mobile', () => {
  test.use({ viewport: { width: 390, height: 844 }, timezoneId: 'Asia/Singapore' })

  test('calendar, mobile', async ({ page }) => {
    await openMonth(page)
    await expect(page.getByTestId('calendar-entry')).toHaveCount(1)
    await expect(page).toHaveScreenshot('calendar-mobile.png', {
      fullPage: true,
      animations: 'disabled',
    })
  })
})

test.describe('visual proof, desktop', () => {
  test.use({ viewport: { width: 1280, height: 900 }, timezoneId: 'Asia/Singapore' })

  test('calendar, desktop', async ({ page }) => {
    await openMonth(page)
    await expect(page.getByTestId('calendar-entry')).toHaveCount(1)
    await expect(page).toHaveScreenshot('calendar-desktop.png', {
      fullPage: true,
      animations: 'disabled',
    })
  })
})
