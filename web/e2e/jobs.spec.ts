import { expect, test } from '@playwright/test'
import { completeQuestionnaire, denyMicrophone, passPermissions, signIn, uniqueEmail } from './helpers'

const CAPTURE = 'buy glue'

test('an open capture shows in the sidebar and opens on its own page', async ({ page }) => {
  await denyMicrophone(page)
  await signIn(page, { name: 'Lam', email: uniqueEmail('jobs') })
  await completeQuestionnaire(page)

  await page.getByTestId('capture-text').fill(CAPTURE)
  await page.getByTestId('send-text').click()
  await expect(page.getByTestId('job-message').first()).toHaveText(CAPTURE)

  // The list is what makes a capture reachable after the user moves on.
  const row = page.getByTestId('open-job').first()
  await expect(row).toContainText(CAPTURE)
  await row.click()

  await expect(page).toHaveURL(/\/jobs\/\d+$/)
  await expect(page.getByTestId('job-message').first()).toHaveText(CAPTURE)
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
  await expect(page.getByTestId('job-message').first()).toHaveText(CAPTURE)

  const href = await page.getByTestId('open-job').first().getAttribute('href')
  expect(href).toMatch(/\/jobs\/\d+$/)

  await page.goto(href ?? '/home')
  // A full load is a new entry, so the gate runs again before the Job page.
  await passPermissions(page)
  await expect(page).toHaveURL(/\/jobs\/\d+$/)
  await expect(page.getByTestId('job-message').first()).toHaveText(CAPTURE)
})