import { expect, test } from '@playwright/test'
import { completeQuestionnaire, fillQuestionnaire, signIn, uniqueEmail } from './helpers'

// Real browser, real Go server, real SQLite, real session cookie, real PIN
// minted by the server (fixed by BLURT_FAKE_PIN, never mailed). No mocks.
test('questionnaire gates home, saves answers, and remembers completion', async ({ page }) => {
  const email = uniqueEmail('questionnaire')

  await signIn(page, { name: 'Q', email })

  // Unfinished setup cannot reach home.
  await expect(page).toHaveURL(/\/questionnaire$/)
  await expect(page.getByText('What time zone are you usually in?')).toBeVisible()
  await page.goto('/home')
  await expect(page).toHaveURL(/\/questionnaire$/)

  await completeQuestionnaire(page)
  await expect(page.getByTestId('home-heading')).toContainText('Hi Q')

  const me = await (await page.request.get('/api/user')).json()
  expect(me.user.lastQuestionnaireVersion).toBe(1)

  // Completion is server state: a reload keeps the gate open.
  await page.reload()
  await expect(page).toHaveURL(/\/home$/)

  // A brand new session for the same email also skips the questionnaire.
  await page.getByRole('button', { name: 'Sign out' }).click()
  await expect(page).toHaveURL(/\/login$/)
  await signIn(page, { name: 'Q', email })
  await expect(page).toHaveURL(/\/home$/)

  const again = await (await page.request.get('/api/user')).json()
  expect(again.user.lastQuestionnaireVersion).toBe(1)
})

// A missing cookie is not the same as an expired row (the Go tests cover real
// expiry), but the UI path is the same: a 401 sends the user back to login.
test('missing session during setup drops back to login', async ({ page, context }) => {
  await signIn(page, { name: 'Q2', email: uniqueEmail('sessionless') })
  await expect(page).toHaveURL(/\/questionnaire$/)

  await fillQuestionnaire(page)
  await context.clearCookies()
  await page.getByTestId('questionnaire-submit').click()

  await expect(page).toHaveURL(/\/login$/)
})