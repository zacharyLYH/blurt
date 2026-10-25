package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"blurt/internal/db"
	"blurt/internal/push"
	"blurt/internal/queue"
	"blurt/internal/store"
)

// pinned is the anchor every write is checked and resolved against: a Saturday
// evening in Singapore. Singapore is UTC+8 year round, so a fixed zone stands in
// for it and no test leans on the tz database being installed.
var pinned = time.Date(2026, 10, 24, 18, 30, 0, 0, time.FixedZone("Asia/Singapore", 8*60*60))

const zone = "Asia/Singapore"

// scriptedModel answers a fixed list of turns, so the loop's checking and
// committing are testable without a model. It keeps every Input it was handed,
// which is how the prompt assembly and the missing fields are asserted.
type scriptedModel struct {
	turns  []Turn
	inputs []Input
	err    error
	// question is what the question writer answers, and askErr what it fails
	// with. An unscripted answer is unusable, so the canned sentence stands.
	question string
	asks     []Input
	missing  [][]Field
	askErr   error
	// tags is what the tagger answers, and tagErr what it fails with. Neither is
	// scripted by default, which leaves a Memory carrying no features at all.
	tags   []string
	tagErr error
	// tagBlock holds the tagger open, which is how a test sees that a run does not wait
	// for its features.
	tagBlock bool

	// tagCalls is appended from the tagger's own goroutine, so it is read under the lock.
	mu       sync.Mutex
	tagCalls []string
}

func (p *scriptedModel) Propose(_ context.Context, in Input) (Turn, error) {
	p.inputs = append(p.inputs, in)
	if p.err != nil {
		return Turn{}, p.err
	}
	if len(p.turns) == 0 {
		return Turn{}, nil
	}
	turn := p.turns[0]
	p.turns = p.turns[1:]
	return turn, nil
}

func (p *scriptedModel) Question(_ context.Context, in Input, missing []Field) (string, error) {
	p.asks = append(p.asks, in)
	p.missing = append(p.missing, missing)
	if p.askErr != nil {
		return "", p.askErr
	}
	return p.question, nil
}

func (p *scriptedModel) Tags(ctx context.Context, text string) ([]string, error) {
	p.mu.Lock()
	p.tagCalls = append(p.tagCalls, text)
	block := p.tagBlock
	p.mu.Unlock()
	if block {
		// The slow tagger: it answers only when the deadline it was given runs out.
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if p.tagErr != nil {
		return nil, p.tagErr
	}
	return p.tags, nil
}

// tagging is what the tagger has been asked about so far. The call lands on the tagger's own
// goroutine, so a test reads it here rather than off the field.
func (p *scriptedModel) tagging() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.tagCalls...)
}

var _ Model = (*scriptedModel)(nil)

// blockingModel never answers before the run's own deadline, which is the slow-host case.
type blockingModel struct{}

func (blockingModel) Propose(ctx context.Context, _ Input) (Turn, error) {
	<-ctx.Done()
	return Turn{}, ctx.Err()
}

func (blockingModel) Question(ctx context.Context, _ Input, _ []Field) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func (blockingModel) Tags(ctx context.Context, _ string) ([]string, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

var _ Model = blockingModel{}

// faultyStore is the durable half with one fault in it, so a retry and a fail are
// testable without breaking SQLite itself.
type faultyStore struct {
	Store
	commit error
}

func (s faultyStore) CommitAgentStep(ctx context.Context, jobID int64, step store.AgentStep) (store.Job, error) {
	if s.commit != nil {
		return store.Job{}, s.commit
	}
	return s.Store.CommitAgentStep(ctx, jobID, step)
}

type recordedPush struct {
	calls int
	title string
	body  string
}

func (p *recordedPush) NotifyUser(_ context.Context, _ int64, msg push.Message) (push.Result, error) {
	p.calls++
	p.title, p.body = msg.Title, msg.Body
	return push.Result{Sent: 1}, nil
}

// oneOff is a write the server can commit as a single local instant.
func oneOff(text, date, clock string) Write {
	return Write{Kind: KindReminder, Text: text, Zone: zone, Date: date, Time: clock, Recurrence: RecurrenceNone}
}

// repeating is a write with a pattern: the date is the first occurrence, which is
// where a weekly, monthly or yearly rule reads its calendar part from.
func repeating(text, date, clock, recurrence string) Write {
	return Write{Kind: KindReminder, Text: text, Zone: zone, Date: date, Time: clock, Recurrence: recurrence}
}

// turn is the one write a turn carries.
func turn(write Write) Turn { return Turn{Writes: []Write{write}} }

type fixture struct {
	worker *AgentWorker
	jobs   *store.JobStore
	model  *scriptedModel
	pusher *recordedPush
	handle *db.DB
	userID int64
}

func newFixture(t *testing.T, turns ...Turn) *fixture {
	t.Helper()
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })

	ctx := context.Background()
	user, err := store.NewUserStore(handle.Write, handle.Read).EnsureUser(ctx, "Lam", "lam@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	model, pusher := &scriptedModel{turns: turns}, &recordedPush{}
	jobs := store.NewJobStore(handle.Write, handle.Read)
	return &fixture{
		worker: &AgentWorker{
			Jobs:       jobs,
			AgentQueue: queue.NewMock(),
			Loop: &AgentLoop{
				Store:     jobs,
				Recall:    Reads{Reminders: store.NewReminderStore(handle.Write, handle.Read), Memories: store.NewMemoryStore(handle.Read)},
				Model:     model,
				Pusher:    pusher,
				ModelName: "local-model",
				MaxTurns:  3,
				Now:       func() time.Time { return pinned },
			},
		},
		jobs:   jobs,
		model:  model,
		pusher: pusher,
		handle: handle,
		userID: user.UserID,
	}
}

func (f *fixture) textJob(t *testing.T, text string) store.Job {
	t.Helper()
	job, err := f.jobs.Create(context.Background(), f.userID,
		store.Message{Role: "user", Text: text, TZ: zone})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := f.worker.AgentQueue.Publish(context.Background(), job.JobID, 1); err != nil {
		t.Fatalf("publish job: %v", err)
	}
	return job
}

func (f *fixture) reply(t *testing.T, jobID int64, text string) {
	t.Helper()
	if _, err := f.jobs.AppendTurn(context.Background(), f.userID, jobID,
		store.Message{Text: text, TZ: zone}, nil); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if err := f.worker.AgentQueue.Publish(context.Background(), jobID, 1); err != nil {
		t.Fatalf("publish reply: %v", err)
	}
}

// runOnce claims and processes one Job, the way the poller does.
func (f *fixture) runOnce(t *testing.T) {
	t.Helper()
	if !f.worker.processOne(context.Background()) {
		t.Fatal("no Job was claimed")
	}
}

// tagCalls waits for the tagger to have been asked about n facts, and returns what it saw.
// Tagging runs behind the commit now, so a test cannot read it the moment a run returns.
func (f *fixture) tagCalls(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		saw := f.model.tagging()
		if len(saw) >= n {
			return saw
		}
		if time.Now().After(deadline) {
			t.Fatalf("tagger saw %v, want %d calls", saw, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// memoryTags waits for the features the tagger writes onto one committed Memory and returns
// them: the row is durable before they are, so a test waits for the write it is asserting.
func (f *fixture) memoryTags(t *testing.T, text string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var tags string
	for {
		if err := f.handle.Read.QueryRow(
			`SELECT Tags FROM Memory WHERE Text = ?`, text).Scan(&tags); err != nil {
			t.Fatalf("read memory: %v", err)
		}
		if tags != "" || time.Now().After(deadline) {
			return tags
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *fixture) load(t *testing.T, jobID int64) store.Job {
	t.Helper()
	job, err := f.jobs.ByID(context.Background(), f.userID, jobID)
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	return job
}

func (f *fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.handle.Read.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", query, err)
	}
	return n
}

// agentTrace reads the newest agent-stage Trace row, which is where the loop's
// diagnostics live.
func (f *fixture) agentTrace(t *testing.T, jobID int64) map[string]any {
	t.Helper()
	var raw string
	err := f.handle.Read.QueryRow(
		`SELECT Detail FROM Trace WHERE JobId = ? AND Stage = 'agent' ORDER BY TraceId DESC LIMIT 1`,
		jobID).Scan(&raw)
	if err != nil {
		t.Fatalf("read agent trace: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	return detail
}

// missingFields reads the fields the newest run handed the question, which is how a
// test names what MissingFields refused.
func (f *fixture) missingFields(t *testing.T, jobID int64) []Field {
	t.Helper()
	raw, ok := f.agentTrace(t, jobID)["missing"].([]any)
	if !ok {
		t.Fatal("the trace recorded no missing fields")
	}
	fields := make([]Field, 0, len(raw))
	for _, item := range raw {
		name, ok := item.(string)
		if !ok {
			t.Fatalf("missing field = %T, want a name", item)
		}
		fields = append(fields, Field(name))
	}
	return fields
}

// reminder reads the one committed Reminder row. The two one-off columns come back
// as nulls for a repeat, which is what tells the two shapes apart in the store.
func (f *fixture) reminder(t *testing.T) (text, gotZone string, local, emit sql.NullString) {
	t.Helper()
	if err := f.handle.Read.QueryRow(
		`SELECT Text, Timezone, LocalDateTime, EmitAtUtc FROM Reminder`).Scan(&text, &gotZone, &local, &emit); err != nil {
		t.Fatalf("read reminder: %v", err)
	}
	return text, gotZone, local, emit
}

// TestWorkerCommitsAOneOffReminder is the happy path: a resolved capture becomes one
// Reminder whose UTC instant the server derived, and the Job closes on the server's
// own sentence.
func TestWorkerCommitsAOneOffReminder(t *testing.T) {
	write := oneOff("Buy glue", "2026-10-25", "18:00")
	write.Evidence = `the user said "at 6pm tomorrow"`
	f := newFixture(t, turn(write))
	job := f.textJob(t, "buy glue at 6pm tomorrow")

	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	if len(stored.Conversation) != 2 || stored.Conversation[1].Role != "assistant" ||
		stored.Conversation[1].Text != reminderReply {
		t.Fatalf("conversation = %+v, want the capture and the commit sentence", stored.Conversation)
	}
	text, gotZone, local, emit := f.reminder(t)
	if text != "Buy glue" || gotZone != zone || local.String != "2026-10-25T18:00" {
		t.Fatalf("reminder = %q %q %q", text, gotZone, local.String)
	}
	// The local time is the truth and the instant is derived from it plus the zone,
	// which is the whole time model in one assertion.
	if emit.String != "2026-10-25T10:00:00Z" {
		t.Fatalf("emit at utc = %q, want 2026-10-25T10:00:00Z", emit.String)
	}
	if n := f.count(t, `SELECT count(*) FROM RecurringReminder`); n != 0 {
		t.Fatalf("recurring rows = %d, want none for a one-off", n)
	}
	detail := f.agentTrace(t, job.JobID)
	if detail["outcome"] != store.JobComplete || detail["turns"] != float64(1) {
		t.Fatalf("trace = %v", detail)
	}
	// What the model said it read the time from rides the Trace, so a reader can
	// check the answer against the user's own words.
	if detail["evidence"] != write.Evidence {
		t.Fatalf("trace evidence = %v, want the model's own words", detail["evidence"])
	}
	if f.pusher.calls != 0 {
		t.Fatalf("pushes = %d, want none for a settled Job", f.pusher.calls)
	}
}

// TestWorkerCommitsARecurringReminder pins the other row shape: the rule is what is
// stored, the two one-off columns stay NULL, and the next instant is derived from the
// rule and the zone rather than carried forward.
func TestWorkerCommitsARecurringReminder(t *testing.T) {
	f := newFixture(t, turn(repeating("Take the trash out", "2026-10-27", "19:00", store.FrequencyWeekly)))
	job := f.textJob(t, "remind me to take the trash out every tuesday at 7pm")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	text, gotZone, local, emit := f.reminder(t)
	if text != "Take the trash out" || gotZone != zone {
		t.Fatalf("reminder = %q %q", text, gotZone)
	}
	if local.Valid || emit.Valid {
		t.Fatalf("one-off columns = %v %v, want NULL for a repeat", local, emit)
	}
	var rule, next string
	if err := f.handle.Read.QueryRow(
		`SELECT Rule, NextEmitAtUtc FROM RecurringReminder`).Scan(&rule, &next); err != nil {
		t.Fatalf("read rule: %v", err)
	}
	if rule != `{"frequency":"weekly","time":"19:00","weekday":2}` {
		t.Fatalf("rule = %s", rule)
	}
	if next != "2026-10-27T11:00:00Z" {
		t.Fatalf("next emit = %s, want 2026-10-27T11:00:00Z", next)
	}
	if n := f.count(t, `SELECT count(*) FROM Reminder`); n != 1 {
		t.Fatalf("reminder rows = %d, want the one beside the rule", n)
	}
}

// TestWorkerAsksForMissingFields is the deterministic half over the whole loop: each
// write is refused by field, the Job stays open on a question, nothing is committed, and
// the user is pushed once.
func TestWorkerAsksForMissingFields(t *testing.T) {
	changed := func(mutate func(*Write)) Write {
		write := oneOff("Call mum", "2026-10-25", "19:00")
		mutate(&write)
		return write
	}
	cases := []struct {
		name  string
		write Write
		want  []Field
	}{
		{"no zone", changed(func(w *Write) { w.Zone = "" }), []Field{FieldZone}},
		{"a zone that is not one", changed(func(w *Write) { w.Zone = "Mars/Olympus" }), []Field{FieldZone}},
		{"no date", changed(func(w *Write) { w.Date = "" }), []Field{FieldDate}},
		{"a date the calendar does not have", changed(func(w *Write) { w.Date = "2026-02-30" }), []Field{FieldDate}},
		{"no clock", changed(func(w *Write) { w.Time = "" }), []Field{FieldTime}},
		{"a clock that is not one", changed(func(w *Write) { w.Time = "19:60" }), []Field{FieldTime}},
		{"a time already gone", oneOff("Call mum", "2026-10-24", "09:00"), []Field{FieldTime}},
		{"no event", changed(func(w *Write) { w.Text = "  " }), []Field{FieldEvent}},
		{"a pattern that is not one", repeating("Call mum", "2026-10-25", "19:00", "hourly"), []Field{FieldRecurrence}},
		{"a daily repeat with no clock", repeating("Call mum", "", "", store.FrequencyDaily), []Field{FieldTime}},
		// A repeat's calendar part comes from its first occurrence, so a weekly
		// one with no first day has nothing to land on.
		{"a weekly repeat with no first day", repeating("Call mum", "", "19:00", store.FrequencyWeekly), []Field{FieldDate}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, turn(test.write))
			job := f.textJob(t, "remind me to call mum")

			f.runOnce(t)

			if stored := f.load(t, job.JobID); stored.Status != store.JobClarifying {
				t.Fatalf("status = %q, want clarifying", stored.Status)
			}
			if got := f.missingFields(t, job.JobID); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("missing = %v, want %v", got, test.want)
			}
			for _, table := range []string{"Reminder", "RecurringReminder"} {
				if n := f.count(t, `SELECT count(*) FROM `+table); n != 0 {
					t.Fatalf("%s rows = %d, want none while the Job asks", table, n)
				}
			}
			if f.pusher.calls != 1 {
				t.Fatalf("pushes = %d, want the one question", f.pusher.calls)
			}
		})
	}
}

// A turn with nothing in it is the model giving up: every field is the user's to
// supply, and the Job asks rather than closing on nothing.
func TestWorkerAsksWhenTheModelProposesNothing(t *testing.T) {
	f := newFixture(t)
	job := f.textJob(t, "mumble")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobClarifying {
		t.Fatalf("status = %q, want clarifying", stored.Status)
	}
	if got := f.missingFields(t, job.JobID); !reflect.DeepEqual(got,
		[]Field{FieldEvent, FieldZone, FieldDate, FieldTime}) {
		t.Fatalf("missing = %v, want every field", got)
	}
}

// The question is the model's own sentence when it wrote a usable one, so a capture's
// edge case is covered by wording rather than by one canned line — and the writer is
// told which fields are missing, because that is what it has to ask for.
func TestWorkerPrefersTheModelsQuestion(t *testing.T) {
	f := newFixture(t, turn(oneOff("Call mum", "", "19:00")))
	f.model.question = `Which day should I set for "call mum"?`
	job := f.textJob(t, "remind me to call mum at 7pm")

	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if got := stored.Conversation[len(stored.Conversation)-1].Text; got != f.model.question {
		t.Fatalf("question = %q, want the model's own", got)
	}
	if len(f.model.missing) != 1 || !reflect.DeepEqual(f.model.missing[0], []Field{FieldDate}) {
		t.Fatalf("the writer was handed %v, want the date", f.model.missing)
	}
	if f.pusher.body != f.model.question {
		t.Fatalf("push body = %q, want the question", f.pusher.body)
	}
}

// The canned sentence stands in whenever the written question cannot be put to the
// user: it is nothing, it is not a question, it names a clock the user never said, or
// it carries a placeholder Blurt leaked into the prompt.
func TestWorkerFallsBackWhenTheQuestionIsUnusable(t *testing.T) {
	cases := []struct {
		name    string
		written string
		askErr  error
	}{
		{"nothing", "", nil},
		{"not a question", "I don't know.", nil},
		{"a question that asserts a clock", "Was that at 6pm?", nil},
		{"a leaked placeholder", "Is <unknown> right?", nil},
		{"the model failed", "", errors.New("agent: call model: connection refused")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, turn(oneOff("Call mum", "", "19:00")))
			f.model.question, f.model.askErr = test.written, test.askErr
			job := f.textJob(t, "remind me to call mum at 7pm")

			f.runOnce(t)

			stored := f.load(t, job.JobID)
			if stored.Status != store.JobClarifying {
				t.Fatalf("status = %q, want clarifying", stored.Status)
			}
			if got := stored.Conversation[len(stored.Conversation)-1].Text; got != defaultQuestion {
				t.Fatalf("question = %q, want the canned sentence", got)
			}
		})
	}
}

// A Job that has already asked twice gets the canned sentence rather than costing a
// third model call: the writer is not reached at all on that round.
func TestWorkerStopsAskingTheModelAtTheClarityCap(t *testing.T) {
	refused := turn(oneOff("Call mum", "", "19:00"))
	f := newFixture(t, refused, refused, refused)
	f.model.question = "Which day should I set?"
	job := f.textJob(t, "remind me to call mum at 7pm")

	f.runOnce(t)
	f.reply(t, job.JobID, "next tuesday")
	f.runOnce(t)
	f.reply(t, job.JobID, "next tuesday at 7pm")
	f.runOnce(t)

	if len(f.model.asks) != maxClarityRounds {
		t.Fatalf("the writer was asked %d times, want %d", len(f.model.asks), maxClarityRounds)
	}
	stored := f.load(t, job.JobID)
	if got := stored.Conversation[len(stored.Conversation)-1].Text; got != defaultQuestion {
		t.Fatalf("question = %q, want the canned sentence on the last round", got)
	}
}

// The two-turn flow: the first turn cannot pin a day, the answer supplies it, and the
// second turn commits against the same conversation.
func TestWorkerCommitsTheAnsweredTurn(t *testing.T) {
	f := newFixture(t,
		turn(oneOff("Buy glue", "", "18:00")),
		turn(oneOff("Buy glue", "2026-10-25", "18:00")))
	job := f.textJob(t, "buy glue when I'm home")

	f.runOnce(t)
	if stored := f.load(t, job.JobID); stored.Status != store.JobClarifying {
		t.Fatalf("status = %q, want the question first", stored.Status)
	}

	f.reply(t, job.JobID, "tomorrow at 6pm")
	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want the commit", stored.Status)
	}
	// The answer earns the whole attempt budget back, so a resumed Job is never
	// poisoned by the questions it already answered.
	if stored.RetryCount != 0 {
		t.Fatalf("retry count = %d, want a fresh budget after the reply", stored.RetryCount)
	}
	if _, _, local, emit := f.reminder(t); local.String != "2026-10-25T18:00" ||
		emit.String != "2026-10-25T10:00:00Z" {
		t.Fatalf("reminder = %q %q", local.String, emit.String)
	}
	// The answer's own turn is in the conversation, so the Job reads as a chat.
	if len(stored.Conversation) != 4 {
		t.Fatalf("conversation = %+v, want capture, question, answer, reply", stored.Conversation)
	}
}

// A model that cannot be reached is a retry, not a failure, while the attempt budget
// holds: the Job goes back to the queue and commits nothing.
func TestWorkerRetriesWhenTheModelFails(t *testing.T) {
	f := newFixture(t)
	f.model.err = errors.New("agent: call model: connection refused")
	job := f.textJob(t, "buy glue at 6pm tomorrow")

	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobOpen || stored.RetryCount != 1 {
		t.Fatalf("job = %q retry %d, want open on the first retry", stored.Status, stored.RetryCount)
	}
	if detail := f.agentTrace(t, job.JobID); detail["outcome"] != "retry" {
		t.Fatalf("trace outcome = %v, want retry", detail["outcome"])
	}
	if n := f.count(t, `SELECT count(*) FROM Reminder`); n != 0 {
		t.Fatalf("reminders = %d, want none from a failed attempt", n)
	}
	if f.pusher.calls != 0 {
		t.Fatalf("pushes = %d, want none while the Job can still be retried", f.pusher.calls)
	}
}

// A refusal a retry cannot fix — a bad key — fails the Job at once, with the capture
// path's own sentence, rather than burning the whole budget on it.
func TestWorkerFailsOnAPermanentRefusal(t *testing.T) {
	f := newFixture(t)
	f.model.err = &Error{Status: http.StatusUnauthorized, Message: "missing or invalid api key"}
	job := f.textJob(t, "buy glue at 6pm tomorrow")

	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobFailed {
		t.Fatalf("status = %q, want failed", stored.Status)
	}
	if last := stored.Conversation[len(stored.Conversation)-1]; last.Role != "assistant" || last.Text != FallbackMessage {
		t.Fatalf("last turn = %+v, want the fallback sentence", last)
	}
	if f.pusher.calls != 1 || f.pusher.body != FallbackMessage {
		t.Fatalf("push = %d %q, want the failure sentence", f.pusher.calls, f.pusher.body)
	}
}

// The durable half can fail too, and a commit that did not land is retried rather than
// asked about: the user owes Blurt nothing.
func TestWorkerRetriesWhenTheCommitFails(t *testing.T) {
	f := newFixture(t, turn(oneOff("Buy glue", "2026-10-25", "18:00")))
	f.worker.Loop.Store = faultyStore{Store: f.jobs, commit: errors.New("disk is full")}
	job := f.textJob(t, "buy glue at 6pm tomorrow")

	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobOpen || stored.RetryCount != 1 {
		t.Fatalf("job = %q retry %d, want open on the first retry", stored.Status, stored.RetryCount)
	}
	if n := f.count(t, `SELECT count(*) FROM Reminder`); n != 0 {
		t.Fatalf("reminders = %d, want none from a commit that rolled back", n)
	}
}

// The run's own wall is what bounds a slow model: the attempt ends and the Job takes the
// ordinary retry rather than pinning the worker.
func TestWorkerStopsAtTheHardDeadline(t *testing.T) {
	f := newFixture(t)
	f.worker.Loop.Model = blockingModel{}
	f.worker.Loop.Timeout = 20 * time.Millisecond
	job := f.textJob(t, "buy glue at 6pm tomorrow")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobOpen {
		t.Fatalf("status = %q, want the Job back in the queue", stored.Status)
	}
}

// Recall is advisory, exactly like the transcript gate: a failing read is dropped rather
// than failing a capture the user already made, and the prompt still carries the anchor
// zone and the loop's own clock.
func TestWorkerRecallIsAdvisory(t *testing.T) {
	f := newFixture(t, turn(oneOff("Buy glue", "2026-10-25", "18:00")))
	f.worker.Loop.Recall = failingRecall{}
	job := f.textJob(t, "buy glue at 6pm tomorrow")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want the capture to survive a failing index", stored.Status)
	}
	in := f.model.inputs[0]
	if !in.Now.Equal(pinned) || in.CurrentTZ != zone {
		t.Fatalf("input = %+v, want the pinned clock and the anchor zone", in)
	}
	if len(in.Nearby) != 0 || len(in.Remembered) != 0 {
		t.Fatalf("input = %+v, want the failed reads dropped", in)
	}
}

// failingRecall is the advisory read that cannot work.
type failingRecall struct{}

func (failingRecall) Nearby(context.Context, int64, time.Time, time.Time) ([]store.Reminder, error) {
	return nil, errors.New("index is down")
}

func (failingRecall) Remembered(context.Context, int64, string) ([]store.Memory, error) {
	return nil, errors.New("index is down")
}

var _ Recall = failingRecall{}

// A fact becomes a Memory and closes the Job. It carries no zone, day, clock or pattern, and
// MissingFields accepts it without any: a Memory is context, not an interruption.
func TestWorkerCommitsAMemory(t *testing.T) {
	f := newFixture(t, turn(Write{Kind: KindMemory, Text: "Milk is cheaper at store A"}))
	job := f.textJob(t, "remember that milk is cheaper at store A")

	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	if got := stored.Conversation[len(stored.Conversation)-1].Text; got != memoryReply {
		t.Fatalf("reply = %q, want %q", got, memoryReply)
	}
	if n := f.count(t, `SELECT count(*) FROM Reminder`); n != 0 {
		t.Fatalf("reminders = %d, want none for a fact", n)
	}
	// The tagger runs either way; with nothing scripted it has nothing to write.
	f.tagCalls(t, 1)
	var text, tags string
	if err := f.handle.Read.QueryRow(`SELECT Text, Tags FROM Memory`).Scan(&text, &tags); err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if text != "Milk is cheaper at store A" {
		t.Fatalf("memory = %q", text)
	}
	if tags != "" {
		t.Fatalf("tags = %q, want none without a scripted tagger", tags)
	}
}

// The generated features land on the Memory the run commits, which is what lets a later
// capture worded nothing like the fact still find it.
func TestWorkerTagsTheMemoryItCommits(t *testing.T) {
	f := newFixture(t, turn(Write{Kind: KindMemory, Text: "Milk is cheaper at store A"}))
	f.model.tags = []string{"milk", "grocery", "price"}
	job := f.textJob(t, "remember that milk is cheaper at store A")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	if tags := f.memoryTags(t, "Milk is cheaper at store A"); tags != "milk,grocery,price" {
		t.Fatalf("tags = %q, want the generated features", tags)
	}
	// The tag call is about the fact alone: it carries no conversation, so nothing
	// else it was told could colour the features.
	if saw := f.tagCalls(t, 1); saw[0] != "Milk is cheaper at store A" {
		t.Fatalf("tagger saw %v, want one call about the Memory", saw)
	}
}

// Tagging is advisory, like recall: a tagger that fails leaves the Memory without
// features rather than failing a capture the user already made.
func TestWorkerDropsTagsWhenTheTaggerFails(t *testing.T) {
	f := newFixture(t, turn(Write{Kind: KindMemory, Text: "Milk is cheaper at store A"}))
	f.model.tagErr = errors.New("agent: call model: connection refused")
	job := f.textJob(t, "remember that milk is cheaper at store A")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want the capture to survive a failing tagger", stored.Status)
	}
	// The call is on the tagger's own goroutine, so waiting for it is what makes an empty
	// column a real assertion: the tagger was asked, and it wrote nothing.
	f.tagCalls(t, 1)
	var tags string
	if err := f.handle.Read.QueryRow(`SELECT Tags FROM Memory`).Scan(&tags); err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if tags != "" {
		t.Fatalf("tags = %q, want none", tags)
	}
}

// An ask exit commits nothing, so it costs no tag call: tagging runs at commit, not at accept.
func TestWorkerDoesNotTagOnAnAskExit(t *testing.T) {
	f := newFixture(t, turn(Write{Kind: KindReminder, Text: "Buy glue", Zone: zone}))
	f.model.tags = []string{"milk"}
	job := f.textJob(t, "buy glue when I'm home")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobClarifying {
		t.Fatalf("status = %q, want clarifying", stored.Status)
	}
	// Nothing was scheduled, so a moment's grace is all a wrong implementation would need to
	// show itself here.
	time.Sleep(20 * time.Millisecond)
	if saw := f.model.tagging(); len(saw) != 0 {
		t.Fatalf("tagger saw %v, want no call for a run that committed nothing", saw)
	}
}

// A capture that carries an event and a fact commits both in one step, and only the fact
// costs a tag call — the Reminder has no features to generate.
func TestWorkerCommitsAnEventAndAFact(t *testing.T) {
	f := newFixture(t, Turn{Writes: []Write{
		oneOff("Lunch at Ikea", "2026-10-30", "13:00"),
		{Kind: KindMemory, Text: "I have an Ikea voucher"},
	}})
	f.model.tags = []string{"ikea", "voucher", "lunch"}
	job := f.textJob(t, "lunch at Ikea this Friday at 1pm, and I have an Ikea voucher")

	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	stored = f.load(t, job.JobID)
	if got := stored.Conversation[len(stored.Conversation)-1].Text; got != reminderReply {
		t.Fatalf("reply = %q, want the Reminder's", got)
	}
	if n := f.count(t, `SELECT count(*) FROM Reminder`); n != 1 {
		t.Fatalf("reminders = %d, want the event", n)
	}
	if tags := f.memoryTags(t, "I have an Ikea voucher"); tags != "ikea,voucher,lunch" {
		t.Fatalf("tags = %q, want the fact's generated features", tags)
	}
	if saw := f.tagCalls(t, 1); saw[0] != "I have an Ikea voucher" {
		t.Fatalf("tagger saw %v, want the fact alone", saw)
	}
}

// Tagging is off the run's path: a tagger that never answers still leaves the Memory committed
// and the Job closed, because by then the user has already read the reply.
func TestWorkerCommitsWithoutWaitingForTheTagger(t *testing.T) {
	f := newFixture(t, turn(Write{Kind: KindMemory, Text: "Milk is cheaper at store A"}))
	f.model.tags = []string{"milk", "grocery", "price"}
	f.model.tagBlock = true
	f.worker.Loop.AttemptTimeout = 50 * time.Millisecond
	job := f.textJob(t, "remember that milk is cheaper at store A")

	// The run returns with the tagger still held.
	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want the capture closed without its features", stored.Status)
	}
	if got := stored.Conversation[len(stored.Conversation)-1].Text; got != memoryReply {
		t.Fatalf("reply = %q, want %q", got, memoryReply)
	}
	// The row is there and its features are not: they arrive later, or not at all.
	f.tagCalls(t, 1)
	var text, tags string
	if err := f.handle.Read.QueryRow(`SELECT Text, Tags FROM Memory`).Scan(&text, &tags); err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if text != "Milk is cheaper at store A" || tags != "" {
		t.Fatalf("memory = %q %q, want the fact committed untagged", text, tags)
	}
}
