# blurt

Self-hosted, voice-first reminders. One capture becomes a Reminder, a Memory, or both.
Specs: [docs/PRD.MD](docs/PRD.MD) · [docs/specs/AGENT-LOOP.MD](docs/specs/AGENT-LOOP.MD).

## Run

Prereqs: Docker (compose v2), Go 1.26+ (`env-init`), Node 24+ (frontend only). Redis runs in the stack.

```sh
make env-init          # .env.example -> .env, plus generated session secret, encryption key, VAPID pair
# fill BLURT_SMTP_PASSWORD, BLURT_STT_{URL,API_KEY}, BLURT_{PRESCREEN,CHECK,TAGS}_MODEL
make start-reset-full  # or: start-no-reset | stop | logs
```

App: <http://localhost:9998>. nginx proxies `/api` and `/health`; the Go server has no host port.

## Configure

Boot panics on any gap. Every `BLURT_FAKE_*` switch is local/e2e only: never set one on a public host.

| Var | What |
| --- | --- |
| `BLURT_STT_URL` `_API_KEY` `_MODEL` | phonon transcription route. Not OpenAI-shaped: base64 WAV data URI in, `{"output":{"text":"…"}}` out. `_MODEL` must be `phonon`; a wrong key gets a 401 that Blurt treats as permanent. |
| `BLURT_PRESCREEN_MODEL` `BLURT_TAGS_MODEL` | the two cheap stages: prescreen and memory tagger. Same gateway as STT, so URL and key default to `BLURT_STT_*`; `BLURT_AUX_URL` / `BLURT_AUX_API_KEY` override. |
| `BLURT_SMTP_*` | login-PIN mail. Gmail App Password (2FA required); nothing else to configure. |
| `BLURT_PUBLIC_ORIGINS` | comma-separated CSRF allowlist. Empty = localhost:9998 + :5173 (dev only). Set it and localhost is no longer trusted unless listed. |
| `BLURT_REDIS_URL` | queue transport. Default `redis://redis:6379/0`. |
| `BLURT_FAKE_PIN` | fixed PIN `123456`, no mail. |
| `BLURT_FAKE_STT` | deterministic transcriber, no gateway. |
| `BLURT_FAKE_AGENT` | scripted model, no provider (how e2e runs). |
| `BLURT_FAKE_CLOCK` | emitter clock offset, e.g. `90s` (e2e clock spec). |

The core planner is per user, not `.env`: the setup page stores provider, endpoint, key (encrypted at rest) and model on the User row. Gateway contract and traps: [skills/gateway-records](skills/gateway-records/SKILL.md). Hosted provider (`strict` + `require_parameters`): [skills/openrouter-model](skills/openrouter-model/SKILL.md).

## Test

```sh
cd server && go test ./...   # unit + integration, no network
make test-redis              # queue + flow against a throwaway Redis (docker)
make e2e                     # docker stack on :9977 + real browser (needs: cd web && npm install)
cd web && npm run lint       # must be clean
```

Frontend dev server, proxying the docker stack: `cd web && npm install && npm run dev`.
Questionnaire questions live in `web/src/data/questionnaire/v1.json` (versioned file).
SQLite holds the durable Job record; Redis Streams owns routing and crash recovery.

## Eval (L1, L2)

Scores the shipped prompts against real models. One case per file, in `server/internal/agent/L1/cases/`; the case's `layer` picks which test runs it. Settings come from `.env` (the process environment wins).

**L1** is the agent loop, on the user's own hosted model — a key and a model id are the only things to supply:

```sh
BLURT_EVAL_KEY=...                                              # required
BLURT_EVAL_MODEL=openai/gpt-6-luna                              # required
BLURT_EVAL_URL=https://openrouter.ai/api/v1/chat/completions    # optional, this is the default

cd server
go test ./internal/agent/L1 -run TestPlannerCases -eval -v     # every L1 case
```

**L2** is the two helpers — prescreen and the tagger — one call each on the deployment's own gateway, so it reads the same settings the server boots on (`BLURT_STT_URL`/`_API_KEY`, falling back from `BLURT_AUX_*`, plus one model id per stage). Nothing queues and no database is seeded, so the whole layer runs in seconds:

```sh
go test ./internal/agent/L1 -run TestHelperCases -eval -layer L2 -repeat 10 -v   # pass rates, not bits
```

| Flag | Does |
| --- | --- |
| `-eval` | opt in. Nothing else in `go test ./...` needs a provider |
| `-case <name>` `-layer L1\|L2` | run one case, or one layer |
| `-repeat N` | N runs per case, reported as a pass rate |
| `-audit` | print each turn and write no report |
| `-update` | (re)write the goldens |
| `-wire` | record every provider request and answer |
| `-prompt <name>` | prompt arm: `full` (shipped) or files in `cases/_prompts/`. L1 reads `<name>.txt`; L2 reads `<name>-<stage>.txt`, one file per helper |

Output: a per-case scorecard on stdout (pass rate, calls, faults, latency — shown with `-v`, and on any failure), `server/eval-out/<layer>-<model>-<arm>.json`, and a per-case transcript beside the report.
`L1/cases/_golden/*.txt` is the rendered context block; it runs offline inside `go test ./...`, so a prompt change shows up as a diff.

## Deploy

Point the host at nginx `127.0.0.1:9998` (`BLURT_WEB_PORT` overrides). Cloudflare Tunnel or any reverse proxy works.
