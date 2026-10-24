# blurt

Self-hosted, voice-first reminders. Say it; Blurt turns it into a Reminder, a Memory, or both. Design lives in [docs/PRD.MD](docs/PRD.MD).

## Developer

Prereqs: Docker (compose v2) and Go 1.26+ for `env-init`. Node 24+ only if working on the frontend.

### First run

```sh
make env-init
```

Copies `.env.example` to `.env` and generates the session secret, encryption key, and VAPID key pair. With `BLURT_PUBLIC_ORIGINS` empty, the localhost dev origins are allowed. Once you set a public origin, only the listed origins are trusted, so add localhost explicitly if you use both.

Then fill the remaining values in `.env`:

`BLURT_SMTP_*` is optional in the environment but required by the production server: with no mailbox configured it refuses to boot. There is nothing else to set up for mail — no TLS or certificate knobs, because Go verifies Gmail against the OS trust store. For a local stack with no mailbox, set `BLURT_FAKE_PIN=true` and sign in with the fixed PIN `123456`. Never set that on a public host.

| Variable | What to get |
| --- | --- |
| `BLURT_SMTP_PASSWORD` | Gmail App Password for `leeyihong03@gmail.com` (Google Account → Security → 2-Step Verification → App passwords). |
| `BLURT_PUBLIC_ORIGINS` | Public https origin, e.g. `https://blurt.bytesbylyh.dev`. Optional locally. |

For the public host, point `blurt.bytesbylyh.dev` at the nginx port (`127.0.0.1:9998`) — a Cloudflare Tunnel or any reverse proxy works. `BLURT_WEB_PORT` overrides the port.

### Run

```sh
make start-no-reset     # start, keep existing data
make start-reset-full   # wipe the SQLite volume, start clean
make stop
make logs
```

App: <http://localhost:9998>. The Go server has no host port; nginx proxies `/api` and `/health`.

### Test

```sh
cd server && go test ./...
make e2e                # docker stack on :9977 + real browser
```

Frontend dev server (proxies to the docker stack):

```sh
cd web && npm install && npm run dev
```

Questionnaire questions live in `web/src/data/questionnaire/v1.json` (versioned file). `/home` redirects to `/questionnaire` until the user's stored version reaches that file's version.

### Current gaps

Login PINs are hashed in an in-memory map (5 minute TTL, single use, 5 attempts), so a restart invalidates outstanding codes. Admin user creation is not built yet: the User row is created on the first verified PIN. Job capture and the reminder engine are not wired yet, so `POST /api/push` is the only way to fire a notification today; when reminders land they set `notificationTraceId` from the attempt's Trace row. The app has no push controls yet either — `/home` shows the setup state and retries the subscription handoff.
