---
name: gateway-records
description: Call the self-hosted gateway's chat route to turn a capture into structured Reminder and Memory records (Blurt's AgentLoop model). Use when a model answer is unparseable, a field comes back invented or truncated, or you need the exact /v1/infer body for a chat model.
---

# Gateway records over /v1/infer

The gateway serves phonon (audio), laya (typed decisions), and chat models, all
over the same `/v1/infer` route with a different `payload`. A chat model takes
OpenAI-shaped messages plus a JSON schema, and answers with an OpenAI-shaped
completion nested under `output`:

```sh
curl -s https://<gateway>/v1/infer -H "Authorization: Bearer $KEY" \
  -H 'content-type: application/json' -d '{"model":"<model-id>","payload":{
    "messages":[{"role":"system","content":"..."},{"role":"user","content":"..."}],
    "max_tokens":200,
    "response_format":{"type":"json_schema","json_schema":{"name":"records","schema":{...}}}}}'
```

The answer is at `output.choices[0].message.content` — and that content is a
**JSON string**, not an object, so it needs a second decode. There is no
`tool_calls`: the schema is what constrains the model, which is why Blurt does not
ask it to pick a tool.

## The traps

1. **An empty string has to be a legal answer, or the model fills the field with an
   invention.** A constrained decoder fills every field you declare: a model asked for
   a resolved local date and time invented `2026-10-26T17:00` for `buy glue when I'm
   home from school`, and read `6pm tomorrow` as `18:30` — the clock out of
   `now_local`. The answer is not to ask for prose and resolve it on the server (that
   was tried; it moved the guessing, it did not remove it). Declare the two resolved
   fields as strings whose pattern **also matches the empty string**
   (`"^\\d{4}-\\d{2}-\\d{2}$|^$"`), and put the clock in the prompt:
   `now_local: 2026-10-24T18:30`. A model told it may leave a field empty leaves the
   ones it cannot know empty, and an empty field is the server's cue to ask the user —
   a decision the server makes, not the model.
2. **Seven fields, two of them patterned, one an enum — and keep every field a string.**
   The schema declares `text`, `memoryText`, `zone`, `date`, `time`, `recurrence`,
   `evidence`; `recurrence` is an enum (`none`/`daily`/`weekly`/`monthly`/`yearly`) so a
   pattern the server cannot read never arrives at all. A ten-field all-required schema is
   what collapses: it produced a truncated answer followed by repeated
   `DoublyModeratorIsMean` tokens and a leaked Chinese `提醒`. Numbers, dates, and
   objects leave a small model room to wander. **Assert the schema on its compacted
   form**: `encoding/json` compacts a `json.RawMessage`, so a test looking for
   `"enum": [...]` with spaces fails against `"enum":[...]`.
3. **Few-shot examples decide the answer, and their order matters.** With an
   "ask the user" example *last*, a model answered `When should I remind you?` for
   a capture that named 6pm tomorrow. Ending on the example that yields the common
   case fixed it. Treat the system prompt as a versioned artifact, not prose.
4. **Do not ask a small model to choose a tool from an enum.** Six tool names: it
   chose wrong on a clear capture and on an ambiguous one both, and its own `say`
   disagreed with the tool it chose. One schema whose *filled fields* imply the
   intent was right three times out of three. Which record to commit is the
   server's decision.
5. **Do not enable thinking for extraction.** No thinking is under a second and
   answers honestly; a small thinking budget multiplied the latency and invented a
   deadline that was not in the input. Reserve it for hard questions and cap it.
6. **The classifier is not the router either.** Asked whether a capture needed a
   clarification, laya answered `CreateReminder` at 0.69 for an ambiguous capture
   and 0.81 for a clear one — no usable separation, and its `metadata.confidence`
   was inverted-ish (0.11 ambiguous, 0.29 clear). Blurt's accept/ask decision is
   deterministic (`MissingFields`: field format, timezone, an instant still ahead), and
   the model is only asked to *write the question* once the missing fields are known.
   The laya reader that used to re-read a proposed record is gone.
7. **`max_tokens` is a cap worth setting low.** It is what stops a model that has
   started repeating itself, and the measured reply needs 60.

## Where it lives

`internal/agent`: `model.go` is the transport, the `Write` shape and the `Model` port,
`prompt.go` holds the prompt and the schema (the two things that must be edited
together), `time.go` parses the structured date and time the model returns, `fields.go` is
the deterministic accept/ask check (`MissingFields`), `record.go` turns an accepted write
into the stored row — the local instant for a one-off, the rule and its next instant for a
repeat — and `loop.go` is the AgentLoop: the state machine that checks and either commits
or asks. `worker.go` is only the poller. `agent.Fake` is the scripted model behind
`BLURT_FAKE_AGENT`; it keys off the words `remember`, the frequency word (`every day`,
`daily`, `every week`, …) and whether the capture names a clock, so a spec can reach each
branch on purpose. A capture that repeats without naming how often it repeats gets the ask
rather than a one-off, because the fake will not guess a pattern it cannot read.

The tags call is a third, single-purpose ask on the same model, and it never rides the
records schema: a declared list gives a small model room to wander, so the answer is one
comma-separated string the server splits, lowercases, dedupes and caps. It runs behind the commit
that creates its Memory, not inside it: the record and the reply are durable before the
features are even asked for, so a slow tagger never holds a Job the user has already read,
and the features are written onto the row by their own step. That is what makes the result a
feature rather than more text — and it is best effort, so a failing call leaves the Memory
untagged rather than failing a capture the user already made.