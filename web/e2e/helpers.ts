import { readFileSync } from 'node:fs'
import { expect, request, type Page } from '@playwright/test'
import { BASE_URL } from './env'

// The stack runs with BLURT_FAKE_PIN (see docker-compose.e2e.yml), so the real
// server accepts this fixed code and mails nothing: login needs no mailbox for
// a test to read, and everything else stays real.
export const DEV_PIN = '123456'

// OWNER is the address docker-compose.e2e.yml names as the app's owner. Boot
// seeds it active, so a spec can sign in as it and approve everyone else.
export const OWNER = 'owner@example.com'

// OWNER_STATE is the cookie globalSetup leaves behind: the owner signed in once,
// so no two workers race for the same single-use PIN.
export const OWNER_STATE = 'test-results/owner.json'

// Read the same versioned file the app imports, without relying on Node's
// JSON import attributes.
const questionnaire = JSON.parse(
  readFileSync(new URL('../src/data/questionnaire/v1.json', import.meta.url), 'utf8'),
) as {
  version: number
  items: { id: string; kind: 'text' | 'timeRange' | 'choice'; default?: string }[]
}

export function uniqueEmail(prefix: string): string {
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}@example.com`
}

// createAccount walks the real door: the applicant applies, the owner approves.
// Every test account starts as an application, so the app's own gate is what a
// spec exercises rather than a row written behind the API's back. It is
// idempotent, so a spec that signs in twice is fine.
export async function createAccount(name: string, email: string): Promise<void> {
  const api = await request.newContext({
    baseURL: BASE_URL,
    storageState: OWNER_STATE,
    extraHTTPHeaders: { Origin: BASE_URL },
  })
  try {
    await api.post('/api/auth/apply', { data: { name, email } })
    const list = (await (await api.get('/api/admin/users')).json()) as {
      users: { userId: number; email: string }[]
    }
    const applicant = list.users.find((user) => user.email === email)
    if (!applicant) throw new Error(`no application row for ${email}`)
    await api.post(`/api/admin/users/${applicant.userId}/approve`)
  } finally {
    await api.dispose()
  }
}

// Real login path: the account is created and approved, then the browser asks
// the server for a PIN, types the dev code, and verifies.
export async function signIn(page: Page, { name, email }: { name: string; email: string }): Promise<void> {
  await createAccount(name, email)
  await page.goto('/login')
  await page.getByTestId('login-have-account').click()
  await page.getByTestId('login-email').fill(email)
  await page.getByTestId('login-submit').click()

  await expect(page.getByTestId('login-pin')).toBeVisible()
  await page.getByTestId('login-pin').fill(DEV_PIN)
  await page.getByTestId('login-verify').click()
}

const answers: Record<string, string> = {
  home_area: 'Tampines',
  work_days: 'weekdays',
  work_place: 'home',
  commute: 'mrt',
}

// Answers the real question set from the versioned JSON file, stopping on
// the final question so the caller decides when to submit. Time ranges keep
// their prefilled defaults; every choice is selected explicitly so the spec
// does not depend on the browser or the clock.
export async function fillQuestionnaire(page: Page): Promise<void> {
  for (const [index, item] of questionnaire.items.entries()) {
    if (item.kind === 'choice') {
      await page.getByTestId(`questionnaire-${item.id}-${answers[item.id] ?? item.default}`).click()
    } else if (item.kind === 'text') {
      await page.getByTestId(`questionnaire-${item.id}`).fill(answers[item.id] ?? '')
    }
    if (index < questionnaire.items.length - 1) await page.getByTestId('questionnaire-next').click()
  }
}

export async function completeQuestionnaire(page: Page): Promise<void> {
  await passPermissions(page)
  await expect(page).toHaveURL(/\/questionnaire$/)
  await fillQuestionnaire(page)
  await page.getByTestId('questionnaire-submit').click()
  await expect(page).toHaveURL(/\/home$/)
}

// The permission gate runs on every app entry (and on every full page load),
// ahead of the questionnaire. It is tolerant because the gate only appears while
// something still needs attention, so callers do not have to know which load
// they are on.
export async function passPermissions(page: Page): Promise<void> {
  const gate = page.getByTestId('permissions-continue')
  await gate.waitFor({ state: 'visible', timeout: 5_000 }).catch(() => undefined)
  if (await gate.isVisible()) await gate.click()
}

// A revoked microphone, or one that was never granted. A page cannot un-grant a
// permission, so the init script rejects getUserMedia instead; without this,
// --use-fake-ui-for-media-stream auto-accepts.
export async function denyMicrophone(page: Page): Promise<void> {
  await page.addInitScript(() => {
    if (!navigator.mediaDevices) return
    Object.defineProperty(navigator.mediaDevices, 'getUserMedia', {
      configurable: true,
      value: () => Promise.reject(new DOMException('denied', 'NotAllowedError')),
    })
  })
}