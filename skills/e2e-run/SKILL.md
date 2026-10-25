---
name: e2e-run
description: Run Blurt's Playwright suite from a non-interactive shell, where the run gets killed or the docker stack is already up. Use when `npm run test:e2e` reports nothing, dies at "Running N tests", or when you already have a warm stack and do not want the suite to boot it again.
---

# Running the browser suite from a non-interactive shell

`make e2e` is `cd web && npm run test:e2e`. Playwright's `webServer` boots the
production stack in docker (`docker compose ... -p blurt-e2e up --build`) and waits
on `/health`, which takes a minute or two before the first test runs.

## The trap: a backgrounded run gets reaped

Launching the suite with `&` and then letting the parent shell time out **kills it
mid-run**. The symptom is a log that stops dead at `Running N tests using 1 worker`
with no results, while `ps` shows no playwright process — the docker stack it booted
stays up, so the failure looks like a hang instead of a kill.

Daemonize it properly: a double fork into its own session survives the parent dying.

```sh
cat > /tmp/daemon.py <<'PY'
import os, sys
if os.fork() > 0: sys.exit(0)
os.setsid()
if os.fork() > 0: sys.exit(0)
os.chdir('/path/to/blurt/web')
fd = os.open('/tmp/e2e.log', os.O_WRONLY | os.O_CREAT | os.O_TRUNC)
os.dup2(fd, 1); os.dup2(fd, 2)
os.execvp('npx', ['npx', 'playwright', 'test'])
PY
python3 /tmp/daemon.py          # returns immediately
```

Then poll `tr '\r' '\n' < /tmp/e2e.log | tail` — Playwright updates its progress line
in place, so a plain `tail` shows the same stale frame twice.

## The shortcut: reuse a warm stack

The suite always re-runs the `webServer` command (`reuseExistingServer: false`), so
a second run rebuilds what is already up. If `docker ps` shows `blurt-e2e-web-1` and
`blurt-e2e-server-1` healthy, drop the webServer with a throwaway config beside the
real one — the config must sit in `web/`, because it imports `./e2e/env`:

```ts
// web/playwright.local.config.ts
import base from './playwright.config'
export default { ...base, webServer: undefined }
```

```sh
npx playwright test --config=playwright.local.config.ts e2e/agent.spec.ts
```

**Delete that file when done.** Anything in the repo gets auto-staged here, so a
leftover stub shows up as `AD web/playwright.local.config.ts`; `git restore --staged
<path>` unstages it.

## Notes

- The base suite ends with `globalTeardown` (`docker compose -p blurt-e2e down -v`),
  so it wipes the throwaway volume itself. Check `docker ps -a | grep blurt` is empty
  afterwards.
- The run needs a local Chrome: `channel: 'chrome'` unless `CI` is set. A stale
  `CI=1` in the environment switches it to chromium and fails if that is not
  installed.
- A fresh volume here is the cheapest way to prove a schema change boots: the stack
  applies `schema.sql` on startup and every flow breaks loudly if the change is wrong.