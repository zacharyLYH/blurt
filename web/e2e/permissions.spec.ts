import { expect, test } from '@playwright/test'
import { BASE_URL } from './env'
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

// Turning notifications on is a permission prompt, a key fetch and a
// subscription. The row has to say it is working rather than look stuck, and
// the button must not fire twice while it does.
test('turning notifications on shows a busy state until the handoff settles', async ({ page, context }) => {
  await signIn(page, { name: 'Lam', email: uniqueEmail('notify') })
  await expect(page).toHaveURL(/\/permissions$/)

  const row = page.getByTestId('permissions-notifications')
  const button = page.getByTestId('permissions-notify-enable')
  // The mount check finishes on its own, so the button is there to press.
  await expect(button).toBeVisible()

  // Grant the permission behind the page's back, then hold the key fetch so the
  // handoff is observably in flight. Releasing it fails the fetch, which settles
  // the row back on "not asked" — no relay or real subscription needed.
  await context.grantPermissions(['notifications'], { origin: BASE_URL })
  let release: () => void = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  await page.route('**/api/push/key', async (route) => {
    await held
    await route.abort()
  })

  await button.click()
  await expect(row).toContainText('Setting up…')
  await expect(button).toBeDisabled()

  release()
  await expect(row).not.toContainText('Setting up…')
  await expect(button).toBeEnabled()
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
  // let the user through without trapping them.
  await page.getByTestId('permissions-continue').click()
  await expect(page).toHaveURL(/\/home$/)
  // The app does not open the mic on entry; the denial surfaces when the halo
  // asks for it.
  await page.getByTestId('halo').click()
  await expect(page.getByTestId('mic-status')).toHaveText('Mic blocked')
})