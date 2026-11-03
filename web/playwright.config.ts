import { defineConfig } from '@playwright/test'
import { BASE_URL, COMPOSE_PROJECT, ciTimeout } from './e2e/env'

// The suite boots the production stack in docker (nginx + Go + SQLite) via the
// compose files, then drives a real browser against it. The stack runs with
// BLURT_FAKE_PIN, so login needs no mailbox. Push is exercised for real: the Go
// sender posts to a relay the test runs on the host (see e2e/push.spec.ts), and
// the service-worker spec delivers a payload straight into the worker.
export default defineConfig({
  testDir: './e2e',
  // Specs are independent — each signs in its own user — so files run side by
  // side instead of one at a time.
  fullyParallel: true,
  workers: process.env.CI ? 2 : 4,
  timeout: ciTimeout(60_000),
  outputDir: 'test-results',
  // One owner session for the whole run: see e2e/setup.ts.
  globalSetup: './e2e/setup.ts',
  // Home re-asks the server on a five-second beat, so an assertion that waits for
  // a row to appear has to outlast a beat: the default five seconds is exactly
  // one, which turns the app's own cadence into a flaky test.
  expect: { timeout: 15_000 },
  // The visual baselines are one set per platform, not one per project: the file
  // name says what the shot is. Without this the named projects below would add
  // "-e2e" to every snapshot path and orphan the checked-in shots.
  snapshotPathTemplate: '{testDir}/{testFilePath}-snapshots/{arg}{-snapshotSuffix}{ext}',
  globalTeardown: './e2e/teardown.ts',
  use: {
    baseURL: BASE_URL,
    // Local runs reuse installed Chrome; CI installs Playwright's chromium.
    channel: process.env.CI ? 'chromium' : 'chrome',
    trace: 'retain-on-failure',
    // Capture tests need a microphone. Chromium's fake device gives a stable
    // stream; per-test init scripts can still force a permission denial.
    permissions: ['microphone'],
    launchOptions: {
      args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'],
    },
  },
  projects: [
    // This spec restarts its own stack with a shifted clock (e2e/clock-stack.ts),
    // so it needs no ordering against the specs on the shared one.
    { name: 'clock', testMatch: /emitter-clock\.spec\.ts/ },
    {
      name: 'e2e',
      testIgnore: /emitter-clock\.spec\.ts|visual\.spec\.ts/,
    },
    // The pixel proofs read state the whole run shares — the owner's access
    // queue — so they run on their own, after every behaviour spec has finished
    // and the applications those specs made are decided.
    { name: 'visual', testMatch: /visual\.spec\.ts/, dependencies: ['e2e'] },
  ],
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
