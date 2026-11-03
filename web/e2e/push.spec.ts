import { createECDH, randomBytes } from 'node:crypto'

import { expect, test, type APIRequestContext } from '@playwright/test'

import { BASE_URL, ciTimeout } from './env'
import { DEV_PIN, createAccount, uniqueEmail } from './helpers'
import { withRelay } from './relay'

// Real backend, real store, real sender. Only the relay is ours: the Go container POSTs to
// a server on the host through host.docker.internal, exactly as it would to FCM or Apple.
// That makes the encrypted body, the VAPID signature, and the pruning behaviour observable.
//
// The endpoint must be plain http for the host relay, which the subscription guard allows
// for localhost and host.docker.internal only.

// clientKeys mints the device half of a subscription exactly as a browser
// would, so the server's own key checks run against a real P-256 point.
function clientKeys() {
  const ecdh = createECDH('prime256v1')
  ecdh.generateKeys()
  return {
    p256dh: ecdh.getPublicKey().toString('base64url'),
    auth: randomBytes(16).toString('base64url'),
  }
}

// signIn runs the real login endpoints; the request context keeps the session
// cookie, so the push endpoints can be driven without a browser. The account is
// created and approved first: signing in never creates one.
async function signIn(request: APIRequestContext, email: string) {
  await createAccount('Push', email)
  const headers = { Origin: BASE_URL }
  const sent = await request.post('/api/auth/request-pin', { data: { email }, headers })
  expect(sent.ok()).toBeTruthy()
  const verified = await request.post('/api/auth/verify-pin', {
    data: { email, pin: DEV_PIN },
    headers,
  })
  expect(verified.ok()).toBeTruthy()
}

async function subscribe(request: APIRequestContext, endpoint: string, keys = clientKeys()) {
  return request.post('/api/push/subscribe', {
    data: { endpoint, keys },
    headers: { Origin: BASE_URL },
  })
}

async function send(request: APIRequestContext, data: Record<string, unknown>) {
  return request.post('/api/push', { data, headers: { Origin: BASE_URL } })
}

test('the real sender encrypts and signs a push for a subscribed device', async ({ request }) => {
  test.setTimeout(ciTimeout(60_000))
  await withRelay(201, async (endpoint, captured) => {
    await signIn(request, uniqueEmail('push'))
    expect((await subscribe(request, endpoint)).status()).toBe(204)
    // Subscribing answers the device with one confirmation of its own; the
    // reminder below is the second thing the relay ever sees.
    await expect.poll(() => captured.length, { timeout: ciTimeout(20_000) }).toBe(1)

    const sent = await send(request, {
      title: 'Buy glue',
      body: 'On the way home. 6:00 PM SGT',
      tag: 'reminder-42',
      navigate: `${BASE_URL}/home`,
    })
    expect(sent.status()).toBe(200)
    expect(await sent.json()).toEqual({ sent: 1, failed: 0, pruned: 0 })

    await expect.poll(() => captured.length, { timeout: ciTimeout(20_000) }).toBe(2)
    const push = captured[1]
    // The relay only ever sees ciphertext: a VAPID credential, an aes128gcm
    // record, and a body far larger than the plaintext.
    expect(String(push.headers.authorization)).toMatch(/^vapid /)
    expect(push.headers['content-encoding']).toBe('aes128gcm')
    expect(push.body.length).toBeGreaterThan(100)
  })
})

// Turning notifications on has to prove itself: the device that just subscribed
// is sent one notification, encrypted like any other, without the app asking.
// The handoff repeats on every page load, so the same subscription must not
// become a notification every page load.
test('a new subscription is confirmed once, and only for a new device', async ({ request }) => {
  test.setTimeout(ciTimeout(60_000))
  await withRelay(201, async (endpoint, captured) => {
    await signIn(request, uniqueEmail('confirm'))
    const keys = clientKeys()

    expect((await subscribe(request, endpoint, keys)).status()).toBe(204)
    await expect.poll(() => captured.length, { timeout: ciTimeout(20_000) }).toBe(1)
    expect(String(captured[0].headers.authorization)).toMatch(/^vapid /)
    expect(captured[0].headers['content-encoding']).toBe('aes128gcm')

    // The identical handoff again: stored, but nothing new to announce.
    expect((await subscribe(request, endpoint, keys)).status()).toBe(204)
    await new Promise((resolve) => setTimeout(resolve, 500))
    expect(captured).toHaveLength(1)

    // A rotated key pair is a new device as far as the user is concerned.
    expect((await subscribe(request, endpoint)).status()).toBe(204)
    await expect.poll(() => captured.length, { timeout: ciTimeout(20_000) }).toBe(2)
  })
})

test('a relay that reports the endpoint gone prunes it, never the session', async ({ request }) => {
  await withRelay(201, async (endpoint, captured, answer) => {
    await signIn(request, uniqueEmail('gone'))
    expect((await subscribe(request, endpoint)).status()).toBe(204)
    // The subscription is confirmed first, while the endpoint is still healthy.
    await expect.poll(() => captured.length, { timeout: ciTimeout(20_000) }).toBe(1)

    // Now the endpoint is gone, and the next send is the one that finds out.
    answer(410)
    const first = await send(request, { title: 't', body: 'b' })
    expect(await first.json()).toEqual({ sent: 0, failed: 0, pruned: 1 })

    // The dead endpoint is forgotten, so the second send has nobody to contact.
    const second = await send(request, { title: 't', body: 'b' })
    expect(await second.json()).toEqual({ sent: 0, failed: 0, pruned: 0 })
    expect(captured).toHaveLength(2)
    // Cleaning up push must not sign the user out.
    expect((await request.get('/api/user')).status()).toBe(200)
  })
})

test('a relay that refuses the VAPID identity is reported, not blamed on the device', async ({ request }) => {
  await withRelay(403, async (endpoint) => {
    await signIn(request, uniqueEmail('vapid'))
    expect((await subscribe(request, endpoint)).status()).toBe(204)

    const refused = await send(request, { title: 't', body: 'b' })
    expect(refused.status()).toBe(502)
    const body = await refused.json()
    expect(body.code).toBe('vapid_rejected')
    expect(body.failed).toBe(1)

    // Still subscribed: the identity is the problem, and a retry still tries.
    const retried = await send(request, { title: 't', body: 'b' })
    expect((await retried.json()).failed).toBe(1)
  })
})

test('rejects an untrusted endpoint, a foreign target, and an oversized payload', async ({ request }) => {
  await signIn(request, uniqueEmail('reject'))

  // The endpoint becomes a POST target, so anything but a push service is out.
  expect((await subscribe(request, 'https://evil.example/push')).status()).toBe(400)
  expect((await subscribe(request, 'http://evil.example/push')).status()).toBe(400)

  // A notification cannot open anything but Blurt, and must carry content.
  expect((await send(request, { title: 't', body: 'b', navigate: 'https://evil.example/home' })).status()).toBe(400)
  expect((await send(request, { title: '  ', body: 'b' })).status()).toBe(400)
  expect((await send(request, { title: 't', body: 'x'.repeat(501) })).status()).toBe(400)
})
