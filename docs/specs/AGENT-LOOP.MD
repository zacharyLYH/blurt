# Agent loop

Status: built. Code is `internal/agent`: `loop.go` (the moves), `tools.go` (tools and what each
answers), `lint.go` (the checks), `prompt.go` (prompts, schemas, wire→record), `model.go` (the
transport). Evals: L1/L2/L3 under `eval/`.

Server authority never moves. `MissingFields`, `resolveDay`, `checkAfterNow`, the zone load and
the whitelist decide what is committable; the model decides what the capture means and what to
look up.

## 1. The moves

```
prescreen
   │
   ▼
agent loop (≤10 calls): model ⇄ tools
   │ submit_plan
   ▼
finalize (1 strict call)
   │
   ▼
lint ─┬─ records ──▶ settle ──▶ commit
      ├─ question ─▶ clarification
      ├─ fault 1 ──▶ agent loop (fresh budget, faults in hand)
      └─ fault 2 ──▶ error + notify
```

| Move | What it is |
| --- | --- |
| `prescreen` | the gate, one cheap call, its own turn |
| `agent` | the tool loop, ended by `submit_plan` |
| `finalize` | one strict-schema call over the same conversation |
| `lint` | the server's deterministic checks on that answer |
| `settle` | the delta is filled from the row, and the day token becomes a date |
| `commit` | inserts, in-place rewrites and the reply, in one transaction |

Two calls cannot merge: a forced schema can never ask to look something up, and a tool turn
carries no schema. So the tools turn is free-form and the answer is read through
`response_format`, which is also what guarantees the answer's shape.

## 2. Budget

10 model calls per loop plus 1 finalize. Tool calls are free inside those. Every tool result
carries the calls left, and the prompt states the cap.

`submit_plan` ends the loop. Spending the budget without it is a synthetic lint fault. A fault
restarts the loop with the full conversation, the faults and a fresh budget; **one retry
maximum**, then it is an error plus a notification.

## 3. Lint

One check is one function; a new rule is a new entry in `checks`, nothing else:

```go
type check func(Record, time.Time) []string // nil when the record passes
```

Every check runs, every fault is collected, and the model reads one block:

```
lint: time "18:00," is not HH:MM
lint: zone "Singapore" is not IANA
```

Two tests sit above the field checks, because they are about the answer:

- **The whitelist.** An op may name only an id this run's own `find_reminders` printed. The run
  keeps those rows, so the check is about the conversation, not the table. A guessed `42` is
  somebody's real reminder, so this runs before any field.
- **The shape.** One kind, the verb, and the row each op names, refused where the JSON is read
  (`answerFrom`) and delivered as a fault like any other.

A `question` passes with no field checks — asking is the point. A `cancel` passes with no record:
the app owns that write (§6). A bad finalize answer, bad submit arguments and a missing
`submit_plan` are all lint faults and follow the same retry rule.

## 4. Tools

Three read-only answers, no writes, plus the one that ends the loop. Every read is scoped by the
Job's `UserId`.

| Tool | Returns |
| --- | --- |
| `find_reminders(query)` | ≤5 rows: id, text, the day it falls on with its clock, zone, state |
| `find_memories(query)` | ≤5 rows: text, tags |
| `check_update(reminder_id, text, zone, day, time, recurrence)` | whether that row can still be rewritten, and whether the place passes lint |
| `submit_plan()` | ends the loop, triggers the strict call |

- The id is the row's own `ReminderId`; no minted handle. It is printed only in tool results —
  the context block carries memories alone, so a row can never be mistaken for this capture's.
- A row prints `pending` while it can still change, and `fired` or `cancelled` once it cannot.
  The stored word for pending is `resolved`, which reads as "done", so the tool says the one
  thing the model has to decide.
- A failed lookup, a refused pre-check and a bad argument are all answers the model reads, never
  a failed turn. Empty results are legal.
- Cancelling has no tool: the app owns that write (§6).

## 5. Context

Built once per turn, reused by a lint retry. Only what cannot be looked up: at most 10 memories.

```
current_tz: Asia/Singapore
now_local: 2026-10-24T18:30
memories:
- I like this noodle shop in Chongqing [food, chongqing]
capture: buy glue at 6pm tomorrow
```

Reminders are absent on purpose: what a capture needs of them is a lookup.

## 6. The answer

An answer is `kind` (`records` or `question`) plus:

- `ops` — one entry per row the capture acts on, in the order it says them.
- `memories` — each durable fact in the user's own words.
- `evidence` — the user's words the day, clock and zone were read from (Trace only).
- `question` — one short ask, on a `question` answer and nowhere else.

| `intent` | `reminder_id` | record |
| --- | --- | --- |
| `create` | 0 | one, whole |
| `update` | ≠0, from a lookup | one, a delta |
| `cancel` | 0 | none |

**A change says only what changed.** "make it 8pm" writes a clock and nothing else. `settle`
fills every field the answer left empty from the row this run's lookup printed — words, zone,
day, clock, repeat — and `recordFrom` is the one wire mapping, so nothing is copied by the model
and no row is read twice. An empty `recurrence` means "the row's"; `none` means a one-off.

**The day token is the server's.** The model copies `tomorrow`, `in:13d`, `next:2:tuesday`, a
row's printed date, or a month and day. `settleRecord` turns it into a date once, after the zone
is known, so the calendar is read in the zone the row rings in.

`check_update` is the pre-check: it builds its record with `recordFrom` and fills it with
`settleRecord` — the commit's own path — then runs lint's own checks, so a place it accepts is a
place lint accepts. It is advisory; lint still decides at submit.

A rewrite is in place: same id, same history, `Status`, `CreatedAt` and `SourceJobId` untouched.
It rides the commit's transaction, so a turn writes all of it or none. A row that fired between
the lookup and the commit fails the turn (`ErrReminderStale`, `stale: true` on the Trace) rather
than being resurrected.

**A call-off writes nothing** and ends the Job `complete` with one fixed sentence:

> I think you asked to cancel a reminder? Blurt doesn't cancel reminders on your behalf, but you
> may go ahead and delete it on the app yourself!

Server-owned like every reply (`Reminder set.`, `Reminder updated.`, `Noted.`), because the schema
has no field for prose. It hedges because it names no row, and it points at
`POST /api/reminders/{id}/cancel` — the write that exists — rather than at an agent action.

## 7. After commit

The tagger runs behind the commit, on its own context: one call per committed Memory, 3–10
lowercase tags, deduped and capped at 10. It is best effort — a failure leaves the row untagged
rather than failing a capture the user already made.

## 8. Evals

Three layers, real models, never merged.

| Layer | Runs | Answers |
| --- | --- | --- |
| **L1** | the loop: seeded DB, real tools, real lint retry | did it read the capture right, which row, which field |
| **L2** | `Prescreen` and `Tags`: one call each | did this stage decide right |
| **L3** | `internal/backend` over HTTP: real DB, workers, text and recorded audio | does a capture become the right rows, end to end |

```
cd server
go test ./internal/agent/eval -run TestPlannerCases -eval -layer L1 -v -timeout 25m
go test ./internal/agent/eval -run TestHelperCases  -eval -layer L2 -v -timeout 15m
go test ./internal/agent/eval -run TestChainCases   -eval -layer L3 -v -timeout 25m
```

`-repeat N` is where L2 means something: a screen that refuses a good capture costs the user the
thing they said, and one run cannot tell that from a coin flip. `-audit` prints what each case saw
and writes no report; `-wire` records the provider traffic.

Settings come from the root `.env`: `BLURT_EVAL_*` for the planner arm, the gateway ids for
prescreen and tags, `BLURT_STT_*` for the one recorded step. Nothing is stubbed. One report per
run in `server/eval-out/`, so two models compare by diff.

## 9. Cases

One case, one file. Add is a file, delete is `rm`, audit is a file plus its golden.

```
server/internal/agent/eval/cases/<name>.json
server/internal/agent/eval/cases/_fixtures/<name>.json   # named large seeds
server/internal/agent/eval/cases/_golden/<name>.txt      # rendered context block
server/internal/agent/eval/cases/_audio/<name>.wav       # an L3 clip
```

```json
{
  "name": "one-off: buy glue at 6pm tomorrow",
  "layer": "L1",
  "size": "small",
  "now_local": "2026-10-24T18:30",
  "current_tz": "Asia/Singapore",
  "seed": {
    "reminders": [
      {"text": "Fetch kids from school", "local": "+1d 17:00", "zone": "Asia/Singapore"}
    ],
    "memories": [{"text": "I like this noodle shop in Chongqing"}]
  },
  "history": [{"role": "assistant", "text": "When should I remind you about buying glue?"}],
  "capture": "tomorrow at 6pm",
  "expect": {
    "kind": "records",
    "text_contains": ["buy glue"],
    "day": "tomorrow", "date": "2026-10-25",
    "time": "18:00", "zone": "Asia/Singapore",
    "recurrence": "none", "memory_empty": true
  }
}
```

- **`seed`** mirrors the store; times may be relative to `now_local` (`+1d 17:00`) so a case means
  the same thing next year.
- **`expect`** names only what the case is about. `updated` reads each rewritten row back by the
  ids the Trace names; `unchanged` pins the rows a turn must leave alone; `reply_contains` reads
  the assistant turn, which is where a call-off sentence lives.
- **L2** adds `stage` (`prescreen` or `tags`), the stage's input, and its truth under
  `expect.helper`. The runner refuses a case that names a stage it cannot feed.
- **L3** is `steps` in order — `capture`, `clip`, `reply` — each with its own `expect`. `kind` is
  `records`, `question` or `failed`; a Memory may pin `tags_min`, never the tag words.

Each case runs against a fresh database, seeded through the real stores, and the prompt is built
by the real `ContextBlock`. Row ids are stable, so goldens do not drift.

## 10. Out of scope

Eino. The kitchen-sink context. Undo. An edit form in the web app — the capture is the edit
surface. Memory update and delete: KIV; a user cannot point at what they cannot recall.

## 11. Risks

- **Tool arguments are unconstrained JSON.** Validation is a lint fault, not a transport failure.
- **Latency.** Up to 20 loop calls plus 2 finalize calls with a retry. The cap bounds a stuck
  turn; it does not make it fast.
- **Budget burn on repeated lookups.** The per-result count and the last-call rule bound it.
- **A wrong lookup looks like a wrong plan.** L3 cannot tell them apart, which is why L1 exists.
- **A small integer is a real id.** The defence is a clean lookup — words, day, clock, zone,
  state printed, id copied — and the whitelist as backstop.
- **The model writes a change twice**, once to `check_update` and once to the schema. A
  disagreement is lint's to catch, and the model pays the one retry.
- **A change with no row has nothing to change.** An empty lookup is legal for a create and a
  dead end for an update; only the prompt turns that into a question.
- **A call-off can be over-read.** Only the prompt's vocabulary separates a change from a
  call-off, so a case pins each way.
