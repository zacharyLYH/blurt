// The owner is a single account, and a PIN is single use: four workers asking
// for one at the same time would each invalidate the others. So the suite signs
// the owner in once, before anything else runs, and every worker reuses the
// cookie it leaves behind.
import { request } from '@playwright/test'
import { BASE_URL } from './env'
import { DEV_PIN, OWNER, OWNER_STATE } from './helpers'

export default async function setup(): Promise<void> {
  const api = await request.newContext({ baseURL: BASE_URL, extraHTTPHeaders: { Origin: BASE_URL } })
  try {
    await api.post('/api/auth/request-pin', { data: { email: OWNER } })
    const signedIn = await api.post('/api/auth/verify-pin', { data: { email: OWNER, pin: DEV_PIN } })
    if (!signedIn.ok()) {
      throw new Error(`owner sign-in failed: ${signedIn.status()} ${await signedIn.text()}`)
    }
    await api.storageState({ path: OWNER_STATE })
  } finally {
    await api.dispose()
  }
}