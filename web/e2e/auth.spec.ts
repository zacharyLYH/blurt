import { expect, test } from '@playwright/test'
import { BASE_URL } from './env'
import { DEV_PIN, completeQuestionnaire, signIn, uniqueEmail } from './helpers'

// Real backend, no mail: BLURT_FAKE_PIN fixes the code, so this proves the
// server's CSRF guard, cookie flags, and PIN rules against the real stack.
test('request-pin mints a PIN and the session is CSRF-safe', async ({ page, request }) => {
  const email = uniqueEmail('auth')
  const origin = { Origin: BASE_URL }

  // No Origin, and a foreign Origin, are rejected before a PIN is minted.
  expect((await request.post('/api/auth/request-pin', { data: { email } })).status()).toBe(403)
  expect(
    (await request.post('/api/auth/request-pin', { data: { email }, headers: { Origin: 'https://evil.example' } })).status(),
  ).toBe(403)

  const sent = await request.post('/api/auth/request-pin', { data: { email }, headers: origin })
  expect(sent.status()).toBe(200)

  // The dev code is the only code that works, so a guess is refused.
  expect((await request.post('/api/auth/verify-pin', { data: { email, pin: '000000' }, headers: origin })).status()).toBe(401)

  const signedIn = await request.post('/api/auth/verify-pin', {
    data: { email, name: 'Auth', pin: DEV_PIN },
    headers: origin,
  })
  expect(signedIn.status()).toBe(200)
  const cookie = signedIn.headers()['set-cookie'] ?? ''
  expect(cookie).toContain('HttpOnly')
  expect(cookie).toContain('SameSite=Lax')
  expect(cookie).toContain('Path=/')

  // The cookie alone is not authority: a cross-origin write still fails.
  expect((await request.get('/api/user')).status()).toBe(200)
  const csrf = await request.post('/api/questionnaire', {
    data: { version: 1, answers: [{ question: 'q', answer: 'a' }] },
    headers: { Origin: 'https://evil.example' },
  })
  expect(csrf.status()).toBe(403)

  // HttpOnly: page scripts cannot read the token.
  await page.goto('/home')
  expect(await page.evaluate(() => document.cookie)).not.toContain('blurt_session')
})

// The UI path: a wrong code surfaces the server's message and stays on the step.
test('a wrong PIN shows the server message and stays on the PIN step', async ({ page }) => {
  await page.goto('/login')
  await page.getByTestId('login-name').fill('Auth')
  await page.getByTestId('login-email').fill(uniqueEmail('wrongpin'))
  await page.getByTestId('login-submit').click()

  await expect(page.getByTestId('login-pin')).toBeVisible()
  await page.getByTestId('login-pin').fill('000000')
  await page.getByTestId('login-verify').click()

  await expect(page.getByRole('alert')).toHaveText('That PIN is wrong or expired.')
  await expect(page).toHaveURL(/\/login$/)
  await expect(page.getByTestId('login-pin')).toBeVisible()
})

// /login is not a dead end for a signed-in visitor: the app sends them inside.
test('a live session skips the login form', async ({ page }) => {
  await signIn(page, { name: 'Auth', email: uniqueEmail('session') })
  await completeQuestionnaire(page)

  await page.goto('/login')
  await expect(page).toHaveURL(/\/home$/)
})