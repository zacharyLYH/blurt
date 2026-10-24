// Shared e2e constants. The app runs in docker on a dedicated port so a
// developer's dev stack on 9998 never collides with a test run. These are
// fixed on purpose: docker-compose.e2e.yml hardcodes the same values, and
// drift there would silently break CSRF or PIN retrieval.
export const COMPOSE_PROJECT = 'blurt-e2e'
export const WEB_PORT = 9977
export const BASE_URL = `http://localhost:${WEB_PORT}`

// CI runners (shared GitHub hosts) are slower than a dev Mac, so every timeout
// budget is scaled there and stays tight locally, where failures should
// surface fast. Wrap test, webServer, and explicit wait budgets with it.
const CI_MULTIPLIER = process.env.CI ? 2 : 1

export function ciTimeout(ms: number): number {
  return ms * CI_MULTIPLIER
}

