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
  // The agent's own turn lands after it in the same thread.
  await expect(page.getByTestId('job-message').nth(1)).toHaveText('Reminder set for tomorrow at 6pm.')
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
  await expect(page.getByTestId('job-message').nth(3)).toHaveText('Reminder set for tomorrow at 6pm.')
  // The question is answered, so the reply composer goes away.
  await expect(page.getByTestId('reply-text')).toHaveCount(0)
})

test('a capture with two events asks for a split and sets nothing', async ({ page }) => {
  await denyMicrophone(page)
  await signIn(page, { name: 'Lam', email: uniqueEmail('split') })
  await completeQuestionnaire(page)

  await page.getByTestId('capture-text').fill('dentist at 5pm and trash at 9pm')
  await page.getByTestId('send-text').click()

  await expect(page.getByTestId('job-message').first()).toHaveText('dentist at 5pm and trash at 9pm')
  // Two clocks are more independent items than the one-reminder shape can carry, so
  // the scripted model asks for a split and proposes no records at all.
  await expect(page.getByTestId('job-message').nth(1)).toHaveText(
    'I can only take one reminder at a time. Which one should I set first?',
  )
  // The Job stays open for the answer, and nothing closed on a partial commit.
  await expect(page.getByTestId('open-job')).toHaveCount(1)
  await expect(page.getByTestId('reply-text')).toBeVisible()
})