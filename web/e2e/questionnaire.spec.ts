import { expect, test } from '@playwright/test'
import { completeQuestionnaire, fillQuestionnaire, passPermissions, signIn, uniqueEmail } from './helpers'

// Real browser, real Go server, real SQLite, real session cookie, real PIN
// minted by the server (fixed by BLURT_FAKE_PIN, never mailed). No mocks.
test('questionnaire gates home, saves answers, and remembers completion', async ({ page }) => {
  const email = uniqueEmail('questionnaire')

  await signIn(page, { name: 'Q', email })
  await passPermissions(page)

  // Unfinished setup cannot reach home.
  await expect(page).toHaveURL(/\/questionnaire$/)
  await expect(page.getByText('When do you usually wake up and go to sleep?')).toBeVisible()
  await page.goto('/home')
  // A full page load is a new entry, so the gate runs again.
  await passPermissions(page)
  await expect(page).toHaveURL(/\/questionnaire$/)

  await completeQuestionnaire(page)
  await expect(page.getByTestId('calendar-month')).toBeVisible()

  const me = await (await page.request.get('/api/user')).json()
  expect(me.user.lastQuestionnaireVersion).toBe(2)

  // Completion is server state: a reload keeps the gate open.
  await page.reload()
  await passPermissions(page)
  await expect(page).toHaveURL(/\/home$/)

  // A brand new session for the same email also skips the questionnaire.
  await page.getByTestId('cancel-audio').click()
  await page.getByRole('button', { name: 'Sign out' }).click()
  await expect(page).toHaveURL(/\/login$/)
  await signIn(page, { name: 'Q', email })
  // signIn starts with a full page load, so the gate runs again.
  await passPermissions(page)
  await expect(page).toHaveURL(/\/home$/)

  const again = await (await page.request.get('/api/user')).json()
  expect(again.user.lastQuestionnaireVersion).toBe(2)
})

// A direct link is not a way around setup: the version gate wraps every inside
// route, so even a Job link asks an unfinished user to finish first. (/traces is
// not the example any more: it is the owner's tooling, not a user's route.)
test('questionnaire freshness gates direct routes, not just home', async ({ page }) => {
  await signIn(page, { name: 'Q3', email: uniqueEmail('direct') })
  await passPermissions(page)
  await expect(page).toHaveURL(/\/questionnaire$/)

  await page.goto('/jobs/1')
  await passPermissions(page)
  await expect(page).toHaveURL(/\/questionnaire$/)

  await completeQuestionnaire(page)
  await page.goto('/jobs/1')
  await passPermissions(page)
  await expect(page).toHaveURL(/\/jobs\/1$/)

  // A current questionnaire sends the setup route back to Home.
  await page.goto('/questionnaire')
  await passPermissions(page)
  await expect(page).toHaveURL(/\/home$/)
})

// A missing cookie is not the same as an expired row (the Go tests cover real
// expiry), but the UI path is the same: a 401 sends the user back to login.
test('missing session during setup drops back to login', async ({ page, context }) => {
  await signIn(page, { name: 'Q2', email: uniqueEmail('sessionless') })
  await passPermissions(page)
  await expect(page).toHaveURL(/\/questionnaire$/)

  await fillQuestionnaire(page)
  await context.clearCookies()
  await page.getByTestId('questionnaire-submit').click()

  await expect(page).toHaveURL(/\/login$/)
})