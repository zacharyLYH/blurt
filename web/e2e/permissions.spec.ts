import { expect, test } from '@playwright/test'
import { denyMicrophone, fillQuestionnaire, signIn, uniqueEmail } from './helpers'

// The gate sits between login and the questionnaire, on every app entry, because
// both permissions can be revoked between visits.
test('asks for microphone and notifications before the questionnaire', async ({ page }) => {
  await signIn(page, { name: 'Lam', email: uniqueEmail('perms') })

  await expect(page).toHaveURL(/\/permissions$/)
  // The e2e context grants the microphone, so the row reports it without a
  // prompt; the notification row is shown even while it is still unset.
  await expect(page.getByTestId('permissions-mic')).toContainText('Allowed')
  await expect(page.getByTestId('permissions-notifications')).toBeVisible()

  await page.getByTestId('permissions-continue').click()
  await expect(page).toHaveURL(/\/questionnaire$/)
})

// Revoked between visits: the next entry asks again, and never traps the user.
test('surfaces a revoked microphone on the next entry', async ({ page }) => {
  await signIn(page, { name: 'Lam', email: uniqueEmail('revoked') })
  await expect(page.getByTestId('permissions-mic')).toContainText('Allowed')
  await page.getByTestId('permissions-continue').click()

  await expect(page).toHaveURL(/\/questionnaire$/)
  await fillQuestionnaire(page)
  await page.getByTestId('questionnaire-submit').click()
  await expect(page).toHaveURL(/\/home$/)

  // Next entry, with the microphone revoked in browser settings.
  await denyMicrophone(page)
  await page.reload()

  await expect(page).toHaveURL(/\/permissions$/)
  await expect(page.getByTestId('permissions-mic')).toContainText('Blocked')
  // A blocked permission cannot be re-prompted by any page, so the gate has to
  // let the user through and lean on the text fallback.
  await page.getByTestId('permissions-continue').click()
  await expect(page).toHaveURL(/\/home$/)
  await expect(page.getByTestId('mic-status')).toHaveText('Mic blocked')
})