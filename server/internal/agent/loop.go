// Package agent is Blurt's AgentLoop: it reads a Job's Conversation, asks the model
// to turn the capture into Reminder and Memory records, checks every proposed record,
// and commits what passes. Retrieval, the model, the durable steps and the push are
// injected, so the loop itself is nothing but the moves one Job makes.
package agent

import (
	"context"
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
	// defaultQuestion is what the user reads when the model wrote no usable question of its own. One
	// sentence covers every refusal, because naming the missing half is the question writer's job and
	// this is the path where nothing named it.
	defaultQuestion = "What should I remind you about, and when?"
)

// Notifier is the push fan-out the loop uses when a Job needs the user.
type Notifier interface {
	NotifyUser(ctx context.Context, userID int64, msg push.Message) (push.Result, error)
}

// Store is the durable half of a run: the three steps that touch SQLite. An
// interface so a test can fault a commit, a retry or a failure without a database.
type Store interface {
	CommitAgentStep(ctx context.Context, jobID int64, step store.AgentStep) (store.Job, error)
	Retry(ctx context.Context, stage string, jobID int64, attempt int, detail map[string]any) error
	Fail(ctx context.Context, stage string, jobID int64, attempt int, fallback string, detail map[string]any) error
	// HasReminder reports whether this Job already committed a Reminder in an
	// earlier run. An ask exit commits none, so a committed one means a run that
	// completed; failed-job recovery can still re-run the Job, where the guardrail
	// has to hold.
	HasReminder(ctx context.Context, jobID int64) (bool, error)
}

// Recall is the context a capture is planned against: what is already scheduled and
// what is worth remembering. It is advisory, so a failing read is logged and dropped
// rather than failing the Job — a working capture never depends on a working index.
type Recall interface {
	Nearby(ctx context.Context, userID int64, from, to time.Time) ([]store.Reminder, error)
	Remembered(ctx context.Context, userID int64, query string) ([]store.Memory, error)
}

// AgentLoop runs one Job's Conversation into records. Every state is one move and
// every collaborator is injected, so the loop's own body is only which move comes
// next.
type AgentLoop struct {
	Store  Store
	Recall Recall
	Model  Model
	// Fallback is the reader a lint hands a write to when it cannot resolve one. Asked only after a
	// deterministic check has abstained, so nil is valid: the lints are the whole of PostAgentCheck.
	Fallback Fallback
	Pusher   Notifier
	// Tagger names the search features of each Memory a run commits. Nil is
	// valid: a Memory then carries no tags, and retrieval rides on its text.
	Tagger Tagger
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
	// Now is the clock the human-time phrases resolve against. Tests pin it.
	Now func() time.Time
}

// state is one move. A nil state ends the run.
type state func(*run, context.Context) (state, error)

// run is one Job's progress: what the checks let through, what the last turn
// refused, and the budget it is spending. It is the whole mutable state of a run.
type run struct {
	loop      *AgentLoop
	job       store.Job
	attempt   int
	started   time.Time
	turns     int
	turn      Turn
	refused   []Check
	reminders []store.Reminder
	memories  []store.Memory
	// accepted is the first proposal this run saw for a kind and text. It tells a
	// repeat from a second Reminder and catches a changed time as a conflict; it
	// never skips a repeat, because the insert's own NOT EXISTS drops the
	// duplicate row.
	accepted map[string]Write
	// passed is every write that checked out, in turn order: what an ask exit
	// drops, and what the next prompt is told survived.
	passed []Write
	// dropped is what a mixed-outcome ask exit discarded, kept for the Trace.
	dropped []Write
	// reminderHeld is the per-Job cardinality fact: true from an earlier run's
	// commit, or from a Reminder this run already accepted.
	reminderHeld bool
	// readings is what the fallback said, in the order it was asked.
	readings []string
}

// Run drives one Job. Each state is one move and each move names the next one, so the
// tree below is the code itself rather than a description of it:
//
//	plan ──▶ check ──▶ decide ──▶ commit ──▶ done
//	          │          └──────▶ ask ─────▶ done
//	          └── revise ──▶ plan
//
// Every failure lands in the one place that decides retry or fail, so a state only has
// to say what it is.
func (l *AgentLoop) Run(ctx context.Context, job store.Job, attempt int) {
	ctx, cancel := context.WithTimeout(ctx, l.jobTimeout())
	defer cancel()
	r := &run{loop: l, job: job, attempt: attempt, started: l.now(), accepted: map[string]Write{}}
	held, err := l.Store.HasReminder(ctx, job.JobID)
	if err != nil {
		r.failed(ctx, err)
		return
	}
	r.reminderHeld = held
	for next := (*run).plan; next != nil; {
		step, err := next(r, ctx)
		if err != nil {
			r.failed(ctx, err)
			return
		}
		next = step
	}
}

// plan asks the model for one turn of records.
func (r *run) plan(ctx context.Context) (state, error) {
	r.turns++
	turn, err := r.loop.propose(ctx, r.input(ctx))
	if err != nil {
		return nil, err
	}
	r.turn = turn
	if len(turn.Writes) == 0 {
		// The model proposed nothing: it gave up, and the refusals it was handed
		// stay, so the question is still about the same items.
		return (*run).decide, nil
	}
	return (*run).check, nil
}

// check accepts the writes the server can prove and collects the rest as refusals, then
// asks revise whether they are worth another turn.
func (r *run) check(ctx context.Context) (state, error) {
	previous := r.refused
	r.refused = nil
	for _, write := range r.turn.Writes {
		if code, field := r.accept(ctx, write); code != "" {
			r.refused = append(r.refused, Check{Write: write, Code: code, Field: field})
		}
	}
	if r.revise(previous) {
		return (*run).plan, nil
	}
	return (*run).decide, nil
}

// revise is the whole revision policy, read rather than looked up: there must be a refusal, it must
// not be the refusal the last turn gave (a repeat means looking again cannot help), it must be one the
// model can answer alone, and the turn budget must have room. Anything else is the user's question;
// the run's hard deadline is what bounds a slow model.
func (r *run) revise(previous []Check) bool {
	return len(r.refused) > 0 &&
		!repeated(previous, r.refused) &&
		!unfixable(r.refused) &&
		r.turns < r.loop.maxTurns()
}

// unfixable reports whether a refusal is one no further turn can settle: the fallback reader has
// already read the write against the user's own words and disagreed, so another turn would only
// repeat it, and the user is the one who has to say what they meant.
func unfixable(refusals []Check) bool {
	for _, refusal := range refusals {
		if refusal.Code == CheckContradicted {
			return true
		}
	}
	return false
}

// repeated reports whether a turn refused what the turn before it refused, by code and by
// field rather than by wording: the model rewording a write that already failed is still
// the same wall, and the same shape twice is what tells the loop to stop spending turns.
func repeated(previous, current []Check) bool {
	if len(previous) != len(current) {
		return false
	}
	refused := make(map[string]bool, len(current))
	for _, check := range current {
		refused[check.Code+"\x00"+check.Field] = true
	}
	for _, check := range previous {
		if !refused[check.Code+"\x00"+check.Field] {
			return false
		}
	}
	return true
}

// decide is the one place a Job's outcome is chosen: a refusal, or a turn that
// committed nothing, is a question for the user, and anything else closes the Job.
// The two outcomes are exclusive per run, so passing writes never outlive a
// refusal; that shape is the model's fault and is kept for the Trace.
func (r *run) decide(ctx context.Context) (state, error) {
	if len(r.refused) > 0 && (len(r.reminders) > 0 || len(r.memories) > 0) {
		r.dropped = r.passed
		slog.Error("agent mixed outcome", "job_id", r.job.JobID,
			"passed", len(r.passed), "refused", len(r.refused))
	}
	if len(r.refused) > 0 || (len(r.reminders) == 0 && len(r.memories) == 0) {
		return (*run).ask, nil
	}
	return (*run).commit, nil
}

// commit closes the Job with the records that passed and the sentence the user reads.
func (r *run) commit(ctx context.Context) (state, error) {
	reply := strings.TrimSpace(r.turn.Say)
	if reply == "" {
		reply = r.replyFor()
	}
	r.tagMemories(ctx)
	return nil, r.step(ctx, store.JobComplete, reply, r.reminders, r.memories)
}

// tagMemories fills each committed Memory's search features. It is best effort,
// like the transcript gate and recall: a tagger that is missing, slow or failing
// leaves the tags empty rather than failing a capture the user already made. It
// runs at commit rather than at accept so a Memory that dies with an ask exit
// never costs a call.
func (r *run) tagMemories(ctx context.Context) {
	if r.loop.Tagger == nil || len(r.memories) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, r.loop.proposeTimeout())
	defer cancel()
	for i := range r.memories {
		tags, err := r.loop.Tagger.Tags(ctx, r.memories[i].Text)
		if err != nil {
			slog.Warn("agent memory tags", "job_id", r.job.JobID, "err", err)
			continue
		}
		r.memories[i].Tags = strings.Join(tags, ",")
	}
}

// ask keeps the Job open on the question the user has to answer, and pushes. It
// commits no records: an ask exit is exclusive, so passing writes die with the
// question and only the refusal list and dropped writes reach the Trace.
func (r *run) ask(ctx context.Context) (state, error) {
	question := r.question(ctx)
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
