---
name: e2e-parallel-auth
description: Give every Playwright worker a session without racing the single-use login PIN. Use when a spec needs the app owner's session, when a helper signs in a fixed address, or when verify-pin returns 401 only when the suite runs in parallel.
---

# Parallel workers and one PIN

`auth.PinStore` keeps **one code per address** and it is single use. Two workers
that both call `request-pin` for the same address invalidate each other: the
first worker's `verify-pin` then answers `401 invalid_pin`. The symptom is a
suite that is green serially and red at 4 workers, always on the helper that
signs in a shared account (the owner).

## Sign the shared account in once

Do it in `globalSetup`, after the web server is up, and hand the cookie to every
worker as a storage state:

```ts
// e2e/setup.ts
const api = await request.newContext({ baseURL: BASE_URL, extraHTTPHeaders: { Origin: BASE_URL } })
await api.post('/api/auth/request-pin', { data: { email: OWNER } })
await api.post('/api/auth/verify-pin', { data: { email: OWNER, pin: DEV_PIN } })
await api.storageState({ path: OWNER_STATE })
```

Then reuse it, never re-login:

```ts
request.newContext({ baseURL: BASE_URL, storageState: OWNER_STATE, ... })  // API calls
browser.newContext({ storageState: OWNER_STATE })                          // a page
test.use({ storageState: OWNER_STATE })                                    // a whole spec
```

`outputDir` is cleaned before `globalSetup` runs, so a path under `test-results/`
survives the run. Do not put it in `test-results/<test>/` — Playwright rewrites
those per test.

## Accounts still go through the door

Signing in never creates an account (`verify-pin` only reads one), so a helper
that needs a fresh user must apply and be approved first — `createAccount()` in
`e2e/helpers.ts` does exactly that with the owner's storage state. That keeps the
app's own gate under test instead of writing a row behind the API's back.

## Shared pages need shared-state care

`/admin` is global state, so a screenshot of it is only stable if the pending
queue is. Wait for the queue to be exactly the rows the test owns
(`expect.poll(...).toEqual([...])`) instead of rejecting other rows — another
worker's application is approved milliseconds after it is made, and deleting it
out from under that worker breaks its test.
