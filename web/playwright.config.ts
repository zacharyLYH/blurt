import { defineConfig } from '@playwright/test'
import { BASE_URL, COMPOSE_PROJECT, ciTimeout } from './e2e/env'

// The suite boots the production stack in docker (nginx + Go + SQLite) via the
// compose files, then drives a real browser against it. The stack runs with
// BLURT_FAKE_PIN, so login needs no mailbox. Push is exercised for real: the Go
// sender posts to a relay the test runs on the host (see e2e/push.spec.ts), and
// the service-worker spec delivers a payload straight into the worker.
export default defineConfig({
  testDir: './e2e',
  workers: 1,
  timeout: ciTimeout(60_000),
  outputDir: 'test-results',
  globalTeardown: './e2e/teardown.ts',
  use: {
    baseURL: BASE_URL,
    // Local runs reuse installed Chrome; CI installs Playwright's chromium.
    channel: process.env.CI ? 'chromium' : 'chrome',
    trace: 'retain-on-failure',
  },
  webServer: {
    command:
      `docker compose -f ../docker-compose.yml -f ../docker-compose.e2e.yml ` +
      `-p ${COMPOSE_PROJECT} up --build --remove-orphans`,
    url: `${BASE_URL}/health`,
    // Drives the compose port interpolation; compose appends port lists on
    // merge, so the e2e file must not add a second mapping.
    env: { BLURT_WEB_PORT: String(new URL(BASE_URL).port) },
    reuseExistingServer: false,
    timeout: ciTimeout(300_000),
    // Cold image builds are noisy; failures still surface on the expect.
    stdout: 'ignore',
    stderr: 'pipe',
  },
})
