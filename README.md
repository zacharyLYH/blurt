# blurt

Self-hosted, voice-first reminders. Say it; Blurt turns it into a Reminder, a Memory, or both. Design lives in [docs/PRD.MD](docs/PRD.MD).

## Developer

Prereqs: Docker (compose v2) and Go 1.26+ for `env-init`. Redis runs in the compose stack; no host install is needed. Node 24+ only if working on the frontend.

### First run

```sh
make env-init
```

Copies `.env.example` to `.env` and generates the session secret, encryption key, and VAPID key pair. With `BLURT_PUBLIC_ORIGINS` empty, the localhost dev origins are allowed. Once you set a public origin, only the listed origins are trusted, so add localhost explicitly if you use both.

Then fill the remaining values in `.env`:

`BLURT_SMTP_*` is optional in the environment but required by the production server: with no mailbox configured it refuses to boot. There is nothing else to set up for mail — no TLS or certificate knobs, because Go verifies Gmail against the OS trust store. For a local stack with no mailbox, set `BLURT_FAKE_PIN=true` and sign in with the fixed PIN `123456`. Never set that on a public host.

`BLURT_STT_URL` must point at the phonon gateway's transcription route. That gateway is self-hosted and **not** OpenAI-compatible: it takes one JSON body carrying a base64 WAV data URI and answers `{"output": {"text": "..."}}`, and it rejects any model id it does not serve, so `BLURT_STT_MODEL` must be `phonon`. `BLURT_STT_API_KEY` is the gateway's bearer key and is required — a missing or wrong key is refused with a 401 that Blurt treats as permanent. `BLURT_FAKE_STT=true` is the local/e2e escape hatch: it swaps the gateway for a deterministic transcriber, so a local stack needs no model and none of the `BLURT_STT_*` values. Never set it on a public host. The fake's failure modes (`error`, `illegible`, `flaky`) are exercised by the worker's unit tests, not by an environment variable.

`BLURT_DECISION_*` points at the same gateway's `laya` model, which answers typed decisions and powers the transcript-quality gate: a transcript the gate labels noise takes the same fallback as an illegible clip. The URL and key default to the `BLURT_STT_*` ones, so only `BLURT_DECISION_MODEL` ever needs setting; with no URL the gate is skipped, which is how the `BLURT_FAKE_STT` stack runs. The full contract and its sharp edges live in the [laya-decisions skill](skills/laya-decisions/SKILL.md).

`BLURT_AGENT_*` points at the same gateway's chat route, which turns a capture into Reminders and Memories. Only `BLURT_AGENT_MODEL` is required, because the gateway serves many chat models and refuses an id it does not serve; `BLURT_AGENT_MAX_TURNS` sets the per-Job turn budget (default 5). `BLURT_FAKE_AGENT=true` swaps the model for a scripted one so the AgentLoop runs with no model at all, which is how the e2e stack runs. The route is not OpenAI tool calling — chat messages plus a JSON schema in, the answer as a JSON string out — and a constrained decoder fills every declared field rather than leaving it empty, so the schema stays small and the server does the resolving. The contract and those traps live in the [gateway-records skill](skills/gateway-records/SKILL.md).

| Variable | What to get |
| --- | --- |
| `BLURT_SMTP_PASSWORD` | Gmail App Password for `leeyihong03@gmail.com` (Google Account → Security → 2-Step Verification → App passwords). |
| `BLURT_PUBLIC_ORIGINS` | Public https origin, e.g. `https://blurt.bytesbylyh.dev`. Optional locally. |

Queues run on Redis Streams (`BLURT_REDIS_URL`, default `redis://redis:6379/0` in compose). Redis owns routing and consumer ownership; SQLite remains the durable Job record, and a background claimer/sweeper recovers crashes without a boot scan.

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

Login PINs are hashed in an in-memory map (5 minute TTL, single use, 5 attempts), so a restart invalidates outstanding codes. Admin user creation is not built yet: the User row is created on the first verified PIN. Every app entry runs a permission gate ahead of the questionnaire, asking for the microphone and for push; a returning user with both already allowed passes straight through. Capture is wired through transcription: `/home` opens the mic and starts recording on its own, pins the capture graph to 16 kHz mono, rewrites the browser's encode into mono 16 kHz PCM WAV — the only shape the gateway accepts — and sends one blob on the round send button; the X discards the clip and drops the caret in the composer. The worker stores the transcript in `Job.Conversation`. Home's sidebar lists every Job the agent has not closed yet, so a capture stays reachable after the user moves on. The AgentLoop then picks that Job up: one model call turns the capture into Reminders and Memories, the server — not the model — decides what is committable, and the Job closes, or asks, in the same thread. The model returns the user's own words for *when* (`"tomorrow at 6pm"`) and the server resolves them, because a small model under a constrained schema will fill a date and time field with an invented one rather than leave it empty. A Job whose time cannot be pinned sets `clarifying`, pushes, and resumes on a typed answer at `POST /api/jobs/{jobId}/messages`. A capture's device zone rides `X-Blurt-Timezone` and is what a relative time is resolved against. `/api/jobs` caps a clip at one minute and 2 MiB (the boom counts down and sends itself a beat early), keeps the blob only until the worker finishes, and retries STT three times before appending “I couldn't process that. Please say it again or type it.” and firing a push — the same fallback a noise verdict from the gate triggers immediately, since re-running the same clip cannot change it. A failed-transcription push opens that Job's own page, `/jobs/{jobId}`. Still missing: the emitter that fires a resolved Reminder, Correction (`ReplaceReminder`, `CancelReminder`, `DeleteMemory`), recurrence, the per-user model setup page, and the semantic half of PostAgentCheck.
