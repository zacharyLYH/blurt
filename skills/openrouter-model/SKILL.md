---
name: openrouter-model
description: Call the hosted core planner over an OpenAI-compatible chat route (OpenRouter), with tool calls, a strict JSON schema, structured-output routing and a reasoning model. Use when the agent loop stalls, the core answer is unparseable, a free endpoint ignores the schema or the tools, or the transport needs the right request body.
---

# The hosted core model

The core planner — the bounded agent loop that places a capture as records or asks the
question — rides a user's own provider, not the home gateway. Its settings
(`ModelProvider`, `ModelName`, `ModelEndpoint`, `ModelApiKey`, the key encrypted
at rest) live on the User row; `internal/agent.UserModels` opens them and builds
one `ModelClient` per Job. The prescreen, the post-agent check and the tagger stay on
the home gateway (`skills/gateway-records`).

The endpoint is OpenAI-shaped, which is the difference from `/v1/infer`:

```sh
curl -s https://openrouter.ai/api/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'content-type: application/json' -d '{
    "model":"<model-id>",
    "messages":[{"role":"system","content":"..."},{"role":"user","content":"..."}],
    "max_tokens":4096,
    "reasoning":{"enabled":true},
    "response_format":{"type":"json_schema","json_schema":{
      "name":"core","strict":true,"schema":{...}}},
    "provider":{"require_parameters":true}}'
```

The answer is at `choices[0].message.content`, again a **JSON string** that needs
a second decode — or, on the loop's free-form turn, at `choices[0].message.tool_calls`,
where each call carries `function.name` and `function.arguments` as a JSON string.

## The loop has two shapes of call

| Turn | Body | Read as |
| --- | --- | --- |
| agent | messages + `tools`, **no** `response_format` | prose and/or `tool_calls` |
| finalize | messages + `response_format` strict, **no** `tools` | the record schema |

A forced schema can never ask to look something up, so the tool turn is free-form and the
call behind `submit_plan` is the strict one. The loop is capped at 10 model calls, every
tool result carries the calls left, and the answer is linted by the server: a fault restarts
the loop once with the faults in hand, and a second fault fails the Job.

Blurt's tools are `find_reminders(query)`, `find_memories(query)` and `submit_plan()`;
the two lookups are scoped by the Job's `UserId` server-side, so nothing the model writes
can widen a lookup. The answer is not a tool: `submit_plan` triggers the strict call that
writes it, and that call is the loop's only exit.

## The three knobs that are not optional

1. **`strict: true`.** Without it the schema is a hint, and a model that drifted
   produced a quote-short answer (`"time": "18:00, "recurrence"`) that failed the
   decoder. Blurt retries an unreadable answer (`callAttempts`) and records the
   head of it in the error, because a strict endpoint that still returns prose is
   a model problem, not a schema one.
2. **`provider.require_parameters: true`.** OpenRouter routes per endpoint.
   Without this flag an answer can come back from an endpoint that dropped
   structured outputs, which is how a strict request returns free text.
3. **`reasoning.enabled` with headroom.** A reasoning model spends output tokens
   before its answer; a 200-token cap starves it. Blurt caps at 4096, which is
   headroom, not a target.

`strict` needs a closed schema: `additionalProperties: false` and every field in
`required`. Blurt's core schema has nine string fields (`kind`, `text`,
`memoryText`, `zone`, `day`, `time`, `recurrence`, `evidence`, `question`); `kind`
is an enum, `recurrence` is an enum, and `time` is pinned by a pattern that also
matches the empty string. Empty is a legal answer wherever the user said nothing:
the server's `MissingFields`, not the model, decides what is committable.

**Do not send `temperature` unless a stage named one.** A reasoning model pins its own
temperature, and `require_parameters: true` turns a `"temperature": 0` into a requirement
no endpoint can meet: every call comes back `404 No endpoints found that can handle the
requested parameters`, which reads like a dead model id and is not.

## What the server owns

- `kind` is read first, and the two kinds are exclusive: a `question` carrying a
  record field is refused (`a question answer carries no record fields, but text was
  filled`), and a `records` answer that also asks is refused too. The decoder cannot
  enforce that, so the server does, and the fault goes back to the model.
- `day` is a token (`tomorrow`, `in:13d`, `next:2:tuesday`, `on:12-25`) that
  `resolveDay` turns into a date. The model never computes a date.
- `time` is the notify clock, never the event's clock.
- `kind: records` is linted by `MissingFields` plus the day token, the zone load and the
  instant still being ahead; a fault goes back to the model as the retry's conversation,
  and a second fault fails the Job rather than reaching the user as a question.
- `kind: question` passes lint untouched: asking is the answer, and it is what puts
  the Job on the clarification path.
- A model answer is refused unless `kind` is `records` or `question` — the server
  never infers the kind from an empty field.

## Exercises

The hosted case list was removed with the one-shot planner; evals come back as their own
layer. Until then the transport and the loop are pinned offline:

```sh
go test ./internal/agent -run 'TestModel|TestTools|TestWorkerLints' -v
BLURT_TEST_REDIS_URL=... go test ./internal/flow -run Redis -v
```
