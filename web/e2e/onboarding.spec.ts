import { expect, test } from '@playwright/test'
import { DEV_PIN, OWNER_STATE, uniqueEmail } from './helpers'

// The whole door, in one test: a stranger applies on /login, cannot get in, the
// owner approves them in the portal, and only then does signing in work.
test('an applicant is approved by the owner and can then sign in', async ({ page, browser }) => {
  const email = uniqueEmail('applicant')

  // 1. The applicant asks for access from the public page.
  await page.goto('/login')
  await page.getByTestId('login-apply').click()
  // The install note rides under the submit button, so the approval email opens
  // the app they already have on their phone.
  await expect(page.getByTestId('install-note')).toContainText('Add to Home Screen')
  await page.getByTestId('login-name').fill('Sam')
  await page.getByTestId('login-email').fill(email)
  await page.getByTestId('apply-submit').click()
  await expect(page.getByTestId('login-title')).toHaveText('Application received')

  // 2. A pending account cannot get in, even with the dev PIN: the PIN is never
  //    minted for a row that is still waiting.
  await page.getByTestId('applied-back').click()
  await page.getByTestId('login-have-account').click()
  await page.getByTestId('login-email').fill(email)
  await page.getByTestId('login-submit').click()
  await page.getByTestId('login-pin').fill(DEV_PIN)
  await page.getByTestId('login-verify').click()
  await expect(page.getByRole('alert')).toHaveText('That PIN is wrong or expired.')
  await expect(page).toHaveURL(/\/login$/)

  // 3. The owner sees the application and approves it. Their session comes from
  //    globalSetup, so this never competes for the owner's single-use PIN.
  const owner = await browser.newContext({ storageState: OWNER_STATE })
  const ownerPage = await owner.newPage()
  await ownerPage.goto('/admin')
  // Other specs may have applications waiting in the same queue, so this test
  // only ever speaks about its own row.
  const row = ownerPage.getByTestId('admin-user').filter({ hasText: email })
  await expect(row).toContainText('Sam')
  await row.getByTestId('admin-approve').click()
  await expect(row).toHaveCount(0)
  await expect(ownerPage.getByTestId('admin-approved')).toContainText(email)
  await owner.close()

  // 4. Now the applicant is let in, through the same form as before.
  await page.goto('/login')
  await page.getByTestId('login-have-account').click()
  await page.getByTestId('login-email').fill(email)
  await page.getByTestId('login-submit').click()
  await page.getByTestId('login-pin').fill(DEV_PIN)
  await page.getByTestId('login-verify').click()
  await expect(page).toHaveURL(/\/permissions$/)

  // 5. And the portal is not theirs: only the owner reaches /admin.
  await page.goto('/admin')
  await expect(page).not.toHaveURL(/\/admin$/)
})
