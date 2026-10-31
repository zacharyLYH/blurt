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
}

// Resolver resolves the model one Job's owner configured. A user brings one provider,
// endpoint, key and model (the User row), so the core transport is built per Job; the
// name it returns is what the Trace records.
type ModelResolver interface {
	ModelFor(ctx context.Context, userID int64) (Model, string, error)
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
	// Resolver builds the Job owner's own model. When set it wins; Planner.Model is the
	// fixed double a test (or the fake stack) supplies instead.
	Resolver ModelResolver
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
	// reminders and memories are what the answer's accepted records resolve to, each
	// kind its own row.
	reminders []store.Reminder
	memories  []store.Memory
	// looked is what this run's own lookups answered. With the user's own turns it is the
	// only evidence a question may quote a clock from: a time the model supplied itself
	// would reach the user as theirs.
	looked []string
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
	if l.Resolver != nil {
		model, name, err := l.Resolver.ModelFor(ctx, job.UserID)
		if err != nil {
			// A user with no usable model cannot be retried into one: the setup is
			// the missing thing, not the answer.
			r.failed(ctx, err)
			return
		}
		r.model, r.modelName = model, name
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
			if faults := lint(answer, r.started); len(faults) > 0 {
				fault = note(faults)
			}
		}
		if fault == "" {
			r.answer = answer
			if answer.Kind == AnswerQuestion {
				return (*jobRun).question, nil
			}
			r.records = answer.Records
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
// and that is the user's to say.
func (r *jobRun) commit(ctx context.Context) (state, error) {
	for _, record := range r.records {
		if record.Kind == KindMemory {
			r.memories = append(r.memories, store.Memory{Text: record.Text})
			continue
		}
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
		r.reminders = append(r.reminders, reminder)
	}
	if err := r.saveStep(ctx, store.JobComplete, r.replyFor(), r.reminders, r.memories); err != nil {
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

// replyFor is the sentence the user reads. Committing is reached only with something
// accepted, so a Reminder is the answer unless the turn carried a fact alone.
func (r *jobRun) replyFor() string {
	if len(r.reminders) > 0 {
		return reminderReply
	}
	return memoryReply
}

// evidence is the user's own words the planner model read the row from. Every record in
// one turn shares it.
func (r *jobRun) evidence() string {
	for _, record := range r.records {
		if record.Evidence != "" {
			return record.Evidence
		}
	}
	return ""
}

// question keeps the Job open on the question the user has to answer, and pushes.
// It commits no records: an ask exit is exclusive, so the Job holds only the question.
func (r *jobRun) question(ctx context.Context) (state, error) {
	question := r.questionText()
	if err := r.saveStep(ctx, store.JobClarifying, question, nil, nil); err != nil {
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
