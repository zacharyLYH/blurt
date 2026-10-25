import { expect, test, type Page } from '@playwright/test'
import { completeQuestionnaire, signIn, uniqueEmail } from './helpers'

// The fake stack's transcript, which is what the clip turn shows once the worker
// has read it (docker-compose.e2e.yml runs BLURT_FAKE_STT).
const FAKE_TRANSCRIPT = 'Remind me to buy glue on the way home.'

// A question the fake agent asks about: a capture with no time words of its own
// is the branch it cannot pin.
async function askAQuestion(page: Page, email: string) {
  await signIn(page, { name: 'Lam', email })
  await completeQuestionnaire(page)
  await page.getByTestId('capture-text').fill('buy glue when I am home')
  await page.getByTestId('send-text').click()
  await expect(page.getByTestId('pull-up-mic')).toBeVisible()
}

// The gesture: pulling the cue up opens the boom on its own screen, the clip is
// sent, and the transcript lands back in the same conversation. The drag is a
// pointer gesture, so the mouse drives it like any finger would.
test('pulls the mic up to answer out loud', async ({ page }) => {
  await askAQuestion(page, uniqueEmail('voice-reply'))

  const cue = page.getByTestId('pull-up-mic')
  const box = await cue.boundingBox()
  if (!box) throw new Error('the cue has no box to drag')

  const centre = box.x + box.width / 2
  await page.mouse.move(centre, box.y + box.height / 2)
  await page.mouse.down()
  // Drag past the threshold — a third of the viewport — by going to the top.
  await page.mouse.move(centre, 5, { steps: 12 })
  await page.mouse.up()

  await expect(page.getByTestId('boom-countdown')).toBeVisible()
  await page.waitForTimeout(800)
  await page.getByTestId('send-audio').click()

  // Back on the thread, and the clip is its own turn with its transcript.
  await expect(page.getByTestId('job-thread')).toBeVisible()
  await expect(page.getByTestId('job-message').filter({ hasText: FAKE_TRANSCRIPT })).toBeVisible()
})

// The tap path has to keep working everywhere: a desktop, a keyboard and a
// screen reader have no swipe to make, and this is the voice path.
test('opens the boom with a tap', async ({ page }) => {
  await askAQuestion(page, uniqueEmail('voice-reply-tap'))

  await page.getByTestId('pull-up-mic').click()
  await expect(page.getByTestId('boom-countdown')).toBeVisible()

  // Cancelling belongs to the boom, and leaves the user back at the thread.
  await page.getByTestId('cancel-audio').click()
  await expect(page.getByTestId('job-thread')).toBeVisible()
})