// Request helpers for specs that drive the stack over the API instead of the browser. The
// session, the Job, the Reminder and the push are all real; only the model and the relay
// are ours (see helpers.ts and relay.ts).
import { expect, type APIRequestContext } from '@playwright/test'
import { BASE_URL } from './env'
import { DEV_PIN } from './helpers'
import { subscription, type DeviceKeys } from './push-crypto'

// Unsafe requests need an allowed origin: the stack guards against CSRF.
const CREDENTIALS = { Origin: BASE_URL }

// The zone every capture is stamped with. It is the anchor the agent resolves a relative
// time against, so a capture without it only earns itself a question.
const TIMEZONE = 'Asia/Singapore'

// signInRequest runs the real login endpoints; the request context keeps the session
// cookie, so everything after this is the caller's own.
export async function signInRequest(request: APIRequestContext, email: string, name = 'Blurt'): Promise<void> {
  const sent = await request.post('/api/auth/request-pin', { data: { email }, headers: CREDENTIALS })
  expect(sent.ok()).toBeTruthy()
  const verified = await request.post('/api/auth/verify-pin', {
    data: { email, name, pin: DEV_PIN },
    headers: CREDENTIALS,
  })
  expect(verified.ok()).toBeTruthy()
}

// subscribeRequest registers one device, so the emitter has somewhere to send.
export function subscribeRequest(request: APIRequestContext, endpoint: string, keys: DeviceKeys) {
  return request.post('/api/push/subscribe', { data: subscription(keys, endpoint), headers: CREDENTIALS })
}

// createReminder captures a text blurt. It returns once the Job is durable; the agent turns
// it into a Reminder behind the response.
export function createReminder(request: APIRequestContext, text: string) {
  return request.post('/api/jobs', {
    data: { text },
    headers: { ...CREDENTIALS, 'X-Blurt-Timezone': TIMEZONE },
  })
}

export interface ReminderPayload {
  reminderId: number
  text: string
  status: string
  timezone: string
  localDateTime?: string
  emitAtUtc?: string
  rule?: { frequency: string; time: string; weekday?: number; day?: number; month?: number }
  nextEmitAtUtc?: string
}

export interface ReminderLists {
  upcoming: ReminderPayload[]
  past: ReminderPayload[]
}

// reminders reads the same API the Reminders page renders, so a spec can assert on what the
// page would show without driving a browser.
export async function reminders(request: APIRequestContext): Promise<ReminderLists> {
  const res = await request.get('/api/reminders')
  expect(res.ok()).toBeTruthy()
  return (await res.json()) as ReminderLists
}

// waitForReminder polls the list until the text lands on the side the caller expects, which
// is how a spec waits out the agent's own run without guessing at its duration.
export async function waitForReminder(
  request: APIRequestContext,
  text: string,
  side: 'upcoming' | 'past',
  timeoutMs: number,
): Promise<ReminderPayload> {
  let found: ReminderPayload | undefined
  await expect
    .poll(async () => {
      found = (await reminders(request))[side].find((reminder) => reminder.text === text)
      return Boolean(found)
    }, { timeout: timeoutMs })
    .toBe(true)
  return found as ReminderPayload
}