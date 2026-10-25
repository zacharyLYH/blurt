# Blurt

Status: aligned design. Supersedes PRD2.

Blurt is a self-hosted, voice-first reminder app. The user blurts something; Blurt turns it into a Reminder, a Memory, or both.

**Thesis:** voice removes friction from capturing the unstructured parts of life. Blurt links reminders to relevant Memory and includes useful context in notifications.

**Time model:** human time is the source of truth. Each Reminder carries its own local date/time and IANA zone. UTC is a derived value for scheduling.

## Core model

- **Job**: one conversation from capture through clarification. It can create many Memories and at most one Reminder.
- **Reminder**: something to interrupt the user about, once or repeatedly.
- **Memory**: useful context for future Reminders. It can be replaced or deleted.

No Profile table, pinned set, ontology, or weekly rewrite. Memory is the only remembered context. Questionnaire answers are Memory. Tags help search; they are not classes.

## Product rules

1. One ingress: voice or text for capture, clarification, and correction.
2. Save a Reminder only when its intended local time and zone are clear.
3. The human-time rule is authoritative. UTC firing times are derived and rebuildable.
4. The server computes UTC and recurrence. The LLM returns human time and a zone, never UTC.
5. The scheduler compares derived UTC firing times with the server UTC clock. Device clock does not control firing.
6. When the agent finds a conflict or ambiguity, it asks. The question and answer live in the Job Conversation. Keep the existing Reminder active until the user confirms a change.
7. Each run either asks or commits. A question commits nothing; a question-free run commits the items that passed.
8. A correction replaces the old record with a new one. Memory can also be deleted.
9. BM25 is phase 1 retrieval. Add vector search only if recall needs it.

## Examples

- “Remind me to ask my mechanic about the screeching noise when I brake.”
- “Remind me to buy glue on the way home from work.”
- “Remind me to buy a gift one week before John's birthday every year.”
- “Remind me of dinner with my parents this Sunday.”
- “Remind me of the GPU Black Friday sale on November 1.”
- “Remember that milk is cheaper at store A than store B.”

## Litmus tests

1. “Take the trash out at 4pm.” A clear Reminder; no clarification.
2. “Fetch kids from school at 5pm,” then “Buy glue on the way home from school.” The glue Reminder anchors to the kids Reminder from a different Job; the linkage (`RelativeToReminderId`) stays unbuilt, and the time resolves from the capture.
3. “I like this noodle shop in Chongqing,” then “Book a flight for Chongqing tonight at 8pm.” Surface the noodle-shop Memory with the flight Reminder.
4. “I need to use my AirAsia points by November,” then “Book a flight after dinner at 8pm.” Surface AirAsia Memory despite different wording.
5. A four-item capture produces the split question and zero rows until it is answered, then one Reminder plus the Memories it names.
6. One voice note can create one Reminder and many Memories.

## Time and recurrence

### One-off reminders

Store local date/time and IANA zone, then derive `EmitAtUtc`. “8pm Tokyo time” means 20:00 in `Asia/Tokyo` on the stated date.

Use an explicit zone when named. Otherwise infer it from the user's current zone, nearby Reminders, and relevant Memory. A trip can supply context for new Reminders; it never moves existing ones. If the intended zone is still unclear, ask.

If DST skips the local time, shift forward through the gap. If the time occurs twice, use the first occurrence.

### Recurring reminders

The rule stores only the repeat pattern and local time:

| Repeat | Pattern |
| --- | --- |
| Daily | Time |
| Weekly | Weekday and time |
| Monthly | Day of month and time |
| Yearly | Month, day, and time |

The parent Reminder stores the zone. The server computes the next UTC firing time from the rule and zone every time. Never add a fixed duration to the previous UTC time.

No interval in phase 1. “Every two weeks” is phase 2. Monthly dates that do not exist, such as the 31st in February, need a defined policy before implementation.

### Travel and zones

Travel is context in Memory, not a command that moves existing Reminders. A user might say, “I'm in China Tuesday to Friday.” Store the dates and zone as Memory. New Reminders within that context can use China time. Existing Reminders keep their stored zones. For dates outside the trip, use an explicit zone, a clear anchor in nearby Reminders or Memory, or ask.

Travel context informs new Reminders only. A device-zone change does not modify scheduled Reminders.

## User journey

Prerequisite: an admin creates the initial User row.

1. User enters their name and receives a login email with a PIN.
2. Before the questionnaire, Blurt asks for microphone and notification permission, and re-checks both on every app entry so a revoked permission is surfaced rather than silently assumed.
3. User provides model settings and API key on the setup page.
4. User completes a questionnaire; answers become Memory.
5. With a microphone, user records and sends. Wide screens open the conversation/Home view.
6. If the microphone is unavailable or off, show a conversation thread with text input; do not leave the user stuck.
7. If clarification is needed, send a push notification. Clicking it opens that Job in a familiar chat-style conversation page.
8. The sidebar lists Jobs awaiting clarification. Remove completed Jobs from that list. The conversation transcript remains available through history.
9. Users can answer by text or audio, cancel recording, and correct Blurt through the same interface.

### Conversation UI

Use a familiar chat layout: message history, composer, and a sidebar of open clarification threads. Evaluate [assistant-ui](https://github.com/assistant-ui/assistant-ui): its React primitives support custom chat layouts and thread lists. Blurt owns Job storage and the Go backend; UI framework handles presentation and interaction. Keep the component layer swappable.

## Architecture

- **Client**: React PWA; service workers handle push notifications.
- **Backend**: Go.
- **Storage**: SQLite; FTS5 for BM25; sqlite-vec in phase 2.
- **Frontier LLM**: the model every capture, clarification, correction, and emission goes to.
- **System-One**: self-hosted Kev-4B with temperature scaling; supporting tasks outside consequential decisions.
- **Speech-to-text**: phonon, behind a self-hosted gateway.
- **Embedder**: bge-small-en-v1.5, phase 2.

```text
audio -> STT -> frontier LLM -> Reminder / Memory records
                                  |
                                  +-> clarification in Job Conversation
```

## Data model

Use integer IDs because FTS5 and vec0 need rowids. The vec table is partitioned by `UserId`.

### User

- `UserId`, `Name`, `Email`
- `ModelProvider`, `ModelName`, `ModelEndpoint`, `ModelApiKey` (entered on setup page; encrypted at rest)

### Session

One row per logged-in device. Cookie is opaque and HttpOnly; token never touches JS. Delete the row to revoke.

- `SessionId`, `UserId`, `TokenHash`, `ExpiresAt`
- `PushSubscription` (device-specific JSON)

### Job

One conversation and its lifecycle. Conversation holds the current work and clarification history.

- `JobId`, `UserId`
- `Status`: open, clarifying, complete, failed. Redis Streams carry the processing stage, so no reader infers it from the Conversation.
- `RetryCount` (failed processing attempts; poison-pill limit)
- `Conversation` (append-only structured messages: role, text/audio reference, timestamp, and the device zone the turn was sent from)
- `CreatedAt`, `queued_at` (last publish time, used by the queue sweeper)

A resumed Job re-reads its Conversation from the start, so it proposes what it proposed before; the commit skips a write this Job already committed, which is what keeps one capture from leaving two identical records.

### Job queues

Two Redis Streams route work: `job.transcription` for clips awaiting STT and `job.agent` for turns awaiting the model. Consumer groups own delivery; SQL is the durable record.

- An in-flight attempt has an eight-minute hard timeout. Clarification wait time does not count against it.
- On timeout or transient failure, increment `RetryCount` and publish the next attempt. After three failed attempts set `failed` and keep it visible for recovery.
- A clarification reply appends to Conversation and publishes the Job to `job.agent` again.
- A claimer reclaims messages from dead consumers. A sweeper republishes open Jobs whose SQL commit never reached Redis; no boot scan is needed.

Index: `(Status, queued_at)` for sweeper scans.

### Reminder

Shared record for one-off and recurring reminders.

- `ReminderId`, `UserId`, `Text`
- `Timezone` (IANA; the zone the user meant)
- `LocalDateTime` (one-off only; no offset)
- `EmitAtUtc` (one-off derived firing instant)
- `Status`: resolved, fired, cancelled
- `SourceJobId`
- `RelativeToReminderId` (nullable; keeps the anchor for relative reminders)

The resolved local time already captures any relative offset; do not store a second offset column.

### RecurringReminder

One-to-one extension of Reminder. No duplicate user, text, zone, status, or source fields. No occurrence table in phase 1.

- `ReminderId` (primary key and foreign key to Reminder)
- `Rule` (JSON: frequency, local time, and only the needed calendar parts; optional start date)
- `NextEmitAtUtc` (derived, rebuildable)

The rule contains `time` for daily; `weekday + time` for weekly; `day + time` for monthly; `month + day + time` for yearly. Relative recurring reminders are flattened into their own rule at creation or clarified. Edits are series-level.

### Memory

- `MemoryId`, `UserId`, `Text`
- `Tags` (search terms such as names, brands, categories, jargon)
- `Metadata` (nullable JSON for structured facts such as trip dates and zone)
- `SourceJobId`, `CreatedAt`

No expiry or retired state. To replace a Memory, delete it and create a new one. Delete means delete; no soft-delete lifecycle.

### Fts_memory

BM25 retrieval for phase 1.

```sql
CREATE VIRTUAL TABLE fts_memory USING fts5(
  text,
  tags,
  content='memory',
  content_rowid='MemoryId',
  tokenize='unicode61 remove_diacritics 2'
);
```

Keep the FTS index in sync on Memory create, update, and delete.

### Vec_memory

Phase 2: `MemoryId`, `UserId` (partition), `Embedding`.

### Trace

Append-only diagnostic events, always linked to a Job.

- `TraceId`, `JobId`, `ReminderId` (nullable), `Timestamp` (UTC), `Stage`, `Detail`

Trace transcription, model turns, PostAgentCheck results, clarification transitions, and notification attempts. Store model, duration, outcome, and useful diagnostics in `Detail`; never log secrets. Order by `Timestamp`, then `TraceId` for ties.

Each notification attempt has a Trace row with its `ReminderId`. Its `TraceId` is the notification ID in the push link, identifying one firing even for recurring reminders.

### NotificationFeedback

One report per notification. The linked Trace identifies the Reminder and owning user.

- `NotificationTraceId` (primary key and foreign key to Trace)
- `FailureMode`
- `Comment` (optional)
- `CreatedAt`

## Pipeline

### Ingress and transcription

Audio is mono 16 kHz PCM WAV, around 32 KB/s. Upload one binary blob on send and keep it temporarily for retry. Save the Job and return; STT and agent run asynchronously.

Capture pins the browser audio graph to 16 kHz mono, then rewrites whatever the browser encoded — WebM/Opus on Chrome, AAC on Safari — into mono 16 kHz PCM WAV, which is the only shape the transcription gateway accepts. The server re-verifies that shape from the blob header, and the blob is capped at one minute. phonon is called over the gateway's own route, not an OpenAI-compatible one: one JSON body carrying a base64 WAV data URI, `{"output": {"text": "..."}}` back. The bearer key is required; a missing or wrong key is a 401 and a permanent failure.

Convert speech to English. If STT fails or the clip is not legible, reply in the Job Conversation: “I couldn't process that. Please say it again or type it.” Send a push notification. System-One may provide transcript-quality signals or alternate entity spellings; the frontier LLM decides whether the user needs to repeat.

### Agent loop

The frontier LLM is the only model. It reads the full Job Conversation and relevant Reminder/Memory context, then proposes to create, replace, cancel, or delete records, ask the user, or clarify the request. A trip statement creates or updates Memory like any other fact.

Allow multiple model turns. Start with a configurable budget of 5 turns and an eight-minute hard timeout; stop when resolved or when either budget is reached. Tune from Trace data. Failed Jobs remain visible for recovery.

Inputs: Job Conversation, `current_tz`, relevant Reminder/Memory results, and the previous turn's
refusals. `current_tz` is the device's IANA zone when no stronger anchor exists. Neither `now_local`
nor `now_utc` is sent: handed a clock, a constrained decoder invents a time from it, so the model
copies the user's own words for when and the server resolves them.

1. Search Reminders from two days before to fourteen days after the target date. If target is unknown, use now. Search Memory with BM25 over the Conversation and tags; include relevant trip Memory by dates and zone.
2. The frontier LLM resolves each item and proposes the records to write. Retrieval is
   prefetched by the server rather than asked for through tools — this model's own tool
   choices measured unreliable, and the results are context either way.
3. Run `PostAgentCheck` on each proposed write before it commits.
4. If a check flags an issue, pass its result into the next AgentLoop turn. The Trace keeps
   it; the Conversation keeps only what the user reads. The LLM may revise the proposal or ask the user. Keep the existing record active until checks pass and the user confirms any change.
5. Commit passing items when the run ends question-free. Which record a turn proposes is read off the fields
   it filled — reminder, memory, or both — so nothing asks the model to name an operation.
6. When time, zone, target, recurrence, or correction is ambiguous, append the question to Conversation, set `clarifying`, and send a push.
7. The Trace keeps the run: the model, the turn count, the refusals and the outcome. The
   Conversation keeps the turns the user reads. A write this Job already committed is
   skipped, so a re-planned turn cannot leave the same record twice.
8. When no clarification remains and all writes finish, set Job to `complete` and remove it from the open-conversation sidebar.

### PostAgentCheck

Checks a proposed write before it commits, and never edits one. Input is the write the
model proposed — its text, its zone, and the user's own words for when — plus the checks
the server can run without a model.

- **Schema lint**: which fields the write filled. A write that names no text is refused, and the
  shape carries at most one Reminder and one Memory per turn. Per Job, at most one Reminder ever:
  a second proposal is refused as `job_reminder_exists` on `kind`.
- **Event lint**: the record's words must all come from the capture, and a Reminder's event must not name a clock. Both fields are asked for in the user's own words, and the time is asked for in the phrase the server resolves.
- **Time lint**: the zone loads as IANA, and the time phrase resolves against the device zone anchor. The model copies the user's words for when and the server resolves them, so a fire time is never the model's arithmetic; a phrase naming no time Blurt can pin is refused rather than guessed.
- **Plan consistency**: one event proposed at two different times is refused rather than dropped, and a record this Job already committed is skipped, so a re-planned turn cannot leave the same record twice.

A refusal returns to the AgentLoop as a code and a field — `time_unresolved` on
`timePhrase`, for instance. The next turn revises against it, and when the turn budget
runs out the Job asks the user.

A record that paraphrases the capture — “call mother” for “call mum” — is the one case the
lints cannot settle, so it goes to the fallback reader: the self-hosted classifier, asked one
choice question with `supported`, `contradicted`, or `unknown`. It is asked only where a lint
abstained, and acted on only when it is decisive: a split answer abstains, and an abstention
leaves the lint's refusal standing. `contradicted` is the one refusal no further model turn
can settle, so the Job asks the user at once rather than spending its budget on it.

Deferred: ownership and state lint (a record belongs to the Job that committed it by
construction, and the correction operations the rest of it needs are not built) and recurrence
shape lint.

Guardrails:

- Server computes `EmitAtUtc` and `NextEmitAtUtc`; model emits human time plus zone only.
- Relative reminders inherit the referenced Reminder's zone unless user specifies otherwise.
- Never change an existing Reminder's zone based only on device-zone movement or a travel statement.
- Commit clear items when the run leaves no question; keep ambiguous items in the same conversation for clarification.

### Clarification

Clarification is a normal chat turn, not a separate workflow. The question is written for the
capture — one prompt whose only job is to ask for the half that is missing — and falls back to a
fixed sentence when the model fails or writes something that would name a time the user never said. The push opens the existing Job conversation. Show the user's original message, Blurt's question, and the reply composer. User can answer with text or audio; append it to Conversation and resume the agent.

Ask when:

- intent or correction target is unclear
- a relative time has no clear anchor or has multiple candidates
- recurrence may be intended but was not explicit
- local time or zone has multiple plausible meanings
- trip dates or destination zone are unclear
- a Memory referent is missing
- a correction would change an existing Reminder's time or zone
- audio or transcript cannot be understood

Batch blocking questions in one turn where possible, within the shape's cardinality: one Reminder and one
Memory per turn, and at most one Reminder per Job. When a capture holds several independent items, ask the
user to take them one at a time, mirroring batch-one-turn. Nudge after one hour. If a linked Reminder fires
or is cancelled while waiting, ask again or close the stale request safely.

### Correction

Same agent and Conversation. A notification may include a ReminderId or MemoryId. Otherwise search and confirm if multiple records match. Never guess silently.

- Replace a Reminder only after the user confirms the new meaning.
- Cancel a Reminder by status; do not delete its history.
- Replace Memory by deleting the old row and creating a new one; user-requested Memory deletion is hard delete.
- A correction to a recurring Reminder keeps its Rule unless the user changes it.

### Emission

Query resolved one-off `EmitAtUtc` and recurring `NextEmitAtUtc` once a second. Send due notifications sequentially, two seconds apart.

1. Search Memory by Reminder text and tags.
2. The frontier LLM picks relevant context and writes the notification.
3. Render intended local time with a short zone label.
4. Before sending, persist Reminder state and a notification-attempt Trace with `ReminderId`. One-off becomes `fired`; recurring Reminder computes its next UTC firing value from its Rule and zone.
5. Send push with the attempt TraceId as `notificationTraceId`; log delivery outcome in Trace. PWA delivery is best effort.

Do not embed Reminders; fetch by time window.

### Notification feedback

When a user opens a notification, offer a short “What went wrong?” form. Submit one selected failure mode and an optional comment:

```http
POST /api/notifications/{notificationTraceId}/feedback
```

Failure modes: wrong Reminder, wrong date/time/zone, wrong or irrelevant context, duplicate/too many notifications, other. Verify the signed-in user owns the linked Reminder. Store one response per notification in `NotificationFeedback`.

## Retrieval

### Reminder window and due queue

Fetch one-off reminders by `EmitAtUtc` and recurring reminders by `NextEmitAtUtc`, ordered by the derived instant. Use the window for relative-time resolution and corrections: from two days before to fourteen days after the target, or now when the target is unknown.

### Memory

```sql
SELECT m.* FROM fts_memory JOIN Memory m ON m.MemoryId = fts_memory.rowid
WHERE fts_memory MATCH ? AND m.UserId = ?
ORDER BY bm25(fts_memory) LIMIT 10
```

Search Reminder text; index tags for brands and jargon. Phase 2 adds vector search and RRF if BM25 misses paraphrases or ASR-mangled names. Trip Memory uses normal retrieval, filtered by its dates when relevant.

## Security

Admin creates users. Login uses an email PIN over SMTP. PIN lasts five minutes, is hashed, single-use, and attempt-capped.

On success, create a Session and opaque HttpOnly, Secure, SameSite=Lax cookie. Session lasts weeks and slides on use. No access/refresh token dance. Delete the Session to revoke it.

Request PIN only without a valid session. SameSite and origin check cover CSRF. Nginx rate-limits ingress. Audio duration cap protects STT and LLM spend.

Permissions are asked on every entry because a browser never reports a revocation on its own. Once a user denies one, no page can prompt again, so the gate surfaces it with what to do about it instead of asking: the user is never trapped, and the text composer remains the fallback.

User enters model provider, model, endpoint if needed, and API key on setup page. Store API key encrypted at rest; no `.env` model configuration.

## DX

- E2E: clean instance with seeded values.
- Mock LLM at HTTP level for specific responses.
- Real free LLM: Qwen 2.5 3B on MLX-LM, local or remote via `ngrok` with static secret.
- Random walker generates schedules; use an injectable clock.

## Known gaps

1. PWA push is best effort; a miss drops the notification.
2. Audio cannot upload after app closes; user must wait for sync before leaving.
3. Recurrence phase 1: daily, weekly, monthly, yearly. No intervals, full RRULE, per-occurrence skip, or relative recurrence linkage.
4. Time zone inference can require clarification when no clear anchor exists.
5. BM25 only; paraphrase and ASR-mangled names may miss until vectors land.
6. No eval yet; Trace helps debug but does not measure quality.
7. Correction ops (replace, cancel, delete), `RelativeToReminderId`, and trip-Memory `Metadata`
   are not built. A completed Job never revisits its own Reminder; a correction is future work
   through its own channel.
8. One event reworded so that both texts are fallback-supported in a single run can slip past
   the conflict key. The at-most-one-Reminder guardrail bounds the harm.

## Deferred

Deliberately not built in this slice: record replace/cancel/delete ops, `RelativeToReminderId`,
recurring reminders, trip-Memory `Metadata`, per-turn model/check Trace rows (one aggregate Trace
row per step today), failed-recovery UI, and dump/split.

## System-One supporting work

The frontier LLM alone interprets intent, chooses actions, resolves time and zone, selects evidence, and writes records. System-One supports it without deciding those outcomes:

- Suggest alternate transcript spellings for names and places; the frontier LLM sees the original and suggestion.
- Expand Memory search queries to improve BM25 recall; the frontier LLM chooses which results matter.
- Generate short conversation titles and sidebar previews from completed Jobs.
- Summarize Trace batches, cluster recurring failures, and draft eval cases for review.

Measure transcript correction rate, retrieval recall, title usefulness, and debugging time saved. Keep the main decision path frontier-driven.

## Failure modes

Ranked by likely harm to user trust and usability. Recovery ideas are deferred.

1. **Wrong action or target**: Blurt creates, changes, cancels, or deletes the wrong Reminder or Memory.
2. **Wrong time or zone**: a Reminder fires at the wrong local date or time.
3. **Missed or late notification**: a resolved Reminder does not reach the user when expected.
4. **Misheard capture**: transcription changes a name, place, number, negation, or intended action.
5. **False or misleading context**: notification surfaces Memory that is incorrect or about the wrong thing.
6. **Correction not applied**: old Reminder remains active or the confirmed replacement is wrong.
7. **Clarification gets stuck**: reply is lost, Job does not resume, or an open question disappears.
8. **Important context missing**: relevant Memory is not found or not surfaced.
9. **Duplicate or excessive notifications**: same Reminder fires more than once or creates interruption fatigue.
10. **Capture blocked**: microphone permission, recording, or upload failure leaves the user unable to blurt.
11. **Unclear notification**: wording does not make the intended action obvious.

## Open questions

1. Monthly dates that do not exist: skip, clamp to month end, or ask?
2. Which assistant-ui elements best fit the PWA, and what is needed for accessibility and mobile layout?
3. What BM25 recall triggers vector search?
4. What indexes are needed?
5. How should one occurrence be skipped or snoozed without an exception table?
6. What if clarification arrives after its linked Reminder time passes?
7. What eval set measures agent and System-One quality?

## Phase 2

1. SQLite durability.
2. Vector retrieval with bge-small-en-v1.5.
3. Floating Reminders and travel inference.
4. Intervals, full RRULE, and per-occurrence exceptions.
