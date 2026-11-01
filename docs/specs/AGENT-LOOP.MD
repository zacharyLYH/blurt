# Agent loop

Status: built. The moves below are `internal/agent`: `loop.go` (the moves), `tools.go` (the
tool table and what each call answers), `lint.go` (the checks and the note the model reads),
`prompt.go` (the agent prompt, the record schema and the context block) and `model.go` (the
two wire shapes). Eval: L1 (the agent loop), L2 (the two helpers) and L3 (the chain) are built
under `L1/`.

Migration is not a constraint. Keep only what is already the best shape
(`MissingFields`, `resolveDay`, the store, the two wire envelopes) and rebuild the
rest. The end state is what matters, not the size of the diff.

The planner turn stops being one call. It becomes a bounded agent loop: the model
calls tools, then asks for a final answer, the server lints that answer, and only a
linter that stays unhappy reaches the user. Server authority does not move:
`MissingFields`, `resolveDay`, the after-now check and the zone load still decide
what is committable.

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
lint ─┬─ pass, records ─┬─▶ commit
      │                 └─▶ question
      ├─ pass, question ─▶ clarification
      ├─ fault, first ───▶ agent loop (fresh budget)
      └─ fault, second ──▶ error + notify
```

| Move | What it is |
| --- | --- |
| `prescreen` | the gate, one call, outside the loop |
| `agent` | the tool loop, ending in `submit_plan` |
| `finalize` | one strict-schema call |
| `lint` | the server's deterministic checks on the final JSON |
| `commit` | the accepted records become rows, stored in one step |

`prescreen` is an independent turn. It is not part of the agent loop.

A second, model-based check of the accepted row used to sit between the plan and the commit.
It was deleted: `lint` owns structure, the planner is the stronger model, and a weaker model
judging its output refused correct rows (a reminder became a question). The reason it existed —
a clock the user never said — is a comparison `lint` can make exactly. The `resolve` and
`commit` moves were split for it to sit between; they are one move now.

## 2. The agent loop

A normal agent: one system prompt, tools, and a loop. The model decides what to
look up. It terminates by calling `submit_plan`, which triggers one strict-schema
call. The loop never pauses.

Budget: **10 model calls per agent loop, plus 1 finalize call**. Tool calls are
unlimited within those 10, including several per response. Every tool result
carries the remaining loop count, and the system prompt states the cap.

The last loop call must be `submit_plan`. A loop that spends its budget without it
gets a synthetic lint fault. On the first fault the loop restarts with a fresh
10 + 1 and the note; on the second it is an error.

A lint fault restarts the loop with the full conversation, the lint errors, and a
fresh budget. **One lint retry maximum.** A second fault is an error plus a user
notification.

## 3. The linter

`lint` is a list of checks behind one interface, so a new check is a new function
and nothing else:

```go
type check func(Record, time.Time) []string // nil when the record passes
```

Every check runs, every error is collected, and the whole list goes back to the
agent in one block:

```
lint: time "18:00," is not HH:MM
lint: zone "Singapore" is not IANA
```

The checks are the server's existing ones: `MissingFields` (event, zone, date,
clock, recurrence), `resolveDay`, the zone load, the instant still being ahead, and
the target whitelist. Same code, same errors the user would have been asked about,
now offered to the model first.

`lint` guards the final output. It reads only the final JSON. Its records half is the list of
checks above; the answer's own composition — one kind, and the fields the other kind does not
use — is refused where the JSON is read, so it arrives as a fault the same way. A malformed
finalization call, bad submit arguments, and a missing `submit_plan` are all lint faults and
follow the same retry rule.

A `question` answer passes lint without field checks; asking is the point. It ends
the agent and goes to the clarification path.

## 4. Tools

Two read-only lookups, no writes, plus the finalization tool. Every lookup is
scoped by the Job's `UserId`; the model never sees an id it can pass and never
supplies one.

| Tool | Returns |
| --- | --- |
| `find_reminders(query)` | ≤5 rows: id, text, when, zone, status |
| `find_memories(query)` | ≤5 rows: text, tags |
| `submit_plan()` | ends the loop, triggers the strict call |

- The id is the row's own id (`ReminderId`), no minted handle. It is printed in the tool
  results, which is the only place a reminder is read: the context block carries memories
  alone (§5).
- A result carries the whole row, so nothing looks a reminder up by id again.
- An id outside this run's own results is refused.
- `find_reminders` reads Reminder text; `find_memories` reuses the existing FTS.
- Empty results are a legal answer.

## 5. Context

The prefetch is built once per request, on the first loop. A lint retry reuses it.
It contains only what cannot be looked up: at most 10 memories, no minimum.

```
current_tz: Asia/Singapore
now_local: 2026-10-24T18:30
memories:
- I like this noodle shop in Chongqing [food, chongqing]
```

Memories are the existing FTS read over all user text in the Job. Everything else
is a lookup.

## 6. Sending

| Turn | Body |
| --- | --- |
| agent | messages + `tools`, **no** `response_format` |
| finalize | messages + `response_format` strict, **no** `tools` |

The tools turn is free-form. The finalize turn is strict. The schema is still what
the answer is read through.

## 7. Evals

Three layers, all on real models. One seed, one report. Never merge them.

| Layer | Runs | What it answers |
| --- | --- | --- |
| **L1 agent** | the full agent loop: seeded DB, real tools, real lint retry | did it read this capture right, and which field missed |
| **L2 helpers** | `Prescreen`, `Tags`: one call each | did this stage decide right |
| **L3 chain** | `internal/backend` over HTTP: real DB, real workers, text and recorded capture | does a capture still become the right rows, end to end |

L2 is built and cheap: a case is one call, so `-repeat` is where its answers mean something —
a screen that refuses a good capture costs the user the thing they came to say, and one run
cannot tell that from a coin flip. Its arm is the deployment's own: one gateway model id per
stage, read from the same `.env` the server boots on.

Nothing is stubbed. The case seeds the DB, the real tools find the real rows, and
the model is the one the deployment would use. L1 mirrors the production loop,
including the single lint retry.

L3 is the sanity pass, and coarse on purpose: it says a capture failed, not which
stage. L1 and L2 are the diagnosis, and they are cheaper because nothing queues.

L3 is also the only lane where the feedback loop is real: a capture becomes a question, the
reply is a new user turn, and the row it commits is read back. Several schedules in one
database are its other shape — a trip as Memory, then a Reminder inside the trip's dates —
which no single-turn case can express. It drives `internal/backend` — the same assembly main
boots — through the HTTP surface a browser uses, so the upload, the streams and the
transcription are production's and only the models and the clock are the test's. Its arm is
the whole deployment: planner from `BLURT_EVAL_*`, prescreen and tagger from the gateway, and
`BLURT_STT_*` for the one step that records. A row's prose and a Memory's tags belong to a
model, so a case pins a substring and `tags_min`, never the exact words.

The pump's existing scripted runs stay put: they are the plumbing tests, and they
separate a broken pipe from a broken model.

Browser e2e is UI regression, not eval.

## 8. Cases

One case, one file. Add is a file, delete is `rm`, audit is one file plus its
golden.

```
server/internal/agent/L1/cases/<name>.json
server/internal/agent/L1/cases/_fixtures/<name>.json   # named large seeds
server/internal/agent/L1/cases/_golden/<name>.txt      # rendered context block
server/internal/agent/L1/cases/_audio/<name>.wav       # an L3 clip capture
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

- **`seed` is JSON rows**, structured, mirroring the store. Times may be relative
  to `now_local` (`+1d 17:00`, `-2h`) so a case still means the same thing next
  year.
- **`use`** pulls a named fixture in. The fixture is for reuse; the golden is what
  you read.
- **`expect`** keeps today's keys and adds nothing until a case needs it.
  `asks_for` is a hard key and matches by substring.
- **An L2 case** adds `stage` (`prescreen` or `tags`) plus that stage's input — the `capture`
  or the `fact` — and its truth under `expect.helper`. The runner checks the pairing at load,
  so a case that names a stage it cannot feed fails before it spends a call.
- **An L3 case** is `steps`, in order, each with its own `expect`: a `capture` opens a Job,
  a `clip` opens one from a recorded fixture, and a `reply` answers the newest Job still
  waiting on the user. Its `kind` is `records`, `question` or `failed`; a Memory may carry
  `tags_min`, because the tagger's answer is a guess and the count is the claim.

## 9. Seeding

One operation, in the test build (`L1/cases_test.go` and `L1/doubles_test.go`) where
the rest of the harness lives:

```
seed(ctx, db, Case) → inserts Reminder, RecurringReminder and Memory rows
```

`flow` and the hosted runs reuse it when their layers are built; today the L1 and L3
runners call it.

Each case runs against a fresh database. Row ids are stable, so goldens do not
drift.

The runner seeds the real database, then lets the real stores and the real
`contextBlock` build the prompt. Nothing hand-built, no rendered strings, no
fixture grammar to parse back.

## 10. Audit

```
cd server && go test ./internal/agent/L1 -run TestPlannerCases -audit -v
```

Per case: seed rows, rendered context block, tools called, calls used, the answer,
the lint faults, the failures. The golden context is checked in, so a prompt
change shows as a diff.

## 11. Report

One format for all three layers: case, layer, size, ok, per-field failures, calls
used, latency, model. Written to a file, one per run name, so two models compare by
diff rather than by two logs read side by side.

## 12. Order

1. the seed helper (§9)
2. L1, the score, and the fastest lane to iterate a prompt on — built
3. L2, the two helpers, one call each — built
4. L3, the chain, last because real calls make it the slowest — built

## 13. Out of scope

Eino. The kitchen-sink context. Memory delete. Multi-target cancel. Update.

## 14. Risks

- **Tool arguments are unconstrained JSON.** Validation is a lint fault, not a
  transport failure.
- **Latency.** Up to 20 loop calls and 2 finalize calls with a lint retry. The cap
  bounds a stuck turn; it does not make it fast.
- **The loop can burn its budget on repeated lookups.** The injected count and the
  last-call rule bound it.
- **A wrong lookup looks like a wrong plan.** L3 alone cannot tell them apart,
  which is why L1 exists.
- **Far-ahead targets need a lookup.** L1 must cover retrieval, not only
  placement.
