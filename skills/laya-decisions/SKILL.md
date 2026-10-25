---
name: laya-decisions
description: Call the self-hosted gateway's laya model for typed decisions (the transcript-quality gate). Use when a decision kind is refused, answers look cut off, or you need the exact /v1/infer contract for laya.
---

# laya typed decisions

laya is the gateway's classifier model, reached over the same `/v1/infer` route
as phonon but with a different body: a state plus named typed questions, and one
typed answer per question.

```sh
curl -s https://<gateway>/v1/infer -H "Authorization: Bearer $KEY" \
  -H 'content-type: application/json' -d '{"model":"laya","payload":{
    "state":"Billed twice for March. Refund or we cancel.",
    "questions":{
      "dept":{"type":"choice","instructions":"Which team?",
              "criteria":{"billing":"refunds","technical":"bugs"}},
      "churn":{"type":"noul","instructions":"Does the customer threaten to cancel?"}}}}'
```

Answers come back typed per question id:

- `bool` (and its alias `noul`): `{"type":"bool","value":true,"probability":0.9654}`.
  The reply's type is always `bool`, even when the question asked with `noul`.
- `choice`: `{"type":"choice","value":"billing","probabilities":{"billing":0.9868,"technical":0.0132}}`.

## The traps

1. **Only `bool`, `noul`, and `choice` exist.** Anything else is
   `400 {"error":{"code":"upstream_error","message":"{\"detail\":\"This model does not support 'X' decisions\"}"}}`.
   The message is doubled JSON: the gateway wraps the model's own detail as a string.
2. **The window is 512 tokens and overflow is silent — no error.** `usage.input_tokens`
   pins at 512 however long the state is; the tail is simply dropped. Instructions and
   criteria serialize *after* the state, so an oversized state is what quietly deletes the
   question itself. `internal/decision` caps the state at `maxStateChars` for this reason.
3. **`output_tokens` is always 0.** laya answers from a decision head instead of generating
   text, so never expect a rationale or any prose.
4. **It is fast and advisory.** ~0.5 s on an idle host. Blurt's gate fails open: a decider
   error never blocks a capture, it is recorded in the Job's Trace instead.
5. **A bare `bool` leans heavily to `false`, so never build a filter out of one.** Asked
   `Is this transcript a real request?`, laya answered `false` for every real capture probed —
   the PRD's own `Remember that milk is cheaper at store A than store B` included. Inverting the
   polarity (`Is this transcript gibberish?`) then caught only literal syllable noise
   (`asdkfj alskdjf qwoieu zzz kkkk`) and waved through filler and repeated words. A forced
   `choice` with explicit criteria has no such bias: `{"request": "a real request about something
   to do or remember", "noise": "unintelligible noise that means nothing"}` under
   `Which is this transcript?` scored 7/8 on the probe set and never rejected a real capture.
   That is the question Blurt's gate asks.
6. **Several similar questions in one call degrade the answers.** Four near-identical booleans
   in one payload came back incoherent (real text `false`, noise `true`). Ask one question per
   call unless the questions are genuinely unrelated.