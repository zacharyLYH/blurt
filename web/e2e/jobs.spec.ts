import { expect, test } from '@playwright/test'
import { completeQuestionnaire, denyMicrophone, passPermissions, signIn, uniqueEmail } from './helpers'

const CAPTURE = 'buy glue'

// A capture never opens a conversation by itself: it lands on the calendar, and
// the only door into the thread is its row in the inbox.
test('a question shows in the inbox and only its row opens the thread', async ({ page }) => {
  await denyMicrophone(page)
  await signIn(page, { name: 'Lam', email: uniqueEmail('jobs') })
  await completeQuestionnaire(page)

  await page.getByTestId('capture-text').fill(CAPTURE)
  await page.getByTestId('send-text').click()

  await expect(page).toHaveURL(/\/home$/)
  await expect(page.getByTestId('job-thread')).toHaveCount(0)

  await page.getByTestId('inbox-toggle').click()
  const row = page.getByTestId('inbox-item').first()
  await expect(row).toContainText(CAPTURE)
  await row.click()

  await expect(page).toHaveURL(/\/jobs\/\d+$/)
  await expect(page.getByTestId('job-message').first()).toHaveText(CAPTURE)

  // The way out is on the page, and it goes back where the user was.
  await page.getByTestId('exit-chat').click()
  await expect(page).toHaveURL(/\/home$/)
})

// A notification links straight at a Job, so a cold start on that URL has to
// end up there. The permission gate runs on every app entry — ahead of the
// questionnaire — so it has to hand the user on to where they were headed.
test('a Job deep link survives the permission gate', async ({ page }) => {
  await denyMicrophone(page)
  await signIn(page, { name: 'Lam', email: uniqueEmail('deeplink') })
  await completeQuestionnaire(page)

  await page.getByTestId('capture-text').fill(CAPTURE)
  await page.getByTestId('send-text').click()

  await page.getByTestId('inbox-toggle').click()
  const href = await page.getByTestId('inbox-item').first().getAttribute('href')
  expect(href).toMatch(/\/jobs\/\d+$/)

  await page.goto(href ?? '/home')
  // A full load is a new entry, so the gate runs again before the Job page.
  await passPermissions(page)
  await expect(page).toHaveURL(/\/jobs\/\d+$/)
  await expect(page.getByTestId('job-message').first()).toHaveText(CAPTURE)
})
