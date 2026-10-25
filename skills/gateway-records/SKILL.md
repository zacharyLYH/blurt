---
name: gateway-records
description: Call the self-hosted gateway's chat route to turn a capture into Reminder and Memory records (Blurt's AgentLoop model). Use when a model answer is unparseable, a field comes back invented or truncated, or you need the exact /v1/infer body for a chat model.
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

1. **A constrained decoder fills every field you declare.** A field cannot be left
   empty for long, so a model asked for a resolved local date and time invented
   `2026-10-26T17:00` for `buy glue when I'm home from school` — a time the capture
   never contained — and read `6pm tomorrow` as `18:30`, the clock out of
   `now_local`. Ask it to *copy the user's own words* instead (`timePhrase`) and it
   does that correctly; do the arithmetic on the server. Its own words for an
   unclear time (`when I am home`) come back verbatim, which is what lets the
   server decide to ask.
2. **Two or three fields is the sweet spot; ten is a collapse.** A ten-field
   all-required schema produced a truncated answer followed by repeated
   `DoublyModeratorIsMean` tokens and a leaked Chinese `提醒`. The schema Blurt
   ships declares five all-string fields, and a reply is 40–60 tokens. Numbers,
   dates, and objects leave a small model room to wander: keep every field a
   string.
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
   was inverted-ish (0.11 ambiguous, 0.29 clear). Blurt's ambiguity decision is a
   deterministic phrase resolver, not a model.
7. **`max_tokens` is a cap worth setting low.** It is what stops a model that has
   started repeating itself, and the measured reply needs 60.

## Where it lives

`internal/agent`: `model.go` is the transport and the `Model` port, `prompt.go` holds
the prompt and the schema (the two things that must be edited together), `phrase.go`
resolves the human time, `loop.go` is the AgentLoop — the state machine that checks
and commits — and `worker.go` is only the poller. `agent.Fake` is the scripted model
behind `BLURT_FAKE_AGENT`; it keys off the words `remember` and `when` so a spec can
reach each branch on purpose.