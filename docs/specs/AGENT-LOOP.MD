# Agent loop

Status: built, §14 included. The moves below are `internal/agent`: `loop.go` (the moves),
`tools.go` (the tool table and what each call answers), `lint.go` (the checks and the note the
model reads), `prompt.go` (the agent prompt, the record schema and the context block) and
`model.go` (the two wire shapes). Eval: L1 (the agent loop), L2 (the two helpers) and L3 (the
chain) are built under `eval/`.

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
| `commit` | the accepted records become rows, and the row an update names is rewritten, in one step |

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
clock, recurrence), `resolveDay`, the zone load, and the instant still being ahead.
Same code, same errors the user would have been asked about, now offered to the
model first.

One test sits above the checks, because it is about the answer rather than a record:
the whitelist. An answer that names a row (§14) may name only an id this run's own
lookups returned; anything else is the first fault, before the fields are read. The
ids a run may name are small integers over one user's own rows, so a model that
invents `42` would otherwise reach a row it never saw.

`lint` guards the final output. It reads the final JSON and the ids this run's own lookups
printed (§14), and nothing else. Its records half is the list of checks above; the answer's own
composition — one kind, the verb, the row it names, and the fields the other kind does not use —
is refused where the JSON is read, so it arrives as a fault the same way. A malformed
finalization call, bad submit arguments, and a missing `submit_plan` are all lint faults and
follow the same retry rule.

A `question` answer passes lint without field checks; asking is the point. It ends
the agent and goes to the clarification path. A `cancel` answer passes lint with no record at
all: the app owns that write (§14.6).

## 4. Tools

Three read-only answers, no writes, plus the finalization tool. Every lookup and the check are
scoped by the Job's `UserId`, so a tool can never reach a row the caller does not own.

| Tool | Returns |
| --- | --- |
| `find_reminders(query)` | ≤5 rows: id, text, the date it falls on with its clock, zone, status |
| `find_memories(query)` | ≤5 rows: text, tags |
| `check_update(reminder_id, text, zone, day, time, recurrence)` | whether that row can be rewritten now, and whether the place it would move to passes lint's own checks |
| `submit_plan()` | ends the loop, triggers the strict call |

- The id is the row's own id (`ReminderId`), no minted handle. It is printed in the tool
  results, which is the only place a reminder is read: the context block carries memories
  alone (§5).
- A row prints its state as `pending` while it can still be changed, and as `fired` or
  `cancelled` once it cannot. The stored word for a pending row is `resolved`, which reads to a
  model as a row that is done — so the tool says the one thing the model has to decide.
- A lookup result carries the whole row, so a plan never needs a second read of it;
  `check_update` reads the row again, to see whether it can still be changed.
- An id this run's own `find_reminders` did not return is refused, by the tool and by
  lint (§14).
- `find_reminders` reads Reminder text; `find_memories` reuses the existing FTS.
- `check_update` is the change pre-check; what it answers, and why it is advisory, is §14.
- Cancelling has no tool. The app owns that write, and §14.6 is the answer that says so.
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
| **L1 agent** | the full agent loop: seeded DB, real tools, real lint retry | did it read this capture right, which row it named, and which field missed |
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
server/internal/agent/eval/cases/<name>.json
server/internal/agent/eval/cases/_fixtures/<name>.json   # named large seeds
server/internal/agent/eval/cases/_golden/<name>.txt      # rendered context block
server/internal/agent/eval/cases/_audio/<name>.wav       # an L3 clip capture
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
  `asks_for` is a hard key and matches by substring. `updated` (§14.9) is read over the user's
  whole table, because a row this run moved still belongs to the Job that created it, and
  `reply_contains` reads the assistant turn, which is where a `cancel` answer lives.
  `unchanged` names the rows a run must leave exactly as they were, found by the same text
  substring a reminder is asserted with: it is how an overlapping chain says the change
  landed on one row and not its neighbours.
- **An L2 case** adds `stage` (`prescreen` or `tags`) plus that stage's input — the `capture`
  or the `fact` — and its truth under `expect.helper`. The runner checks the pairing at load,
  so a case that names a stage it cannot feed fails before it spends a call.
- **An L3 case** is `steps`, in order, each with its own `expect`: a `capture` opens a Job,
  a `clip` opens one from a recorded fixture, and a `reply` answers the newest Job still
  waiting on the user. Its `kind` is `records`, `question` or `failed`; a Memory may carry
  `tags_min`, because the tagger's answer is a guess and the count is the claim. A chain has
  no recorder in front of the model, so `tools` is refused at load: naming a row already
  implies the lookup the whitelist demands.

## 9. Seeding

One operation, in the test build (`eval/cases_test.go` and `eval/doubles_test.go`) where
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
cd server && go test ./internal/agent/eval -run TestPlannerCases -audit -v
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

Eino. The kitchen-sink context. Undo. An edit form in the web app — the capture is the edit
surface, and a row is called off from the app's own day modal.

Memory update and delete: KIV. A user will not remember everything they told Blurt, so asking
them to edit a fact is the wrong shape — they cannot point at what they cannot recall. Stale
facts get weeded by a periodic check-in inside the chatbox instead, later.

## 14. Change and call-off

A capture does one or more of three things to a Reminder: it creates one, changes one, calls one
off — and it may do several in one breath. §1-§13 built the first. This section adds the second,
the one answer the third ever gets, which is that it belongs to the app: calling a reminder off is
a button on the row, not a thing a model does from a voice note. A user who cannot remember what
they have cannot name it by voice either, and a user who can is already looking at the page that
has the button.

Same prescreen, same loop and budget, same lint, same commit step. What changes is that an answer
is a list of ops on rows, and that a call-off op writes nothing at all.

### 14.1 The answer is a list of ops

The strict schema carries one list: `ops`. Each entry is one thing the capture does to one row,
in the order the capture says them, and each names its own verb:

- `intent` — `create`, `update` or `cancel`. `cancel` names what the user wants, not what the
  agent does: it does nothing but say where it is done.
- `reminder_id` — the row that op is about, `0` unless the op is an `update`. An integer,
  because it is the row's own id and never a handle the server mints.
- Then the record fields the capture said: `text`, `zone`, `day`, `time`, `recurrence`.

Two rows moved in one breath is two ops in one answer, and the server checks and commits each
one on its own: the transaction is already all-or-nothing, so two updates land together or not
at all (§14.3). That is why the loop reads a list and not a verb — a capture is one thought,
and half-honouring it was the worst outcome on the table: the user read "Reminder updated."
while the second row sat where it was.

`kind` stays the shape (`records` or `question`), so a question and a call-off are answers to
different questions. A `question` carries no op, no fact and no evidence, refused where the
JSON is read: asking is how a change with an ambiguous row ends — "which dentist appointment?"
— and the ask is the whole answer.

Per op, refused by lint like every other shape fault:

| `intent` | `reminder_id` | record an op carries |
| --- | --- | --- |
| `create` | 0 | one, whole |
| `update` | ≠0 | one, a delta |
| `cancel` | 0 | none |

An `intent` outside the three is refused by name, the way `kind` is. `intent` classifies a
capture's action on a row only: a note that carries a fact alone has no op, memories ride any
answer, and a Memory is never updated or deleted here (§13). An answer with no op and no fact
proposes nothing, which is its own fault.

Ambiguity has no other exit. The server never ranks a best match, so two candidates for *one*
op are the model's to ask about: the Job goes to clarification, and the user's next turn is the
one that picks. Nothing found is the same question. That is why a crowded or an empty lookup is
a case (§14.9) and not a fallback in code.

### 14.2 The whitelist

lint's first test on each op, before any field: its `reminder_id` must be an id this run's own
`find_reminders` returned. The ids ride the run, from what the tools already printed — this is
not a second store read, it is the answer checked against what the model was shown. A model that
names an id it never saw is refused, whatever it was about to do with it, and one bad id is
enough to refuse the answer. Small integers over one user's own rows make this the cheapest and
most valuable check in the file: a hallucinated `42` is a real row that somebody owns.

### 14.3 Update rewrites in place

- An update rewrites the named row: `Text`, `Timezone`, and the local time with its derived
  instant — or, for a repeat, the rule beside it. `Status`, `CreatedAt` and `SourceJobId` are
  untouched, so the row keeps its id, its place on the page and its history. The predicate is
  `Cancel`'s: the caller's own row, and still `resolved`.
- It rides `CommitAgentStep`'s transaction with the inserts, so one turn writes all of it or
  none of it, and the Trace says which verb it was.
- The new value is built by `ReminderFor`, exactly as a create's is: one derivation of a local
  time, a rule and an instant, so a moved reminder cannot drift from a made one.
- A call-off never reaches here: `ReminderStore.Cancel` stays the app's write, behind
  `POST /api/reminders/{id}/cancel`. The agent does not call it (§14.6).

Rejected: an update as a call-off plus a create. It buys zero new SQL, and it costs the row's id
— any deeplink or notification pointing at the old id goes stale — a `cancelled` row in Past for
every move the user ever makes, and a status that says they called something off when they only
moved it. A polished app edits in place; the ~30 lines it costs are the cheapest honest answer.

One race is left, and it is refused rather than papered over: a row that fires or is cancelled
between the lookup and the commit no longer matches the rewrite's predicate. A rewrite that
matches no row fails the turn (`ErrReminderStale`), the way a cancel of an already-fired row
returns false — the capture is not resurrected as a new row (a duplicate fire is the PRD's worst
outcome) and the reply is not allowed to claim a change that did not happen. The Trace records
`stale: true`.

### 14.4 A change says only what changed

"make it 8pm" names a clock and nothing else, and that is all the model writes. Nothing is copied
by the model: the server fills every field the answer left empty from the row it is rewriting — its
words, its zone, the day it falls on, its clock and its repeat — and lint sees the whole record as
usual. A field the model does name replaces the row's, never the other way round, so the copy that
could be wrong does not exist.

The row prints the date as it is, `2026-10-27 10:00 Asia/Singapore`, so `day` carries a date as
well as a day word and nothing has to tell them apart by prefix: a full date is taken as it stands
and never walked forward, while a month and a day (`12-02`) is a day the user said and is walked
to the next year that has it.

A change also moves a row between shapes: a `recurrence` on a one-off writes the rule, a `none`
on a repeat clears it, and the row keeps its id through either conversion — the rule row follows
whatever the Reminder itself now is. An empty `recurrence` is the one word that means "the row's",
which is why the enum carries it.

The consequence is the point: a record is always whole, so `MissingFields`, `resolveDay`, the
zone load and the after-now check are the same four rules for a change as for a create. An
update to a clock that has gone is refused by `checkAfterNow` with no new code, and a repeat's
weekday, day-of-month or anniversary is pinned by `ruleFor` from the date the model copied.

### 14.5 The check

`check_update(reminder_id, text, zone, day, time, recurrence)` exists so the model verifies
before it submits: a tool call is free, and the lint retry it saves is the whole budget. A field
the change left empty is filled from the row before the checks run, so it asks about the same
record the commit will write. It answers `can update id=42 to 20:00 on 2026-10-27 Asia/Singapore`,
or the row's refusal — not a row this run looked up, no such row, no longer resolved — or the
faults the place would earn.

- It reads the row by id, scoped by owner: `Recall.FindReminder`, over `ReminderStore.ByID`.
- It builds the `Record` its arguments describe and runs the linter's own `checks` list over it,
  so a place it accepts is a place lint accepts — one implementation, one wording, no second
  rulebook, and no drift when a check is added.
- It is advisory, the way recall is: a refusal is a tool result the model reads, never a fault.
  lint still decides at submit, so a model that skips it, or writes a record that differs from
  the one it checked, meets lint exactly as it always did.

### 14.6 A call-off goes back to the app

A `cancel` answer writes nothing and ends the Job `complete`, with one fixed sentence where a
reply would be:

> I think you asked to cancel a reminder? Blurt doesn't cancel reminders on your behalf, but you
> may go ahead and delete it on the app yourself!

Server-owned like every other reply, and fixed here on purpose — there is no record to describe,
and prose about a row nothing touched is the one place a wrong claim costs nothing to make. It
opens with a hedge because the answer names no row: the most the turn can honestly claim is how
it read the capture.

Not a question. A question ends the Job in clarification and waits for an answer that cannot
come: the user already said what they wanted, and the server has nowhere to put it. The sentence
points at the write that does exist — `POST /api/reminders/{id}/cancel`, reached from the day
modal's own button — rather than at an agent action that never will. There is nothing to be
ambiguous about either: the answer names no row, so three dentist appointments get the same
sentence as one.

The rest of the reply set is unchanged: `Reminder set.`, `Reminder updated.`, `Noted.`,
strongest verb first.

### 14.7 The prompt

`AgentPrompt` is decisions and nothing else: five short paragraphs, one per question the model
has to answer — what a capture carries, whether it is about a row, what a change copies, when to
ask, and what it is handed. What each field means lives in `plannerSchema`, which travels with
every call: two copies of the day grammar were two things to keep in step, and the shorter prompt
scored the same cases.

```
Is it about a row already on their calendar? A capture that changes one ("move", "push", "make it
…", "the X should be …", "the X is … now"), repeats it ("again", "same as before"), or leans on a
time only that row knows, is about that row: call find_reminders first, and take what the capture
leaves out from the row you find. One row is that row, however loosely it fits; ask which one only
when two could be it. Two rows acted on is two ops, each naming its own. A change that finds no row
has nothing to change, so it asks. A capture that calls something off ("cancel", "delete", "scrap")
is not a change: answer intent cancel, name no row, look nothing up, write nothing — the app owns
that write. A row that has already fired or been cancelled is history.

A change names the row and then only what the capture changes: its reminder_id, plus the fields it
actually moves. The server keeps the rest of the row, so an empty field means "as it was", and a
repeat of none drops it.
```

- The tools paragraph names the four answers and the count, and says nothing else.
- No worked examples, for the reason it has none already: a model handed them measures recall of
  the examples.

### 14.8 Code, in order

1. `time.go` — a date beside the day words, with no prefix. The vocabulary is closed, so a unit
   test pins it.
2. `model.go` — `Op`, `Answer.Ops`, the ops schema, the kind exclusivity in `answerFrom`.
3. `lint.go` — the whitelist and the verb, per op, and a `cancel` op accepted with no record.
4. `tools.go` — `check_update`, and the ids this run may name.
5. `store` — `AgentStep.Updates` inside `CommitAgentStep`'s transaction, `ReminderStore.ByID`,
   and the rewrite's own predicate with `ErrReminderStale`.
6. `loop.go` — the update branch, the `cancel` op that writes nothing and earns its sentence, the
   `settleRecord` step that fills a delta from the op's own row before lint, and the Trace
   (`intent` as a list, `reminderIds`, `stale`).
7. `prompt.go` — §14.7.
8. unit tests, which encode server authority rather than UX: the token, the op shape, the
   whitelist, the check's answers, and both commit branches.
9. the cases below, then the three lanes re-run.

### 14.9 Retest

Two `expect` keys, and only one of them is about the table:

| Key | Means |
| --- | --- |
| `updated` | one entry per row a change rewrote, in op order, then read back by the ids the Trace names with the new `datetime`, `utc`, `zone`, `recurrence` and `status` |
| `reply_contains` | the assistant turn this run published contains each string — the one case that asserts on a sentence, because a `cancel` answer is a sentence and nothing else |

`updated` is `[]ExpectReminder`, the shape a case already uses for the rows a run creates, so a
case names only the fields it is about. It is read from the ids the run named in its update ops
(`reminderIds` on the Trace) rather than by scanning the table, so a case pins the rows it
meant: a run that rewrites the wrong one fails on `text_contains` instead of passing because
something moved. More than one entry is a turn that moved more than one row, in op order.

`reminders` stays Job-scoped — it is what this run inserted — so an update case asserts
`"reminders": []` and gets, for free, the check that the server rewrote rather than re-created.
A `cancel` case asserts `"reminders": []` too, and that the seeded row it named is still
`resolved`: the point is that it changed nothing.

L1, one turn each:

| Case | Pins |
| --- | --- |
| `update-move-the-dentist` | "move the dentist to 2 december at 4pm" on one seeded row: `updated` on the new day at the new clock, `reminders: []`, tools `find_reminders` + `check_update` |
| `update-keeps-the-day-only-the-row-knows` | "the physio is at 9:30am now": the day copied out of the row, the clock out of the capture |
| `update-turns-a-one-off-into-a-repeat` | "the credit card bill is monthly now": the same row gains a monthly rule, no second row |
| `update-turns-a-repeat-into-a-one-off` | "the vitamins are just for tomorrow, not every day": the rule goes, the row keeps its id |
| `big-calendar-finds-one-row` | the same change through twelve seeded rows: one candidate, found by words |
| `cancel-the-dentist-reminder` | "cancel the dentist reminder": no tools, no rows, the fixed sentence, the seeded row untouched |
| `big-calendar-calls-a-row-off` | the same call-off with twelve rows in sight, and still no write |
| `update-two-rows-at-once` | "move the dentist to 3pm and the gym to 5pm": two ops, two rewrites, and `reminders: []` with both rows read back by id |
| `create-two-reminders-at-once` | "buy glue tomorrow at 6pm and call mum at 7pm": two ops, two rows, one answer and one sentence |

Not yet written, one file each: a text-only change, a zone that travels with the row, two
candidates asking which, and a change with no row at all.

L2: no new case. It is untouched by this slice and is re-run as the regression gate — a
prescreen or a tagger that moved under a schema change shows there first, at one call a case.

L3, the chain, where the feedback loop is real:

| Case | Pins |
| --- | --- |
| `chain-move-the-reminder-it-just-created` | capture, then "make the glue 8pm": step 2 finds the row step 1 committed, so the lookup spans Jobs, and a change that names only the clock keeps the words it had |
| `chain-update-asks-then-moves` | "move the dentist" with no clock: a question, the reply names the day and time, and the second run looks the row up again to rewrite it |
| `chain-cancel-changes-nothing` | "cancel the dentist reminder" on a seeded row: no rows written, the fixed sentence, and the row is still a row the next capture can move |
| `chain-two-rows-one-answer` | two rows added in one answer, then both moved in one answer: the list is what the loop reads, and neither row is left behind |
| `chain-a-day-of-changes` | five turns on one database — create, move a row that lives in `Asia/Tokyo`, create again, call one off, then move the row the call-off left alone — with `unchanged` pinning every row the turn was not about |

## 15. Risks

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
- **A small integer is a real id.** The defence is a clean lookup: a row prints its id, words,
  the date it falls on, its clock, zone and state, so the model picks by words and copies an id
  it read. The whitelist is the backstop, not the plan — a model can still be unwise, but it can
  never touch a row it was not shown. It runs first, before any field.
- **The model writes the update twice**, once to `check_update` and once to the finalize schema.
  A disagreement is not the tool's to catch: the linter catches it, and the model pays its one
  retry for it.
- **A change with no row has nothing to change.** An empty lookup is a legal answer for a create
  and a dead end for an update. Only the prompt turns that into a question, and only a case pins
  the prompt.
- **A rewrite can lose its row to the clock.** §14.3: a row that fired in between fails the turn.
  Rare, and the alternative is a duplicate fire or a change the reply claims and the table never
  got.
- **A call-off can be over-read.** A model that reads a change as a call-off writes nothing and
  points at the app; a model that reads a call-off as a create writes the row the user wanted
  gone. Only the prompt's vocabulary separates them, so a case pins each way.
- **The cancel sentence is a promise about the app.** It is true while the app has its own
  delete/call-off write on the row; if that moves, the sentence moves with it, and the case with
  it.
