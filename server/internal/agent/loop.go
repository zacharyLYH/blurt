// Package agent is Blurt's AgentLoop: one Job's Conversation in, records or a question out.
// Retrieval, the model, the durable steps and the push are injected, so the loop's own body is
// nothing but the moves one Job makes.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"blurt/internal/failure"
	"blurt/internal/push"
	"blurt/internal/queue"
	"blurt/internal/store"
)

const (
	// maxAttempts is the PRD's poison-pill limit for one stage, sharing the
	// requeue's ceiling so a crash and an in-run failure hit the same wall.
	maxAttempts = store.MaxAttempts
	// attemptTimeout bounds one model call. It sits just above the transport's own
	// timeout, so a slow provider reports as a model failure rather than as a
	// cancelled turn. A hosted reasoning model answers in tens of seconds, and a
	// free tier can be slower still.
	attemptTimeout = 330 * time.Second
	// defaultJobTimeout is the hard wall for one Job's run. It has to hold a whole agent
	// loop and the finalize call behind it; it is a ceiling, not a revision gate.
	defaultJobTimeout = 20 * time.Minute
	// maxLoopCalls is the agent loop's budget: this many model calls, and then the one
	// finalize call behind submit_plan. Tool calls are free within it, however many the
	// model makes in one response.
	maxLoopCalls = 10
	// maxFaults is how many lint faults one run may see. The first buys a fresh budget and
	// the faults themselves; the second is an error, because a model that cannot place the
	// capture twice will not place it on a third call either.
	maxFaults = 1

	// FallbackMessage is what the user reads when the model could not be reached at
	// all. It is the capture worker's sentence on purpose: from the user's side the
	// two failures are the same one.
	FallbackMessage = failure.FallbackMessage
	// reminderReply and memoryReply are what the user reads when a record lands. There is no
	// model sentence to prefer, because the schema has no field for prose: the reply is the
	// server's, so a commit is visible in the Conversation at no model cost.
	reminderReply = "Reminder set."
	memoryReply   = "Noted."
	updateReply   = "Reminder updated."
	// cancelReply is the one sentence for the one answer that writes nothing. A call-off is the
	// app's own write, so the reply is the app's own button, and it is fixed for the reason
	// every reply is: the schema has no field for prose, and a model sentence about a row
	// nothing touched is the one place a wrong claim would cost nothing to make.
	cancelReply = "I think you asked to cancel a reminder? Blurt doesn't cancel reminders on your behalf, but you may go ahead and delete it on the app yourself!"
	// defaultQuestion is what the user reads when no model wrote a usable question of its own.
	defaultQuestion = "What should I remind you about, and when?"
	// maxQuestions caps the questions one Job may ask. The next round falls back
	// to defaultQuestion rather than spending another call.
	maxQuestions = 2
	// verdictOK is the one screening answer that lets the turn reach the planner.
	verdictOK = "ok"
)

// Store is the durable half of a run: the three steps that touch SQLite, plus the one record
// the tagger makes after the run is over.
type Store interface {
	CommitAgentStep(ctx context.Context, jobID int64, step store.AgentStep) (store.Job, error)
	// TagMemory sets one committed Memory's search features. It is separate from
	// CommitAgentStep because the features are generated behind the commit rather than
	// inside it.
	TagMemory(ctx context.Context, jobID int64, text, tags string) error
	Retry(ctx context.Context, stage string, jobID int64, attempt int, detail map[string]any) error
	Fail(ctx context.Context, stage string, jobID int64, attempt int, fallback string, detail map[string]any) error
}

// Recall is the two lookups the agent's tools run, each scoped by the Job's owner, and the
// memory read the context block is built from. It is advisory, so a failing read is logged
// and dropped rather than failing the Job — a working capture never depends on a working
// index.
type Recall interface {
	FindReminders(ctx context.Context, userID int64, query string, limit int) ([]store.Reminder, error)
	FindMemories(ctx context.Context, userID int64, query string, limit int) ([]store.Memory, error)
	// FindReminder reads one of the user's own rows by id: what a change names, and what the
	// pre-check confirms is still there to change.
	FindReminder(ctx context.Context, userID, reminderID int64) (store.Reminder, bool, error)
}

// Stage is one model stage: the model it asks, and the name the Trace reports it under. A
// stage with no model of its own rides the planner's, so a single-provider deployment — and
// every test double — wires one model and nothing else.
type Stage struct {
	Model Model
	Name  string
}

// AgentLoop runs one Job's Conversation into records: the prescreen, the agent loop, the
// lint and the commit. Every state is one move and every collaborator is injected, so the
// loop's own body is only which move comes next.
type AgentLoop struct {
	Store  Store
	Recall Recall
	// Planner is the core turn, and the fallback for the two cheap stages below, which ask
	// the home gateway each under its own model id.
	Planner   Stage
	Prescreen Stage
	Tags      Stage
	Pusher    push.Notifier
	// AgentQueue receives the next message when a run retries.
	AgentQueue queue.Stream
	// Timeout is the hard wall for one run; zero means defaultJobTimeout.
	Timeout time.Duration
	// AttemptTimeout bounds one model call; zero means attemptTimeout.
	AttemptTimeout time.Duration
	// Now is the clock the model's dates are checked and resolved against. Tests
	// pin it.
	Now func() time.Time
}

// Step runs one attempt as a queue.Worker step: the loop runs, and the message is ackable
// unless the runtime was cancelled, in which case it stays pending for the claimer.
func (l *AgentLoop) Step(ctx context.Context, job store.Job, attempt int) bool {
	l.Run(ctx, job, attempt)
	return ctx.Err() == nil
}

// state is one move. A nil state ends the run.
type state func(*jobRun, context.Context) (state, error)

// jobRun is one Job's progress: what the prescreen said, what the agent answered, what the
// linter would not accept, and the budget it is spending. It is the whole mutable state of
// a run.
type jobRun struct {
	loop      *AgentLoop
	job       store.Job
	attempt   int
	started   time.Time
	turns     int
	faults    int
	model     Model
	modelName string
	// prescreenVerdict and answer are the two model answers this run got, kept whole so the
	// Trace can show what each stage decided.
	prescreenVerdict Verdict
	answer           Answer
	records          []Record
	// lint is every fault the server raised against this run's answers, in the model's own
	// terms: what the Trace keeps so a refused answer is readable after the fact.
	lint []string
	// reminders and memories are what the answer resolved to: the rows it adds, the rows it
	// rewrites, and the facts worth keeping.
	reminders []store.Reminder
	memories  []store.Memory
	// looked is what this run's own lookups answered. With the user's own turns it is the
	// only evidence a question may quote a clock from: a time the model supplied itself
	// would reach the user as theirs.
	looked []string
	// found is what those lookups printed, which is the whole of the whitelist: a change may
	// name one of them and nothing else, and an update is filled from the row it names rather
	// than read again.
	found []store.Reminder
	// updates are the rows this run rewrites in place, each carrying its own id.
	updates []store.Reminder
	// stale is set when a rewrite found its row gone between the lookup and the commit.
	stale bool
}

// Run drives one Job. Three moves decide the outcome, and each names the next one:
//
//	prescreen ──▶ agent ──┬──▶ commit ──▶ done
//	   │                  └──▶ question ──▶ done
//	   └──▶ failed (a verdict that is not ok)
//
// The agent move is a loop of its own: the model calls tools, submits a plan, and the strict
// call behind that plan is linted. Every failure lands in the one place that decides retry or
// fail. The commit move asks instead of storing a pattern the calendar cannot place.
func (l *AgentLoop) Run(ctx context.Context, job store.Job, attempt int) {
	ctx, cancel := context.WithTimeout(ctx, l.jobTimeout())
	defer cancel()
	r := &jobRun{
		loop: l, job: job, attempt: attempt, started: l.now(),
		model: l.Planner.Model, modelName: l.Planner.Name,
	}
	for next := (*jobRun).prescreen; next != nil; {
		step, err := next(r, ctx)
		if err != nil {
			r.failed(ctx, err)
			return
		}
		next = step
	}
}

// screen is the first move on every turn. A verdict that is not ok closes the Job
// with the fallback reply and the verdict in the Trace, exactly as an illegible
// transcript does, and costs one cheap call. An answer is screened like any other
// capture: a fragment needs no relevance judgment here, only a meaning.
func (r *jobRun) prescreen(ctx context.Context) (state, error) {
	verdict, err := r.loop.prescreenModel(ctx, latestCapture(r.job))
	if err != nil {
		return nil, err
	}
	r.prescreenVerdict = verdict
	if verdict.Outcome == verdictOK {
		return (*jobRun).agent, nil
	}
	// A refusal is not an error: retrying would ask the same model the same
	// question. The reply is the fallback sentence an illegible transcript gets.
	r.loop.fail(ctx, r.job, r.attempt, verdict.Outcome, r.trace(verdict.Outcome))
	return nil, nil
}

// agent is the model's turn: the tool loop, the one strict call behind submit_plan, and the
// server's own lint on the answer. A first fault buys the loop a fresh budget with the faults in
// hand; a second is an error. A question passes lint and ends the agent: asking is a legitimate
// answer, and the model is the one that knows what the capture left open.
func (r *jobRun) agent(ctx context.Context) (state, error) {
	in := r.input(ctx)
	var sent []Message
	for {
		answer, conversation, fault, err := r.runLoop(ctx, in, sent)
		if err != nil {
			return nil, err
		}
		sent = conversation
		if fault == "" {
			answer = r.settle(answer)
			if faults := lint(answer, r.started, r.found); len(faults) > 0 {
				fault = note(faults)
			}
		}
		if fault == "" {
			r.answer = answer
			if answer.Kind == AnswerQuestion {
				return (*jobRun).question, nil
			}
			return (*jobRun).commit, nil
		}
		r.lint = append(r.lint, fault)
		r.faults++
		if r.faults > maxFaults {
			return nil, &RefusedError{Message: fault}
		}
		// The retry is this whole conversation plus the faults: the model reads what the
		// server refused and places the capture again, on a budget of its own.
		sent = append(sent, Message{Role: "user", Content: fault})
	}
}

// settle fills what the model left unsaid, so it never has to copy a field the server already
// holds: a change is a delta, and the rest is the row's. The row is the one this run's own
// lookup printed, so nothing is read twice; a row it never saw is the whitelist's to refuse
// before any field is checked.
func (r *jobRun) settle(answer Answer) Answer {
	for i := range answer.Ops {
		op := &answer.Ops[i]
		row, _ := printed(r.found, op.ReminderID)
		settleRecord(&op.Record, row, deviceZone(r.job), r.started)
	}
	return answer
}

// settleRecord is the one fill: the row an op changes, or the user's own zone for a create. An
// empty field means "as it was", and the day token becomes a date here — once, after the zone is
// settled — so the calendar is read in the zone the row rings in.
func settleRecord(record *Record, row store.Reminder, zone string, now time.Time) {
	if record.Zone == "" {
		record.Zone = pick(row.Timezone, zone)
	}
	if record.Date == "" && record.Day != "" {
		if at, ok := resolveDay(record.Day, inZone(now, record.Zone)); ok {
			record.Date = at.Format("2006-01-02")
		}
	}
	if row.ReminderID == 0 {
		return
	}
	date, clock := rowWhen(row)
	record.Text = pick(record.Text, row.Text)
	record.Time = pick(record.Time, clock)
	record.Recurrence = pick(record.Recurrence, rowRecurrence(row))
	if record.Date == "" && record.Day == "" {
		record.Day, record.Date = date, date
	}
}

// rowWhen is the day and clock a row already carries: a one-off's own, a repeat's next
// occurrence in the row's zone. It is the one reader of a stored moment, so the day a change
// copies and the day a tool result prints cannot disagree.
func rowWhen(row store.Reminder) (string, string) {
	when := row.LocalDateTime
	if when == "" {
		when = wallClock(row.NextEmitAtUtc, row.Timezone)
	}
	if date, clock, ok := strings.Cut(when, "T"); ok {
		return date, clock
	}
	return "", ""
}

// rowRecurrence is the repeat a row carries, or none for a one-off.
func rowRecurrence(row store.Reminder) string {
	if row.Rule == nil {
		return RecurrenceNone
	}
	return row.Rule.Frequency
}

// runLoop is one agent loop and the strict call behind it: the model calls tools until it
// calls submit_plan, which triggers the one schema-constrained call that writes the answer.
// It answers the decoded answer, the conversation it ended with, and the loop's own
// complaint — the model spent the budget without submitting, or its answer was not what the
// schema means — which is empty when there is an answer to lint. A provider failure is an
// error rather than a fault: a retry with backoff is what fixes that, not another loop.
func (r *jobRun) runLoop(ctx context.Context, in PlannerInput, sent []Message) (Answer, []Message, string, error) {
	for left := maxLoopCalls; left > 0; left-- {
		reply, err := r.loop.agentModel(ctx, r.model, in, sent)
		if err != nil {
			return Answer{}, sent, "", err
		}
		r.turns++
		sent = append(sent, reply)
		submitted := false
		for _, call := range reply.ToolCalls {
			result, done := r.runTool(ctx, call, left-1)
			sent = append(sent, toolResult(call, result))
			submitted = submitted || done
		}
		if !submitted {
			continue
		}
		answer, err := r.loop.finalizeModel(ctx, r.model, in, sent)
		r.turns++
		var bad schemaError
		if err != nil && !errors.As(err, &bad) {
			return Answer{}, sent, "", err
		}
		if err != nil {
			// The model's own answer was not what the schema means. It is a fault like any
			// other, so it reaches the model in the linter's voice.
			return Answer{}, sent, note([]string{err.Error()}), nil
		}
		return answer, sent, "", nil
	}
	return Answer{}, sent, "the loop spent its calls without submit_plan", nil
}

// commit is the last move: the accepted records become the rows Blurt stores, and both kinds
// land in one step, which is why a bad one asks instead of committing half a turn. Lint
// accepted every field already, so a pattern the calendar cannot place is the one thing left,
// and that is the user's to say. A change rewrites the row it names instead of adding one; a
// call-off writes nothing at all, because the app owns that write.
func (r *jobRun) commit(ctx context.Context) (state, error) {
	for _, op := range r.answer.Ops {
		// A call-off writes nothing: the app owns that write, and the sentence says where.
		if op.Intent == IntentCancel {
			continue
		}
		record := op.Record
		loc, err := time.LoadLocation(record.Zone)
		if err != nil {
			// Lint accepted this zone, so this is the zone database
			// changing under us: a failure, not a question.
			return nil, fmt.Errorf("agent: load zone %q: %w", record.Zone, err)
		}
		reminder, ok := ReminderFor(record, r.started.In(loc), loc)
		if !ok {
			// The question is the record of it: the Job keeps what the user reads, and
			// lint has already accepted every field this pattern was built from.
			return (*jobRun).question, nil
		}
		if op.Intent == IntentUpdate {
			// The row the op named, rewritten where it stands: its id, its status and
			// the Job that made it all stay, so nothing that points at it goes stale.
			reminder.ReminderID = op.ReminderID
			r.updates = append(r.updates, reminder)
			continue
		}
		r.reminders = append(r.reminders, reminder)
	}
	for _, text := range r.answer.Memories {
		r.memories = append(r.memories, store.Memory{Text: text})
	}
	if err := r.saveStep(ctx, store.JobComplete, r.replyFor(), r.reminders, r.updates, r.memories); err != nil {
		// A row that fired or was called off between the lookup and this commit has
		// nowhere to put the change: the turn fails rather than inventing a row for it.
		r.stale = errors.Is(err, store.ErrReminderStale)
		return nil, err
	}
	// The records and the reply are durable before the features are even asked for, so a slow
	// tagger never holds a Job the user has already read.
	go r.tagMemories(ctx)
	return nil, nil
}

// tagMemories fills each committed Memory's search features, so a later capture worded
// nothing like the fact can still find it. It runs behind the commit rather than inside it,
// and on its own context rather than the run's, which ends with the run. It is best effort,
// like recall: a call that fails leaves the row untagged rather than failing a capture the
// user already made. It runs at commit rather than at accept so a Memory that dies with an
// ask exit costs no call.
func (r *jobRun) tagMemories(parent context.Context) {
	if len(r.memories) == 0 {
		return
	}
	// One deadline covers the tagger's calls, which is at most one per committed Memory.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), r.loop.modelTimeout())
	defer cancel()
	tagger := r.loop.model(r.loop.Tags)
	for i := range r.memories {
		tags, err := tagger.Tags(ctx, r.memories[i].Text)
		if err != nil {
			slog.Warn("agent memory tags", "job_id", r.job.JobID, "err", err)
			continue
		}
		if len(tags) == 0 {
			continue
		}
		if err := r.loop.Store.TagMemory(ctx, r.job.JobID, r.memories[i].Text, strings.Join(tags, ",")); err != nil {
			slog.Warn("agent memory tag record", "job_id", r.job.JobID, "err", err)
		}
	}
}

// replyFor is the sentence the user reads: the call-off's fixed one when the turn asked for
// anything to be called off, because it says where the app does that write; then the change's,
// the reminder's, and last the fact's. It speaks for the turn, not for every row in it.
func (r *jobRun) replyFor() string {
	calledOff := false
	for _, op := range r.answer.Ops {
		calledOff = calledOff || op.Intent == IntentCancel
	}
	switch {
	case calledOff:
		return cancelReply
	case len(r.updates) > 0:
		return updateReply
	case len(r.reminders) > 0:
		return reminderReply
	}
	return memoryReply
}

// evidence is the user's own words the planner model read the row from. Every op in one turn
// shares it.
func (r *jobRun) evidence() string {
	return r.answer.Evidence
}

// question keeps the Job open on the question the user has to answer, and pushes.
// It commits no records: an ask exit is exclusive, so the Job holds only the question.
func (r *jobRun) question(ctx context.Context) (state, error) {
	question := r.questionText()
	if err := r.saveStep(ctx, store.JobClarifying, question, nil, nil, nil); err != nil {
		return nil, err
	}
	// The push only saves the user a look: the Job already says the same thing.
	failure.Notify(ctx, r.loop.Pusher, r.job, "Blurt needs one thing", question)
	return nil, nil
}

func (l *AgentLoop) jobTimeout() time.Duration {
	if l.Timeout <= 0 {
		return defaultJobTimeout
	}
	return l.Timeout
}

// modelTimeout is the model call's own wall; a slow provider reports as a model
// failure rather than pinning the worker.
func (l *AgentLoop) modelTimeout() time.Duration {
	if l.AttemptTimeout > 0 {
		return l.AttemptTimeout
	}
	return attemptTimeout
}

func (l *AgentLoop) now() time.Time {
	if l.Now == nil {
		return time.Now()
	}
	return l.Now()
}

// model is the model a stage runs on: its own when the stack split the stages, the planner's
// otherwise.
func (l *AgentLoop) model(s Stage) Model {
	if s.Model == nil {
		return l.Planner.Model
	}
	return s.Model
}

// latestCapture is the newest user turn's words: what the screen reads and what a
// continuation's answer is.
func latestCapture(job store.Job) string {
	for i := len(job.Conversation) - 1; i >= 0; i-- {
		turn := job.Conversation[i]
		if turn.Role == "user" && turn.Text != "" {
			return turn.Text
		}
	}
	return ""
}
