import { expect, test, type Page } from '@playwright/test'
import { createReminder, signInRequest } from './api'
import { completeQuestionnaire, passPermissions, signIn, uniqueEmail } from './helpers'

// The page the emitter's notifications open into, against the real stack: real Reminders,
// real statuses, and a call-off that moves one from upcoming to past without deleting it.

// openReminders runs the browser's own login and then lands on the reminders route: the
// questionnaire gate lives on Home, so a page that only reads reminders does not need it.
async function openReminders(page: Page, prefix: string) {
  await signIn(page, { name: 'Lam', email: uniqueEmail(prefix) })
  await page.goto('/reminders')
  await passPermissions(page)
}

test('a scheduled reminder shows up, and calling it off keeps it as history', async ({ page }) => {
  await openReminders(page, 'reminders')
  await expect(page.getByTestId('reminder-upcoming-empty')).toBeVisible()

  // The capture runs over the page's own session, so the reminder belongs to the user the
  // browser is signed in as.
  expect((await createReminder(page.request, 'buy glue at 6pm tomorrow')).status()).toBe(201)
  await page.reload()
  await passPermissions(page)

  await expect(page.getByTestId('reminder-upcoming').getByTestId('reminder-row')).toHaveCount(1)
  await expect(page.getByTestId('reminder-text')).toHaveText('buy glue')
  // The instant reads in the zone the reminder was scheduled in, not the device's.
  await expect(page.getByTestId('reminder-when')).toContainText('6:00 PM')
  await expect(page.getByTestId('reminder-upcoming')).toContainText('Scheduled')

  await page.getByTestId('cancel-reminder').click()

  // The row changes sides rather than vanishing: the status is the record.
  await expect(page.getByTestId('reminder-upcoming-empty')).toBeVisible()
  await expect(page.getByTestId('reminder-past').getByTestId('reminder-row')).toHaveCount(1)
  await expect(page.getByTestId('reminder-past')).toContainText('Called off')

  // And it stays called off, so a reload does not resurrect it.
  await page.reload()
  await passPermissions(page)
  await expect(page.getByTestId('reminder-past').getByTestId('reminder-row')).toHaveCount(1)
  await expect(page.getByTestId('reminder-upcoming-empty')).toBeVisible()
})

test('a repeat shows the pattern it repeats on', async ({ page }) => {
  await openReminders(page, 'repeat-page')

  expect((await createReminder(page.request, 'take pills every day at 6pm')).status()).toBe(201)
  await page.reload()
  await passPermissions(page)

  await expect(page.getByTestId('reminder-text')).toHaveText('take pills')
  await expect(page.getByTestId('reminder-when')).toContainText('Every day')
  // A repeat is still a scheduled reminder, so it can be called off like any other.
  await expect(page.getByTestId('cancel-reminder')).toHaveCount(1)
})

test('Home is the way in, and a notification opens the same page', async ({ page }) => {
  await signIn(page, { name: 'Lam', email: uniqueEmail('home-link') })
  await completeQuestionnaire(page)

  await page.getByTestId('reminders-link').click()
  await expect(page).toHaveURL(/\/reminders$/)
  // A cold start on the deep link renders the same page under the same gate.
  await page.goto('/reminders')
  await passPermissions(page)
  await expect(page.getByRole('heading', { name: 'Reminders' })).toBeVisible()
})

test('one user never sees another user’s reminders', async ({ page, playwright }) => {
  // A reminder for somebody else, committed over a session of its own.
  const other = await playwright.request.newContext()
  const email = uniqueEmail('other-user')
  await signInRequest(other, email)
  expect((await createReminder(other, 'buy glue at 6pm tomorrow')).status()).toBe(201)
  await other.dispose()

  await openReminders(page, 'isolated')
  await expect(page.getByTestId('reminder-upcoming-empty')).toBeVisible()
  await expect(page.getByTestId('reminder-row')).toHaveCount(0)
})