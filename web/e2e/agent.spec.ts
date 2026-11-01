import { expect, test, type Page } from '@playwright/test'
import { waitForReminder } from './api'
import { ciTimeout } from './env'
import { completeQuestionnaire, denyMicrophone, signIn, uniqueEmail } from './helpers'

// The e2e stack runs the scripted model (BLURT_FAKE_AGENT), so a capture resolves
// deterministically: one with a clear time becomes a Reminder and closes, and one
// with no time in it asks the user and resumes on the answer.

// openQuestion walks the only path into a thread: the capture, then its inbox row.
async function openQuestion(page: Page, capture: string): Promise<void> {
  await page.getByTestId('capture-text').fill(capture)
  await page.getByTestId('send-text').click()
  await page.getByTestId('inbox-toggle').click()
  await page.getByTestId('inbox-item').first().click()
  await expect(page.getByTestId('job-thread')).toBeVisible()
}

test('a clear capture becomes a Reminder and the Job closes', async ({ page }) => {
  await denyMicrophone(page)
  await signIn(page, { name: 'Lam', email: uniqueEmail('agent') })
  await completeQuestionnaire(page)

  await page.getByTestId('capture-text').fill('buy glue at 6pm tomorrow')
  await page.getByTestId('send-text').click()

  // The write landed, the Job closed, and a closed Job is nothing to answer.
  const reminder = await waitForReminder(page.request, 'buy glue', 'upcoming', ciTimeout(30_000))
  expect(reminder.status).toBe('resolved')

  await page.getByTestId('inbox-toggle').click()
  await expect(page.getByTestId('inbox-item')).toHaveCount(0)
  await expect(page.getByTestId('inbox-empty')).toBeVisible()
})

test('a capture with no time asks, and the answer finishes the Job', async ({ page }) => {
  await denyMicrophone(page)
  await signIn(page, { name: 'Lam', email: uniqueEmail('clarify') })
  await completeQuestionnaire(page)

  // The time cannot be pinned from "when I'm home", so the agent asks rather than
  // inventing one. The wording is the model's, and this stack has no model, so the spec
  // asserts the shape — a question — rather than a sentence.
  await openQuestion(page, "buy glue when I'm home")
  await expect(page.getByTestId('job-message').nth(1)).toHaveText(/\?$/)

  await page.getByTestId('reply-text').fill('at 6pm tomorrow')
  await page.getByTestId('send-reply').click()

  await expect(page.getByTestId('job-message').nth(2)).toHaveText('at 6pm tomorrow')
  await expect(page.getByTestId('job-message').nth(3)).toHaveText('Reminder set.')
  // The question is answered, so the reply composer goes away.
  await expect(page.getByTestId('reply-text')).toHaveCount(0)
})

test('a capture that repeats closes on the rule', async ({ page }) => {
  await denyMicrophone(page)
  await signIn(page, { name: 'Lam', email: uniqueEmail('repeat') })
  await completeQuestionnaire(page)

  // The scripted model reads the frequency off the word, which is the only way this stack
  // reaches a recurring write. Nothing in the thread distinguishes a rule from a one-off
  // yet, so the spec pins the flow: the repeat commits instead of asking.
  await page.getByTestId('capture-text').fill('take pills every day at 6pm')
  await page.getByTestId('send-text').click()

  await waitForReminder(page.request, 'take pills', 'upcoming', ciTimeout(30_000))
  await page.getByTestId('inbox-toggle').click()
  await expect(page.getByTestId('inbox-item')).toHaveCount(0)
})

test('a capture with two events asks for a split and sets nothing', async ({ page }) => {
  await denyMicrophone(page)
  await signIn(page, { name: 'Lam', email: uniqueEmail('split') })
  await completeQuestionnaire(page)

  // One write is one reminder, and the scripted model cannot say which of two events
  // to take first, so it proposes no records at all and the Job asks instead of
  // silently dropping the second one.
  await openQuestion(page, 'dentist at 5pm and trash at 9pm')
  await expect(page.getByTestId('job-message').first()).toHaveText('dentist at 5pm and trash at 9pm')
  await expect(page.getByTestId('job-message').nth(1)).toHaveText(/\?$/)
  // The Job stays open for the answer, so the reply composer is still there.
  await expect(page.getByTestId('reply-text')).toBeVisible()
})
