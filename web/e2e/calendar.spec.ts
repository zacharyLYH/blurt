import { expect, test, type Page } from '@playwright/test'
import { createReminder, waitForReminder } from './api'
import { ciTimeout, visualProofs } from './env'
import { completeQuestionnaire, passPermissions, signIn, uniqueEmail } from './helpers'

// The calendar is Reminder-only and month-scoped. These specs seed reminders on fixed
// days (the dev model accepts a pinned date) so the month grid, and its screenshots,
// do not move with the run date. react-day-picker owns the month math; the page owns
// the dots and the selected-day list.

// Seed a month of fixed reminders over the page's own session, then open that month.
// The calendar is Home now, so it runs the same setup gate: finish the
// questionnaire, then land on the month the spec seeded.
async function openMonth(page: Page) {
  await signIn(page, { name: 'Calendar', email: uniqueEmail('calendar') })
  await completeQuestionnaire(page)
  const captures = ['buy glue at 9am on 2030-03-05', 'post the letter at 6pm on 2030-03-19']
  for (const capture of captures) {
    expect((await createReminder(page.request, capture)).status()).toBe(201)
  }
  for (const text of ['buy glue', 'post the letter']) {
    await waitForReminder(page.request, text, 'upcoming', ciTimeout(45_000))
  }

  await page.goto('/calendar?month=2030-03')
  await passPermissions(page)
  // A full load opens the boom again; the month grid is what these specs are
  // about, so step out of the recording first.
  const cancel = page.getByTestId('cancel-audio')
  await cancel.waitFor({ state: 'visible', timeout: 5_000 }).catch(() => undefined)
  if (await cancel.isVisible()) await cancel.click()
}

// seedSecondMoment adds a second time to the day the fixtures already use, so the
// day's timeline has two bullets to join.
async function seedSecondMoment(page: Page) {
  expect((await createReminder(page.request, 'call mum at 6pm on 2030-03-05')).status()).toBe(201)
  await waitForReminder(page.request, 'call mum', 'upcoming', ciTimeout(45_000))
  await page.goto('/home?month=2030-03')
  await passPermissions(page)
  const cancel = page.getByTestId('cancel-audio')
  await cancel.waitFor({ state: 'visible', timeout: 5_000 }).catch(() => undefined)
  if (await cancel.isVisible()) await cancel.click()
  await expect(page.getByTestId('calendar-entry')).toHaveCount(2)
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
  // The day also opened its own timeline; close it to keep moving the month.
  await expect(page.getByTestId('day-modal')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.getByTestId('day-modal')).toHaveCount(0)

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

test('a typed blurt lands on the month without a reload', async ({ page }) => {
  await signIn(page, { name: 'Calendar', email: uniqueEmail('calendar-live') })
  await completeQuestionnaire(page)
  await page.getByTestId('cancel-audio').click()

  // A month of its own, so the arrival cannot be confused with anything the
  // rest of the run left behind.
  await page.goto('/home?month=2030-03')
  await passPermissions(page)
  const cancel = page.getByTestId('cancel-audio')
  await cancel.waitFor({ state: 'visible', timeout: 5_000 }).catch(() => undefined)
  if (await cancel.isVisible()) await cancel.click()
  await expect(page.getByTestId('calendar-entry')).toHaveCount(0)

  await page.getByTestId('capture-text').fill('buy stamps at 9am on 2030-03-05')
  await page.getByTestId('send-text').click()

  // The receipt is a toast, and the countdown to the next refresh is what
  // brings the reminder in without the user touching the page.
  await expect(page.getByTestId('toast')).toContainText('Sent to Blurt')
  await expect(page.getByTestId('poll-countdown')).toContainText('Updating in')
  await expect(page.getByTestId('calendar-entry')).toHaveCount(1, { timeout: ciTimeout(20_000) })
  await expect(page.getByTestId('calendar-day-list')).toContainText('buy stamps')
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

test('a day calls a reminder off, and its dot goes', async ({ page }) => {
  await openMonth(page)
  await page.getByTestId('calendar-day-2030-03-05').click()
  await expect(page.getByTestId('day-modal')).toBeVisible()

  await page.getByTestId('cancel-reminder').click()

  // The day empties, and so does the month: a called-off reminder emits nothing.
  await expect(page.getByTestId('day-reminder')).toHaveCount(0)
  await page.keyboard.press('Escape')
  await expect(page.getByTestId('calendar-entry')).toHaveCount(0)
  await expect(page.getByTestId('calendar-empty')).toBeVisible()
})

test('one user never sees another user’s month', async ({ page }) => {
  await signIn(page, { name: 'Other', email: uniqueEmail('calendar-other') })
  await completeQuestionnaire(page)
  await page.goto('/calendar?month=2030-03')
  await passPermissions(page)
  await expect(page.getByTestId('calendar-entry')).toHaveCount(0)
})

test.describe('visual proof, mobile', () => {
  test.skip(!visualProofs, 'visual proofs run where the baselines live: make e2e-visual')
  test.use({ viewport: { width: 390, height: 844 }, timezoneId: 'Asia/Singapore' })

  test('calendar, mobile', async ({ page }) => {
    await openMonth(page)
    await expect(page.getByTestId('calendar-entry')).toHaveCount(1)
    await expect(page).toHaveScreenshot('calendar-mobile.png', {
      fullPage: true,
      animations: 'disabled',
    })
  })

  test('a day opens its timeline, mobile', async ({ page }) => {
    await openMonth(page)
    await seedSecondMoment(page)
    await page.getByTestId('calendar-day-2030-03-05').click()
    await expect(page.getByTestId('day-modal')).toBeVisible()
    await expect(page.getByTestId('day-time')).toHaveCount(2)
    await expect(page).toHaveScreenshot('day-mobile.png', {
      fullPage: true,
      animations: 'disabled',
    })
  })
})

test.describe('visual proof, desktop', () => {
  test.skip(!visualProofs, 'visual proofs run where the baselines live: make e2e-visual')
  test.use({ viewport: { width: 1280, height: 900 }, timezoneId: 'Asia/Singapore' })

  test('calendar, desktop', async ({ page }) => {
    await openMonth(page)
    await expect(page.getByTestId('calendar-entry')).toHaveCount(1)
    await expect(page).toHaveScreenshot('calendar-desktop.png', {
      fullPage: true,
      animations: 'disabled',
    })
  })

  test('a day opens its timeline, desktop', async ({ page }) => {
    await openMonth(page)
    await seedSecondMoment(page)
    await page.getByTestId('calendar-day-2030-03-05').click()
    await expect(page.getByTestId('day-modal')).toBeVisible()
    await expect(page.getByTestId('day-time')).toHaveCount(2)
    await expect(page).toHaveScreenshot('day-desktop.png', {
      fullPage: true,
      animations: 'disabled',
    })
  })
})
