// The clock spec restarts a server to move the emitter's clock, so it owns a
// stack of its own on its own port: a restart of the shared one would break
// every spec running beside it.
import { execFile } from 'node:child_process'
import { expect } from '@playwright/test'
import { CLOCK_PORT, CLOCK_PROJECT, CLOCK_URL, ciTimeout } from './env'

// The same two compose files playwright.config.ts boots the shared stack with,
// so the images, the throwaway settings and the fakes all match.
const COMPOSE = ['-f', '../docker-compose.yml', '-f', '../docker-compose.e2e.yml', '-p', CLOCK_PROJECT]

function compose(args: string[], clock?: string): Promise<void> {
  return new Promise((resolve, reject) => {
    execFile(
      'docker',
      ['compose', ...COMPOSE, ...args],
      {
        env: {
          ...process.env,
          BLURT_WEB_PORT: String(CLOCK_PORT),
          ...(clock ? { BLURT_FAKE_CLOCK_FAST_FORWARD_AMOUNT: clock } : {}),
        },
      },
      (err, _stdout, stderr) => {
        if (err) reject(new Error(`${err.message}\n${stderr}`))
        else resolve()
      },
    )
  })
}

async function waitForHealth(): Promise<void> {
  await expect
    .poll(
      async () => {
        try {
          return (await fetch(`${CLOCK_URL}/health`)).status
        } catch {
          return 0
        }
      },
      { timeout: ciTimeout(60_000) },
    )
    .toBe(200)
}

// start brings the stack up from scratch, so no row from an earlier run can
// answer a poll.
export async function startClockStack(): Promise<void> {
  await compose(['up', '-d', '--build', '--wait', '--remove-orphans'])
  await waitForHealth()
}

// recreate rebuilds the server and the proxy in place with the emitter's clock
// set to clock. The data volume is untouched, so the session, the subscription
// and the Reminder survive — which is the restart this spec is really about.
// The proxy comes with the server because nginx resolves its address when it
// loads its config.
export async function recreate(clock: string): Promise<void> {
  await compose(['up', '-d', '--force-recreate', 'server', 'web'], clock)
  await waitForHealth()
}

// stop takes the stack and its volume with it, so a run leaves nothing behind.
export async function stopClockStack(): Promise<void> {
  await compose(['down', '-v', '--remove-orphans']).catch(() => undefined)
}
