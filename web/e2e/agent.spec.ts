import { expect, test } from '@playwright/test'
import { completeQuestionnaire, denyMicrophone, signIn, uniqueEmail } from './helpers'

// The e2e stack runs the scripted model (BLURT_FAKE_AGENT), so a capture resolves
// deterministically: one with a clear time becomes a Reminder and closes, and one
// with no time in it asks the user and resumes on the answer.
test('a clear capture becomes a Reminder and the Job closes', async ({ page }) => {
  await denyMicrophone(page)
  await signIn(page, { name: 'Lam', email: uniqueEmail('agent') })
  await completeQuestionnaire(page)

  await page.getByTestId('capture-text').fill('buy glue at 6pm tomorrow')
  await page.getByTestId('send-text').click()

  await expect(page.getByTestId('job-message').first()).toHaveText('buy glue at 6pm tomorrow')
  // The agent's own turn lands after it in the same thread. The reply is the
  // server's own sentence, because the structured schema has no field for prose.
  await expect(page.getByTestId('job-message').nth(1)).toHaveText('Reminder set.')
  // A completed Job leaves the open-conversation sidebar.
  await expect(page.getByTestId('open-job')).toHaveCount(0)
})

test('a capture with no time asks, and the answer finishes the Job', async ({ page }) => {
  await denyMicrophone(page)
  await signIn(page, { name: 'Lam', email: uniqueEmail('clarify') })
  await completeQuestionnaire(page)

  await page.getByTestId('capture-text').fill("buy glue when I'm home")
  await page.getByTestId('send-text').click()

  // The time cannot be pinned from "when I'm home", so the agent asks rather than
  // inventing one. The wording is the model's, and this stack has no model, so the spec
  // asserts the shape — a question — rather than a sentence.
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

  await expect(page.getByTestId('job-message').first()).toHaveText('take pills every day at 6pm')
  await expect(page.getByTestId('job-message').nth(1)).toHaveText('Reminder set.')
  await expect(page.getByTestId('open-job')).toHaveCount(0)
})

test('a capture with two events asks for a split and sets nothing', async ({ page }) => {
  await denyMicrophone(page)
  await signIn(page, { name: 'Lam', email: uniqueEmail('split') })
  await completeQuestionnaire(page)

  await page.getByTestId('capture-text').fill('dentist at 5pm and trash at 9pm')
  await page.getByTestId('send-text').click()

  await expect(page.getByTestId('job-message').first()).toHaveText('dentist at 5pm and trash at 9pm')
  // One write is one reminder, and the scripted model cannot say which of two events
  // to take first, so it proposes no records at all and the Job asks instead of
  // silently dropping the second one.
  await expect(page.getByTestId('job-message').nth(1)).toHaveText(/\?$/)
  // The Job stays open for the answer, and nothing closed on a partial commit.
  await expect(page.getByTestId('open-job')).toHaveCount(1)
  await expect(page.getByTestId('reply-text')).toBeVisible()
})