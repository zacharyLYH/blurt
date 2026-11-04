import { expect, test } from '@playwright/test'
import { completeQuestionnaire, denyMicrophone, signIn, uniqueEmail } from './helpers'

// The e2e stack runs with BLURT_FAKE_STT=true (docker-compose.e2e.yml), so the
// transcript is deterministic while the rest of the chain stays real: browser
// capture, the WAV rewrite, upload, temp file, worker, Job Conversation.
const FAKE_TRANSCRIPT = 'Remind me to buy glue on the way home.'

test('records a clip, uploads it, and its question waits in the inbox', async ({ page }) => {
  await signIn(page, { name: 'Lam', email: uniqueEmail('voice') })
  await completeQuestionnaire(page)

  // Entry belongs to the composer: no microphone until the halo asks for one.
  await expect(page.getByTestId('capture-text')).toBeFocused()
  await expect(page.getByTestId('boom-countdown')).toHaveCount(0)
  await page.getByTestId('halo').click()
  // The mic opens and the boom is already recording: the countdown is the proof
  // that a clip is running.
  await expect(page.getByTestId('mic-status')).toHaveText('Mic ready')
  await expect(page.getByTestId('boom-countdown')).toBeVisible()
  // Long enough for the recorder to emit audio packets; the client refuses
  // anything under 300 ms anyway.
  await page.waitForTimeout(800)
  await page.getByTestId('send-audio').click()

  // The clip does not open a thread: it lands on the calendar, and the question
  // it becomes waits in the inbox.
  await expect(page.getByTestId('capture-status')).toHaveCount(0)
  await expect(page).toHaveURL(/\/home$/)
  await page.getByTestId('inbox-toggle').click()
  await expect(page.getByTestId('inbox-item')).toHaveCount(1)
})

// Cancel is the way out of a clip the user does not want, and it puts them
// straight into the composer rather than back at a menu.
test('cancelling a capture lands in the composer', async ({ page }) => {
  await signIn(page, { name: 'Lam', email: uniqueEmail('cancel') })
  await completeQuestionnaire(page)

  await page.getByTestId('halo').click()
  await expect(page.getByTestId('boom-countdown')).toBeVisible()
  await page.getByTestId('cancel-audio').click()
  await expect(page.getByTestId('boom-countdown')).toHaveCount(0)

  // No extra click needed: the caret is already where typing goes.
  await page.keyboard.type('buy glue')
  await expect(page.getByTestId('capture-text')).toHaveValue('buy glue')
})

// A denied mic must never trap the user: the composer is already the fallback.
test('falls back to typing when the mic is denied', async ({ page }) => {
  await denyMicrophone(page)

  await signIn(page, { name: 'Lam', email: uniqueEmail('fallback') })
  await completeQuestionnaire(page)

  // The mic is opened by the halo now, so the denial surfaces on that tap.
  await page.getByTestId('halo').click()
  await expect(page.getByTestId('mic-status')).toHaveText('Mic blocked')
  // The halo is the calendar's only voice door, and it cannot be honoured.
  await expect(page.getByTestId('halo')).toBeDisabled()

  await page.getByTestId('capture-text').fill(FAKE_TRANSCRIPT)
  await page.getByTestId('send-text').click()
  await page.getByTestId('inbox-toggle').click()
  await expect(page.getByTestId('inbox-item')).toHaveCount(1)
})
