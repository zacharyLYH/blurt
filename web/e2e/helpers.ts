import { readFileSync } from 'node:fs'
import { expect, type Page } from '@playwright/test'

// The stack runs with BLURT_FAKE_PIN (see docker-compose.e2e.yml), so the real
// server accepts this fixed code and mails nothing: login needs no mailbox for
// a test to read, and everything else stays real.
export const DEV_PIN = '123456'

// Read the same versioned file the app imports, without relying on Node's
// JSON import attributes.
const questionnaire = JSON.parse(
  readFileSync(new URL('../src/data/questionnaire/v1.json', import.meta.url), 'utf8'),
) as { version: number; questions: { id: string }[] }

export function uniqueEmail(prefix: string): string {
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}@example.com`
}

// Real login path: request a PIN from the server, type the dev code, verify.
export async function signIn(page: Page, { name, email }: { name: string; email: string }): Promise<void> {
  await page.goto('/login')
  await page.getByTestId('login-name').fill(name)
  await page.getByTestId('login-email').fill(email)
  await page.getByTestId('login-submit').click()

  await expect(page.getByTestId('login-pin')).toBeVisible()
  await page.getByTestId('login-pin').fill(DEV_PIN)
  await page.getByTestId('login-verify').click()
}

const answers: Record<string, string> = {
  timezone: 'Asia/Singapore',
  waking_hours: 'Wake 7am, sleep 11:30pm',
  work_hours: 'Weekdays 9am-6pm at the office',
  home: 'Tampines, by MRT from Raffles Place',
  quiet_hours: 'During meals and after 11pm',
}

// Answers the real question set from the versioned JSON file, stopping on
// the final question so the caller decides when to submit.
export async function fillQuestionnaire(page: Page): Promise<void> {
  for (const [index, question] of questionnaire.questions.entries()) {
    await page.getByTestId('questionnaire-answer').fill(answers[question.id] ?? 'Noted')
    if (index < questionnaire.questions.length - 1) await page.getByTestId('questionnaire-next').click()
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