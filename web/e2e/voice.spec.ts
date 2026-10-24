import { expect, test } from '@playwright/test'
import { completeQuestionnaire, denyMicrophone, signIn, uniqueEmail } from './helpers'

// The e2e stack runs with BLURT_FAKE_STT=true (docker-compose.e2e.yml), so the
// transcript is deterministic while the rest of the chain stays real: browser
// capture, the WAV rewrite, upload, temp file, worker, Job Conversation.
const FAKE_TRANSCRIPT = 'Remind me to buy glue on the way home.'

test('records a clip, uploads it, and shows the transcript', async ({ page }) => {
  await signIn(page, { name: 'Lam', email: uniqueEmail('voice') })
  await completeQuestionnaire(page)

  // The mic opens on /home and the boom is already recording: the countdown is
  // the proof that a clip is running.
  await expect(page.getByTestId('mic-status')).toHaveText('Mic ready')
  await expect(page.getByTestId('boom-countdown')).toBeVisible()
  // Long enough for the recorder to emit audio packets; the client refuses
  // anything under 300 ms anyway.
  await page.waitForTimeout(800)
  await page.getByTestId('send-audio').click()

  await expect(page.getByTestId('job-message').first()).toHaveText(FAKE_TRANSCRIPT)
  await expect(page.getByTestId('capture-status')).toHaveCount(0)
})

// Cancel is the way out of a clip the user does not want, and it puts them
// straight into the composer rather than back at a menu.
test('cancelling a capture lands in the composer', async ({ page }) => {
  await signIn(page, { name: 'Lam', email: uniqueEmail('cancel') })
  await completeQuestionnaire(page)

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

  await expect(page.getByTestId('mic-status')).toHaveText('Mic blocked')
  // Nothing can be recorded, so the boom waits for a tap it cannot honour.
  await expect(page.getByTestId('boom-record')).toBeDisabled()

  await page.getByTestId('capture-text').fill(FAKE_TRANSCRIPT)
  await page.getByTestId('send-text').click()
  await expect(page.getByTestId('job-message').first()).toHaveText(FAKE_TRANSCRIPT)
})
