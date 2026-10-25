// Package agent is Blurt's AgentLoop: it reads a Job's Conversation, asks the model
// for one structured record, checks every field the server can check, and either
// commits the record or asks the user for what is missing. Retrieval, the model,
// the durable steps and the push are injected, so the loop itself is nothing but
// the moves one Job makes.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"blurt/internal/config"
	"blurt/internal/failure"
	"blurt/internal/push"
	"blurt/internal/queue"
	"blurt/internal/store"
)

const (
	// maxAttempts is the PRD's poison-pill limit for one stage, sharing the
	// requeue's ceiling so a crash and an in-run failure hit the same wall.
	maxAttempts = store.MaxAttempts
	// attemptTimeout bounds one model call. It sits just above the gateway client's
	// own timeout, so a slow host reports as a model failure rather than as a
	// cancelled turn.
	attemptTimeout = 150 * time.Second
	// defaultJobTimeout is the hard wall for one Job's run. It is a ceiling, not a
	// revision gate: when it fires the run lands in the ordinary failure path.
	defaultJobTimeout = 8 * time.Minute
	// windowBefore and windowAfter are the PRD's retrieval window: two days back so a
	// correction can still find its Reminder, a fortnight ahead so a relative one
	// lands inside it.
	windowBefore = 48 * time.Hour
	windowAfter  = 14 * 24 * time.Hour

	// FallbackMessage is what the user reads when the model could not be reached at
	// all. It is the capture worker's sentence on purpose: from the user's side the
	// two failures are the same one.
	FallbackMessage = failure.FallbackMessage
	// reminderReply and memoryReply are what the user reads when a record lands. There is no
	// model sentence to prefer, because the schema has no field for prose: the reply is the
	// server's, so a commit is visible in the Conversation at no model cost.
	reminderReply = "Reminder set."
	memoryReply   = "Noted."
	// defaultQuestion is what the user reads when the model wrote no usable question of its own. One
	// sentence covers every missing field, because naming the missing half is the question writer's job
	// and this is the path where nothing named it.
	defaultQuestion = "What should I remind you about, and when?"
	// maxClarityRounds caps the model-written questions one Job may ask. The next
	// round falls back to defaultQuestion rather than spending another call.
	maxClarityRounds = 2
)

// Notifier is the push fan-out the loop uses when a Job needs the user.
type Notifier interface {
	NotifyUser(ctx context.Context, userID int64, msg push.Message) (push.Result, error)
}

// Store is the durable half of a run: the three steps that touch SQLite, plus the one write
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

// Recall is the context a capture is planned against: what is already scheduled and
// what is worth remembering. It is advisory, so a failing read is logged and dropped
// rather than failing the Job — a working capture never depends on a working index.
type Recall interface {
	Nearby(ctx context.Context, userID int64, from, to time.Time) ([]store.Reminder, error)
	Remembered(ctx context.Context, userID int64, query string) ([]store.Memory, error)
}

// AgentLoop runs one Job's Conversation into one Reminder. Every state is one move
// and every collaborator is injected, so the loop's own body is only which move
// comes next.
type AgentLoop struct {
	Store  Store
	Recall Recall
	Model  Model
	Pusher Notifier
	// AgentQueue receives the next message when a run retries.
	AgentQueue queue.Stream
	// ModelName names the model in the Trace; it is not used to call anything.
	ModelName string
	// MaxTurns is the PRD's per-Job model-turn budget.
	MaxTurns int
	// Timeout is the hard wall for one run; zero means defaultJobTimeout.
	Timeout time.Duration
	// AttemptTimeout bounds one model call; zero means attemptTimeout.
	AttemptTimeout time.Duration
	// Now is the clock the model's dates are checked and resolved against. Tests
	// pin it.
	Now func() time.Time
}

// state is one move. A nil state ends the run.
type state func(*run, context.Context) (state, error)

// run is one Job's progress: what the model proposed, what MissingFields could not
// accept of it, and the budget it is spending. It is the whole mutable state of a run.
type run struct {
	loop    *AgentLoop
	job     store.Job
	attempt int
	started time.Time
	turns   int
	writes  []Write
	// reminders and memories are what the turn's accepted writes resolve to, each
	// kind its own record.
	reminders []store.Reminder
	memories  []store.Memory
	missing   []Field
}

// Run drives one Job. Two moves decide the outcome, and each names the next one:
//
//	plan ──▶ check ──┬──▶ commit ──▶ done
//	                 └──▶ question ──▶ done
//
// Every failure lands in the one place that decides retry or fail, so a state only
// has to say what it is.
func (l *AgentLoop) Run(ctx context.Context, job store.Job, attempt int) {
	ctx, cancel := context.WithTimeout(ctx, l.jobTimeout())
	defer cancel()
	r := &run{loop: l, job: job, attempt: attempt, started: l.now()}
	for next := (*run).plan; next != nil; {
		step, err := next(r, ctx)
		if err != nil {
			r.failed(ctx, err)
			return
		}
		next = step
	}
}

// plan asks the model for the one record this capture becomes.
func (r *run) plan(ctx context.Context) (state, error) {
	r.turns++
	turn, err := r.loop.propose(ctx, r.input(ctx))
	if err != nil {
		return nil, err
	}
	if len(turn.Writes) == 0 {
		// The model proposed nothing: it has no event it can commit, so every
		// field is the user's to supply.
		r.missing = []Field{FieldEvent, FieldZone, FieldDate, FieldTime}
		return (*run).question, nil
	}
	r.writes = turn.Writes
	return (*run).check, nil
}

// check runs Blurt's deterministic half over every proposed write, and decides the one
// outcome a turn has: whole enough to commit, or a question for the user.
func (r *run) check(ctx context.Context) (state, error) {
	for _, write := range r.writes {
		r.missing = append(r.missing, MissingFields(write, r.started)...)
	}
	if len(r.missing) > 0 {
		return (*run).question, nil
	}
	return (*run).commit, nil
}

// commit stores the records the accepted writes resolve to and closes the Job. Both kinds
// are committed in one step, which is why a bad one sends the whole turn to a question
// instead: an ask exit commits nothing at all.
func (r *run) commit(ctx context.Context) (state, error) {
	for _, write := range r.writes {
		if write.Kind == KindMemory {
			r.memories = append(r.memories, store.Memory{Text: write.Text})
			continue
		}
		loc, err := time.LoadLocation(write.Zone)
		if err != nil {
			// MissingFields accepted this zone, so this is the zone database
			// changing under us: a failure, not a question.
			return nil, fmt.Errorf("agent: load zone %q: %w", write.Zone, err)
		}
		reminder, ok := reminderFor(write, r.started.In(loc), loc)
		if !ok {
			// A pattern the calendar cannot place is the user's to say.
			r.missing = []Field{FieldDate}
			return (*run).question, nil
		}
		r.reminders = append(r.reminders, reminder)
	}
	if err := r.step(ctx, store.JobComplete, r.replyFor(), r.reminders, r.memories); err != nil {
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
func (r *run) tagMemories(parent context.Context) {
	if len(r.memories) == 0 {
		return
	}
	// One deadline covers the tagger's calls, which is at most one per committed Memory.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), r.loop.proposeTimeout())
	defer cancel()
	for i := range r.memories {
		tags, err := r.loop.Model.Tags(ctx, r.memories[i].Text)
		if err != nil {
			slog.Warn("agent memory tags", "job_id", r.job.JobID, "err", err)
			continue
		}
		if len(tags) == 0 {
			continue
		}
		if err := r.loop.Store.TagMemory(ctx, r.job.JobID, r.memories[i].Text, strings.Join(tags, ",")); err != nil {
			slog.Warn("agent memory tag write", "job_id", r.job.JobID, "err", err)
		}
	}
}

// replyFor is the sentence the user reads. Committing is reached only with something
// accepted, so a Reminder is the answer unless the turn carried a fact alone.
func (r *run) replyFor() string {
	if len(r.reminders) > 0 {
		return reminderReply
	}
	return memoryReply
}

// question keeps the Job open on the question the user has to answer, and pushes.
// It commits no records: an ask exit is exclusive, so the Job holds only the question.
func (r *run) question(ctx context.Context) (state, error) {
	question := r.questionText(ctx)
	if err := r.step(ctx, store.JobClarifying, question, nil, nil); err != nil {
		return nil, err
	}
	// The push only saves the user a look: the Job already says the same thing.
	r.loop.notify(ctx, r.job, "Blurt needs one thing", question)
	return nil, nil
}

// maxTurns and now read the loop's budget and clock, with config's default for a loop
// built without a turn budget.
func (l *AgentLoop) maxTurns() int {
	if l.MaxTurns < 1 {
		return config.DefaultMaxTurns
	}
	return l.MaxTurns
}

func (l *AgentLoop) jobTimeout() time.Duration {
	if l.Timeout <= 0 {
		return defaultJobTimeout
	}
	return l.Timeout
}

// proposeTimeout is the model call's own wall; a slow host reports as a model
// failure rather than pinning the worker.
func (l *AgentLoop) proposeTimeout() time.Duration {
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
