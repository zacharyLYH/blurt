package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"blurt/internal/db"
	"blurt/internal/failure"
	"blurt/internal/push"
	"blurt/internal/queue"
	"blurt/internal/store"
)

// pinned is the anchor every record is checked and resolved against: a Saturday
// evening in Singapore. Singapore is UTC+8 year round, so a fixed zone stands in
// for it and no test leans on the tz database being installed.
var pinned = time.Date(2026, 10, 24, 18, 30, 0, 0, time.FixedZone("Asia/Singapore", 8*60*60))

const zone = "Asia/Singapore"

// scriptedModel answers a fixed list of turns, one per run, so the loop's linting and
// committing are testable without a model. It keeps every PlannerInput it was handed and
// every message the loop sent back, which is how the prompt assembly, the lint note and the
// committing are asserted. Each run is one free-form turn that submits at once, then one
// strict call that returns the scripted answer.
type scriptedModel struct {
	turns  []Answer
	inputs []PlannerInput
	sent   []Message
	// prescreens counts the turns the screen was asked on, which is how a test sees
	// that every user turn reached the gate.
	prescreens int
	err        error
	// tags is what the tagger answers, and tagErr what it fails with. Neither is
	// scripted by default, which leaves a Memory carrying no features at all.
	tags   []string
	tagErr error
	// tagBlock holds the tagger open, which is how a test sees that a run does not wait
	// for its features.
	tagBlock bool
	// stalls makes the agent turn write prose and never submit, which is the loop that
	// spends its whole budget without a plan.
	stalls bool
	// finalizeErr is what the strict call behind submit_plan fails with, when the turn itself
	// is fine: a provider that dies after the loop, or an answer that is not the schema.
	finalizeErr error
	// lookUp makes the agent turn look something up before it submits, which is what a change
	// needs: the id it names has to be one this run's own find_reminders printed.
	lookUp string

	// tagCalls is appended from the tagger's own goroutine, so it is read under the lock.
	mu       sync.Mutex
	tagCalls []string
}

func (p *scriptedModel) Prescreen(_ context.Context, _ string) (Verdict, error) {
	p.prescreens++
	if p.err != nil {
		return Verdict{}, p.err
	}
	return Verdict{Outcome: verdictOK}, nil
}

// Chat is the agent turn: the dev double never looks anything up, so it submits at once. An
// error here is the transport failure every retry test scripts, and stalls makes the model
// answer prose without ever submitting.
func (p *scriptedModel) Chat(_ context.Context, in PlannerInput, sent []Message, _ []Tool) (Message, error) {
	if len(sent) == 0 {
		p.inputs = append(p.inputs, in)
	}
	p.sent = append(p.sent, sent...)
	if p.err != nil {
		return Message{}, p.err
	}
	if p.stalls {
		return Message{Role: "assistant", Content: "let me think about that"}, nil
	}
	if p.lookUp != "" && len(sent) == 0 {
		return lookupCall(p.lookUp), nil
	}
	return submitCall(), nil
}

// lookupCall is find_reminders, as a test double asks for it before it submits.
func lookupCall(query string) Message {
	return Message{Role: "assistant", ToolCalls: []ToolCall{{
		ID: "lookup", Type: functionType,
		Function: CallFunction{Name: toolFindReminders, Arguments: `{"query":"` + query + `"}`},
	}}}
}

// Finalize is the strict call: one scripted turn per run, which is what makes a lint fault
// and its retry scriptable as two answers.
func (p *scriptedModel) Finalize(_ context.Context, _ PlannerInput, _ []Message) (Answer, error) {
	if p.err != nil {
		return Answer{}, p.err
	}
	if p.finalizeErr != nil {
		return Answer{}, p.finalizeErr
	}
	if len(p.turns) == 0 {
		return Answer{}, nil
	}
	turn := p.turns[0]
	p.turns = p.turns[1:]
	return turn, nil
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

func (blockingModel) Prescreen(ctx context.Context, _ string) (Verdict, error) {
	<-ctx.Done()
	return Verdict{}, ctx.Err()
}

func (blockingModel) Chat(ctx context.Context, _ PlannerInput, _ []Message, _ []Tool) (Message, error) {
	<-ctx.Done()
	return Message{}, ctx.Err()
}

func (blockingModel) Finalize(ctx context.Context, _ PlannerInput, _ []Message) (Answer, error) {
	<-ctx.Done()
	return Answer{}, ctx.Err()
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

// oneOff is a record the server can commit as a single local instant.
func oneOff(text, date, clock string) Record {
	return Record{Kind: KindReminder, Text: text, Zone: zone, Date: date, Time: clock, Recurrence: RecurrenceNone}
}

// repeating is a record with a pattern: the date is the first occurrence, which is
// where a weekly, monthly or yearly rule reads its calendar part from.
func repeating(text, date, clock, recurrence string) Record {
	return Record{Kind: KindReminder, Text: text, Zone: zone, Date: date, Time: clock, Recurrence: recurrence}
}

// mutateRecord is one committable one-off with one field changed, so a table of faults can
// read as the change rather than as a whole record.
func mutateRecord(mutate func(*Record)) Record {
	record := oneOff("Call mum", "2026-10-25", "19:00")
	mutate(&record)
	return record
}

// answerWith is the one record an answer carries, as the loop's own answers arrive: a create
// that proposes it.
func answerWith(record Record) Answer {
	return Answer{Kind: AnswerRecords, Ops: []Op{{Intent: IntentCreate, Record: record}}}
}

// change is one update op: the whole row after the change, filled from the row it names.
func change(id int64, record Record) Answer {
	return Answer{Kind: AnswerRecords, Ops: []Op{{Intent: IntentUpdate, ReminderID: id, Record: record}}}
}

// aboutAFact is an answer that keeps one fact and writes no row.
func aboutAFact(text string) Answer {
	return Answer{Kind: AnswerRecords, Memories: []string{text}}
}

type fixture struct {
	worker *queue.Worker
	stream *queue.Mock
	loop   *AgentLoop
	jobs   *store.JobStore
	model  *scriptedModel
	pusher *recordedPush
	handle *db.DB
	userID int64
}

func newFixture(t *testing.T, turns ...Answer) *fixture {
	t.Helper()
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })

	ctx := context.Background()
	user, err := store.NewUserStore(handle.Write, handle.Read).EnsureActive(ctx, "Lam", "lam@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	model, pusher := &scriptedModel{turns: turns}, &recordedPush{}
	jobs := store.NewJobStore(handle.Write, handle.Read)
	loop := &AgentLoop{
		Store:   jobs,
		Recall:  Retrieval{Reminders: store.NewReminderStore(handle.Write, handle.Read), Memories: store.NewMemoryStore(handle.Read)},
		Planner: Stage{Model: model, Name: "local-model"},
		Pusher:  pusher,
		Now:     func() time.Time { return pinned },
	}
	stream := queue.NewMock()
	return &fixture{
		worker: &queue.Worker{Stream: stream, Jobs: jobs, Step: loop.Step},
		stream: stream,
		loop:   loop,
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
	if err := f.stream.Publish(context.Background(), job.JobID, 1); err != nil {
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
	if err := f.stream.Publish(context.Background(), jobID, 1); err != nil {
		t.Fatalf("publish reply: %v", err)
	}
}

// runOnce claims and processes one Job, the way the poller does.
func (f *fixture) runOnce(t *testing.T) {
	t.Helper()
	if !f.worker.ProcessOne(context.Background()) {
		t.Fatal("no Job was claimed")
	}
}

// runText creates a Job for one capture, processes it, and returns the Job the run left
// behind: the setup and the one claim nearly every worker test opens with.
func (f *fixture) runText(t *testing.T, text string) store.Job {
	t.Helper()
	job := f.textJob(t, text)
	f.runOnce(t)
	return f.load(t, job.JobID)
}

// wantStatus is the assertion nearly every worker test opens with about the Job a run left.
func wantStatus(t *testing.T, job store.Job, want string) store.Job {
	t.Helper()
	if job.Status != want {
		t.Fatalf("status = %q, want %q", job.Status, want)
	}
	return job
}

// tagCalls waits for the tagger to have been asked about n facts, and returns what it saw.
// Tagger runs behind the commit now, so a test cannot read it the moment a run returns.
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

// memoryTags waits for the features the tagger records onto one committed Memory and returns
// them: the row is durable before they are, so a test waits for the record it is asserting.
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

// memoryRow reads the one committed Memory and the features written onto it.
func (f *fixture) memoryRow(t *testing.T) (text, tags string) {
	t.Helper()
	if err := f.handle.Read.QueryRow(`SELECT Text, Tags FROM Memory`).Scan(&text, &tags); err != nil {
		t.Fatalf("read memory: %v", err)
	}
	return text, tags
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

// lintFaults reads what the newest run's linter refused, which is how a test names a
// fault by the words the model was told.
func (f *fixture) lintFaults(t *testing.T, jobID int64) []string {
	t.Helper()
	raw, ok := f.agentTrace(t, jobID)["lint"].([]any)
	if !ok {
		t.Fatal("the trace recorded no lint faults")
	}
	faults := make([]string, 0, len(raw))
	for _, item := range raw {
		note, ok := item.(string)
		if !ok {
			t.Fatalf("lint fault = %T, want the note the model read", item)
		}
		faults = append(faults, note)
	}
	return faults
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
	answer := answerWith(oneOff("Buy glue", "2026-10-25", "18:00"))
	answer.Evidence = `the user said "at 6pm tomorrow"`
	f := newFixture(t, answer)
	stored := wantStatus(t, f.runText(t, "buy glue at 6pm tomorrow"), store.JobComplete)

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
	detail := f.agentTrace(t, stored.JobID)
	// One run is two calls: the loop's own turn and the strict one behind submit_plan.
	if detail["outcome"] != store.JobComplete || detail["turns"] != float64(2) {
		t.Fatalf("trace = %v", detail)
	}
	// What the model said it read the time from rides the Trace, so a reader can
	// check the answer against the user's own words.
	if detail["evidence"] != answer.Evidence {
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
	f := newFixture(t, answerWith(repeating("Take the trash out", "2026-10-27", "19:00", store.FrequencyWeekly)))
	wantStatus(t, f.runText(t, "remind me to take the trash out every tuesday at 7pm"), store.JobComplete)

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

// TestWorkerLintsEveryRefusedField is the deterministic half over the whole loop: each
// record is refused by field, the fault names the value the model wrote, nothing is
// committed, and a model that writes the same thing twice fails the Job with the capture
// path's own sentence.
func TestWorkerLintsEveryRefusedField(t *testing.T) {
	cases := []struct {
		name   string
		record Record
		want   string
	}{
		{"a zone that is not one", mutateRecord(func(w *Record) { w.Zone = "Mars/Olympus" }), `zone "Mars/Olympus" is not an IANA zone`},
		{"no date", mutateRecord(func(w *Record) { w.Date = "" }), "date is empty"},
		{"a date the calendar does not have", mutateRecord(func(w *Record) { w.Date = "2026-02-30" }), `date "2026-02-30" is not`},
		{"no clock", mutateRecord(func(w *Record) { w.Time = "" }), "time is empty"},
		{"a clock that is not one", mutateRecord(func(w *Record) { w.Time = "19:60" }), `time "19:60" is not HH:MM`},
		{"a time already gone", oneOff("Call mum", "2026-10-24", "09:00"), `time "09:00" is in the past`},
		{"no event", mutateRecord(func(w *Record) { w.Text = "  " }), "event is empty"},
		{"a pattern that is not one", repeating("Call mum", "2026-10-25", "19:00", "hourly"), `recurrence "hourly" is not`},
		{"a daily repeat with no clock", repeating("Call mum", "", "", store.FrequencyDaily), "time is empty"},
		// A repeat's calendar part comes from its first occurrence, so a weekly
		// one with no first day has nothing to land on.
		{"a weekly repeat with no first day", repeating("Call mum", "", "19:00", store.FrequencyWeekly), "date is empty"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, answerWith(test.record), answerWith(test.record))
			job := wantStatus(t, f.runText(t, "remind me to call mum"), store.JobFailed)

			if faults := strings.Join(f.lintFaults(t, job.JobID), "\n"); !strings.Contains(faults, test.want) {
				t.Fatalf("lint = %q, want it to name %q", faults, test.want)
			}
			for _, table := range []string{"Reminder", "RecurringReminder"} {
				if n := f.count(t, `SELECT count(*) FROM `+table); n != 0 {
					t.Fatalf("%s rows = %d, want none from a refused answer", table, n)
				}
			}
			if f.pusher.calls != 1 || f.pusher.body != FallbackMessage {
				t.Fatalf("pushes = %d %q, want the failure sentence", f.pusher.calls, f.pusher.body)
			}
		})
	}
}

// Every user turn is screened, including the capture that retries a failed Job.
func TestWorkerScreensACaptureAfterAFailure(t *testing.T) {
	f := newFixture(t)
	f.model.finalizeErr = &failure.Refusal{Service: "agent", Status: http.StatusUnauthorized, Message: "missing or invalid api key"}
	job := wantStatus(t, f.runText(t, "buy glue at 6pm tomorrow"), store.JobFailed)
	if f.model.prescreens != 1 {
		t.Fatalf("prescreens = %d, want the failed run to have been screened once", f.model.prescreens)
	}

	f.model.finalizeErr = nil
	f.model.turns = []Answer{answerWith(oneOff("Buy glue", "2026-10-25", "18:00"))}
	f.reply(t, job.JobID, "buy glue at 6pm tomorrow")
	f.runOnce(t)

	wantStatus(t, f.load(t, job.JobID), store.JobComplete)
	if f.model.prescreens != 2 {
		t.Fatalf("prescreens = %d, want the retried capture screened again", f.model.prescreens)
	}
}

// askedQuestions is the Job's own question count, which is what the cap is measured
// against: a commit sentence and a failure notice are not questions, so neither spends a
// round of the cap.
func TestAskedQuestionsCountsOnlyQuestions(t *testing.T) {
	conversation := []store.Message{
		{Role: "user", Text: "buy glue"},
		{Role: "assistant", Text: FallbackMessage},
		{Role: "assistant", Text: reminderReply},
		{Role: "assistant", Text: "When should I remind you?"},
	}
	if got := askedQuestions(conversation); got != 1 {
		t.Fatalf("askedQuestions = %d, want only the question counted", got)
	}
}

// The faults go back to the model as the retry's own conversation, so a fixable answer costs
// one more budget instead of reaching the user as a question.
func TestWorkerRetriesTheLoopWithTheFaults(t *testing.T) {
	f := newFixture(t,
		answerWith(oneOff("Call mum", "", "19:00")),
		answerWith(oneOff("Call mum", "2026-10-25", "19:00")))
	job := wantStatus(t, f.runText(t, "remind me to call mum at 7pm"), store.JobComplete)
	if _, _, local, _ := f.reminder(t); local.String != "2026-10-25T19:00" {
		t.Fatalf("local = %q, want the corrected day", local.String)
	}
	// The retry read the fault the server raised, and the run cost two loop calls and two
	// strict calls: the second budget is a whole loop, not one extra turn.
	var note string
	for _, message := range f.model.sent {
		if message.Role == "user" && strings.HasPrefix(message.Content, "lint:") {
			note = message.Content
		}
	}
	if !strings.HasPrefix(note, "lint: date is empty") {
		t.Fatalf("retry note = %q, want the linter's own words", note)
	}
	if detail := f.agentTrace(t, job.JobID); detail["turns"] != float64(4) {
		t.Fatalf("turns = %v, want two loop calls and two strict calls", detail["turns"])
	}
}

// A model that answers prose and never submits spends the loop's budget: the missing
// submit_plan is a fault like any other, and the second one fails the Job.
func TestWorkerLintsALoopThatNeverSubmits(t *testing.T) {
	f := newFixture(t)
	f.model.stalls = true
	job := wantStatus(t, f.runText(t, "buy glue at 6pm tomorrow"), store.JobFailed)
	if faults := strings.Join(f.lintFaults(t, job.JobID), "\n"); !strings.Contains(faults, "without submit_plan") {
		t.Fatalf("lint = %q, want the missing submit named", faults)
	}
	// Two runs, each spending the whole budget: the cap is what bounds a stuck model.
	if detail := f.agentTrace(t, job.JobID); detail["turns"] != float64(2*maxLoopCalls) {
		t.Fatalf("turns = %v, want both budgets spent", detail["turns"])
	}
}

// A question is the one answer the linter does not check: asking is the point, and the Job
// goes to the clarification path with the model's own sentence.
func TestWorkerAsksWhenTheModelAsks(t *testing.T) {
	const question = "When should I remind you about buying glue?"
	f := newFixture(t, asked(question))
	stored := wantStatus(t, f.runText(t, "buy glue when I'm home"), store.JobClarifying)
	if got := stored.Conversation[len(stored.Conversation)-1].Text; got != question {
		t.Fatalf("question = %q, want the model's own", got)
	}
	if f.pusher.calls != 1 || f.pusher.body != question {
		t.Fatalf("push = %d %q, want the question", f.pusher.calls, f.pusher.body)
	}
	if n := f.count(t, `SELECT count(*) FROM Reminder`); n != 0 {
		t.Fatalf("reminders = %d, want an ask to commit nothing", n)
	}
}

// The canned sentence stands in whenever the written question cannot be put to the
// user: it is nothing, it is not a question, it names a clock the user never said, or
// it carries a placeholder Blurt leaked into the prompt.
func TestWorkerFallsBackWhenTheQuestionIsUnusable(t *testing.T) {
	cases := []struct {
		name    string
		written string
	}{
		{"nothing", ""},
		{"not a question", "I don't know."},
		{"a question that asserts a clock", "Was that at 6pm?"},
		{"a leaked placeholder", "Is <unknown> right?"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, asked(test.written))
			stored := wantStatus(t, f.runText(t, "remind me to call mum at 7pm"), store.JobClarifying)
			if got := stored.Conversation[len(stored.Conversation)-1].Text; got != defaultQuestion {
				t.Fatalf("question = %q, want the canned sentence", got)
			}
		})
	}
}

// A Job that has already asked twice gets the canned sentence on the third round: the
// question is no longer a second call, so the cap is what keeps one Job from spending
// its turns on wording it would only repeat.
func TestWorkerStopsAskingTheModelAtTheClarityCap(t *testing.T) {
	const question = "Which day should I set?"
	f := newFixture(t, asked(question), asked(question), asked(question))
	job := f.runText(t, "remind me to call mum at 7pm")

	f.reply(t, job.JobID, "next tuesday")
	f.runOnce(t)
	f.reply(t, job.JobID, "next tuesday at 7pm")
	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if got := stored.Conversation[1].Text; got != question {
		t.Fatalf("first question = %q, want the model's", got)
	}
	if got := stored.Conversation[len(stored.Conversation)-1].Text; got != defaultQuestion {
		t.Fatalf("question = %q, want the canned sentence on the last round", got)
	}
}

// The two-turn flow: the first turn asks for the day it cannot pin, the answer supplies it,
// and the second turn commits against the same conversation.
func TestWorkerCommitsTheAnsweredTurn(t *testing.T) {
	f := newFixture(t,
		asked("Which day should I set?"),
		answerWith(oneOff("Buy glue", "2026-10-25", "18:00")))
	job := wantStatus(t, f.runText(t, "buy glue when I'm home"), store.JobClarifying)

	f.reply(t, job.JobID, "tomorrow at 6pm")
	f.runOnce(t)
	stored := wantStatus(t, f.load(t, job.JobID), store.JobComplete)
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

// A provider that dies on the strict call is a failure of the provider, not of the answer: the
// run retries with backoff rather than spending another loop on the same dead endpoint. An
// answer that is not the schema is the opposite — the model's own mistake — so it is a fault the
// loop is told about, and it is the retry that costs a second budget.
func TestWorkerSeparatesAProviderFailureFromABadAnswer(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus string
		wantLint   string
	}{
		{"the provider never answered", errors.New("agent: model returned no content (finish length)"),
			store.JobOpen, ""},
		{"the answer is not the schema", schemaError{errors.New("agent: planner answer is not the schema")},
			store.JobFailed, "not the schema"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			f.model.finalizeErr = test.err
			stored := wantStatus(t, f.runText(t, "buy glue at 6pm tomorrow"), test.wantStatus)

			if test.wantLint == "" {
				// A provider failure leaves nothing for the model to read: the Job is
				// retried, and the Trace keeps the error.
				return
			}
			faults := strings.Join(f.lintFaults(t, stored.JobID), "\n")
			if !strings.Contains(faults, test.wantLint) {
				t.Fatalf("lint = %q, want it to name %q", faults, test.wantLint)
			}
		})
	}
}

// A model that cannot be reached is a retry, not a failure, while the attempt budget
// holds: the Job goes back to the queue and commits nothing.
func TestWorkerRetriesWhenTheModelFails(t *testing.T) {
	f := newFixture(t)
	f.model.err = errors.New("agent: call model: connection refused")
	stored := f.runText(t, "buy glue at 6pm tomorrow")

	if stored.Status != store.JobOpen || stored.RetryCount != 1 {
		t.Fatalf("job = %q retry %d, want open on the first retry", stored.Status, stored.RetryCount)
	}
	if detail := f.agentTrace(t, stored.JobID); detail["outcome"] != "retry" {
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
	f.model.err = &failure.Refusal{Service: "agent", Status: http.StatusUnauthorized, Message: "missing or invalid api key"}
	stored := wantStatus(t, f.runText(t, "buy glue at 6pm tomorrow"), store.JobFailed)
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
	f := newFixture(t, answerWith(oneOff("Buy glue", "2026-10-25", "18:00")))
	f.loop.Store = faultyStore{Store: f.jobs, commit: errors.New("disk is full")}
	stored := f.runText(t, "buy glue at 6pm tomorrow")

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
	f.loop.Planner.Model = blockingModel{}
	f.loop.Timeout = 20 * time.Millisecond
	wantStatus(t, f.runText(t, "buy glue at 6pm tomorrow"), store.JobOpen)
}

// Recall is advisory, exactly like the transcript gate: a failing read is dropped rather
// than failing a capture the user already made, and the prompt still carries the anchor
// zone and the loop's own clock.
func TestWorkerRecallIsAdvisory(t *testing.T) {
	f := newFixture(t, answerWith(oneOff("Buy glue", "2026-10-25", "18:00")))
	f.loop.Recall = &stubRecall{err: errors.New("index is down")}
	wantStatus(t, f.runText(t, "buy glue at 6pm tomorrow"), store.JobComplete)

	in := f.model.inputs[0]
	if !in.Now.Equal(pinned) || in.CurrentTZ != zone {
		t.Fatalf("input = %+v, want the pinned clock and the anchor zone", in)
	}
	if len(in.Remembered) != 0 {
		t.Fatalf("input = %+v, want the failed read dropped", in)
	}
}

// A fact becomes a Memory and closes the Job. It carries no zone, day, clock or pattern, and
// MissingFields accepts it without any: a Memory is context, not an interruption.
func TestWorkerCommitsAMemory(t *testing.T) {
	f := newFixture(t, aboutAFact("Milk is cheaper at store A"))
	stored := wantStatus(t, f.runText(t, "remember that milk is cheaper at store A"), store.JobComplete)
	if got := stored.Conversation[len(stored.Conversation)-1].Text; got != memoryReply {
		t.Fatalf("reply = %q, want %q", got, memoryReply)
	}
	if n := f.count(t, `SELECT count(*) FROM Reminder`); n != 0 {
		t.Fatalf("reminders = %d, want none for a fact", n)
	}
	// The tagger runs either way; with nothing scripted it has nothing to write.
	f.tagCalls(t, 1)
	text, tags := f.memoryRow(t)
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
	f := newFixture(t, aboutAFact("Milk is cheaper at store A"))
	f.model.tags = []string{"milk", "grocery", "price"}
	wantStatus(t, f.runText(t, "remember that milk is cheaper at store A"), store.JobComplete)
	if tags := f.memoryTags(t, "Milk is cheaper at store A"); tags != "milk,grocery,price" {
		t.Fatalf("tags = %q, want the generated features", tags)
	}
	// The tag call is about the fact alone: it carries no conversation, so nothing
	// else it was told could colour the features.
	if saw := f.tagCalls(t, 1); saw[0] != "Milk is cheaper at store A" {
		t.Fatalf("tagger saw %v, want one call about the Memory", saw)
	}
}

// Tagger is advisory, like recall: a tagger that fails leaves the Memory without
// features rather than failing a capture the user already made.
func TestWorkerDropsTagsWhenTheTaggerFails(t *testing.T) {
	f := newFixture(t, aboutAFact("Milk is cheaper at store A"))
	f.model.tagErr = errors.New("agent: call model: connection refused")
	wantStatus(t, f.runText(t, "remember that milk is cheaper at store A"), store.JobComplete)
	// The call is on the tagger's own goroutine, so waiting for it is what makes an empty
	// column a real assertion: the tagger was asked, and it wrote nothing.
	f.tagCalls(t, 1)
	if _, tags := f.memoryRow(t); tags != "" {
		t.Fatalf("tags = %q, want none", tags)
	}
}

// A capture that calls a reminder off writes nothing: the app owns that write, so the turn is
// one fixed sentence and the table is untouched. Nothing is looked up either — there is no row
// to name, so not even the ambiguity a change can meet is in play.
func TestWorkerAnswersACallOffWithoutWriting(t *testing.T) {
	f := newFixture(t, Answer{Kind: AnswerRecords, Ops: []Op{{Intent: IntentCancel}}})
	stored := wantStatus(t, f.runText(t, "cancel the dentist reminder"), store.JobComplete)
	if got := stored.Conversation[len(stored.Conversation)-1].Text; got != cancelReply {
		t.Fatalf("reply = %q, want the call-off's own sentence", got)
	}
	if n := f.count(t, `SELECT count(*) FROM Reminder`); n != 0 {
		t.Fatalf("reminders = %d, want nothing written", n)
	}
}

// A capture that changes a reminder rewrites the row it named: the id is one this run's own
// lookup printed, the row keeps it, and the table gains nothing.
func TestWorkerRewritesTheReminderAChangeNames(t *testing.T) {
	f := newFixture(t, answerWith(oneOff("Buy glue", "2026-10-25", "18:00")))
	wantStatus(t, f.runText(t, "buy glue at 6pm tomorrow"), store.JobComplete)

	id := f.reminderID(t)
	f.model.turns = []Answer{change(id, oneOff("Buy glue", "2026-10-27", "20:00"))}
	f.model.lookUp = "glue"

	stored := wantStatus(t, f.runText(t, "make it 8pm on the 27th"), store.JobComplete)
	if got := stored.Conversation[len(stored.Conversation)-1].Text; got != updateReply {
		t.Fatalf("reply = %q, want the change's", got)
	}
	if got := f.localDateTime(t, id); got != "2026-10-27T20:00" {
		t.Fatalf("local = %q, want the rewritten clock", got)
	}
	if n := f.count(t, `SELECT count(*) FROM Reminder`); n != 1 {
		t.Fatalf("reminders = %d, want the change in place rather than a second row", n)
	}
}

// reminderID reads the id of the one Reminder a run committed, which is what a change names.
func (f *fixture) reminderID(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := f.handle.Read.QueryRow(`SELECT ReminderId FROM Reminder`).Scan(&id); err != nil {
		t.Fatalf("read reminder id: %v", err)
	}
	return id
}

// localDateTime reads one Reminder's stored local clock.
func (f *fixture) localDateTime(t *testing.T, id int64) string {
	t.Helper()
	var local string
	if err := f.handle.Read.QueryRow(
		`SELECT LocalDateTime FROM Reminder WHERE ReminderId = ?`, id).Scan(&local); err != nil {
		t.Fatalf("read reminder: %v", err)
	}
	return local
}

// withWire puts the planner on a real ModelClient behind the answers a test scripts, and leaves
// the prescreen on the dev double: the model's own words travel the wire this way, so the schema,
// answerFrom and the linter all sit inside the run rather than in a Go value a test handed the
// loop.
func (f *fixture) withWire(t *testing.T, answers ...[]byte) {
	t.Helper()
	var mu sync.Mutex
	client := modelServer(t, Settings{Model: "stub"}, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		var answer []byte
		if len(answers) > 0 {
			answer, answers = answers[0], answers[1:]
		}
		mu.Unlock()
		if answer == nil {
			t.Error("the run asked for an answer the test did not script")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(answer)
	})
	f.loop.Prescreen = Stage{Model: f.model, Name: "prescreen"}
	f.loop.Planner = Stage{Model: client, Name: "stub"}
}

// changeAnswer is one strict answer that rewrites a row, as the model writes it on the wire.
func changeAnswer(id int64) string {
	return `{"kind":"records","ops":[{"intent":"update","reminder_id":` + strconv.FormatInt(id, 10) +
		`,"text":"Buy glue","zone":"Asia/Singapore","day":"2026-10-27","time":"20:00",` +
		`"recurrence":"none"}],"memories":[],"evidence":"they said 8pm on the 27th","question":""}`
}

// A change travels the wire the product's answers travel: the model looks the row up, submits, and
// its intent and reminder_id are read by the same schema the deployment runs. The run then rewrites
// the row it named instead of adding one.
func TestWorkerChangesTheRowAWireAnswerNames(t *testing.T) {
	f := newFixture(t, answerWith(oneOff("Buy glue", "2026-10-25", "18:00")))
	wantStatus(t, f.runText(t, "buy glue at 6pm tomorrow"), store.JobComplete)

	id := f.reminderID(t)
	f.withWire(t,
		toolCallAnswer(t, toolFindReminders, `{"query":"glue"}`),
		toolCallAnswer(t, toolSubmitPlan, `{}`),
		gatewayAnswer(t, "stop", changeAnswer(id)),
	)

	stored := wantStatus(t, f.runText(t, "make it 8pm on the 27th"), store.JobComplete)
	if got := stored.Conversation[len(stored.Conversation)-1].Text; got != updateReply {
		t.Fatalf("reply = %q, want the change's", got)
	}
	if got := f.localDateTime(t, id); got != "2026-10-27T20:00" {
		t.Fatalf("local = %q, want the place the wire answer named", got)
	}
	if n := f.count(t, `SELECT count(*) FROM Reminder`); n != 1 {
		t.Fatalf("reminders = %d, want the change in place rather than a second row", n)
	}
	detail := f.agentTrace(t, stored.JobID)
	if detail["intent"] != IntentUpdate || fmt.Sprint(detail["reminderIds"]) != fmt.Sprintf("[%d]", id) {
		t.Fatalf("trace = %+v, want the verb and the row it named", detail)
	}
}

// A row the model names but never looked up is refused, and the retry is the loop's own: the fault
// is the note the model reads, and the id it then names is one this run's own lookup printed.
func TestWorkerRefusesARowTheRunNeverLookedUp(t *testing.T) {
	f := newFixture(t, answerWith(oneOff("Buy glue", "2026-10-25", "18:00")))
	wantStatus(t, f.runText(t, "buy glue at 6pm tomorrow"), store.JobComplete)

	id := f.reminderID(t)
	f.withWire(t,
		toolCallAnswer(t, toolSubmitPlan, `{}`),
		gatewayAnswer(t, "stop", changeAnswer(99)),
		toolCallAnswer(t, toolFindReminders, `{"query":"glue"}`),
		toolCallAnswer(t, toolSubmitPlan, `{}`),
		gatewayAnswer(t, "stop", changeAnswer(id)),
	)

	stored := wantStatus(t, f.runText(t, "make it 8pm on the 27th"), store.JobComplete)
	faults := strings.Join(f.lintFaults(t, stored.JobID), "\n")
	if !strings.Contains(faults, "reminder_id 99 was not one of this run's own find_reminders results") {
		t.Fatalf("lint = %q, want the whitelist's own words", faults)
	}
	if got := f.localDateTime(t, id); got != "2026-10-27T20:00" {
		t.Fatalf("local = %q, want the retry's change written in place", got)
	}
	if n := f.count(t, `SELECT count(*) FROM Reminder`); n != 1 {
		t.Fatalf("reminders = %d, want no row for the refused answer", n)
	}
}

// A row that is called off between the lookup and the commit has nowhere to put the change: the
// turn fails, the table keeps the row it had, and the Trace says the rewrite lost its row.
func TestWorkerFailsAChangeWhoseRowLeftTheSchedule(t *testing.T) {
	f := newFixture(t, answerWith(oneOff("Buy glue", "2026-10-25", "18:00")))
	wantStatus(t, f.runText(t, "buy glue at 6pm tomorrow"), store.JobComplete)

	id := f.reminderID(t)
	// The provider calls the row off just before it answers the strict call, which is the window
	// the rewrite's predicate is there for: looked up while resolved, committed after it is not.
	asked := 0
	var mu sync.Mutex
	client := modelServer(t, Settings{Model: "stub"}, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		asked++
		answer := toolCallAnswer(t, toolFindReminders, `{"query":"glue"}`)
		switch asked {
		case 2:
			answer = toolCallAnswer(t, toolSubmitPlan, `{}`)
		case 3:
			if _, err := store.NewReminderStore(f.handle.Write, f.handle.Read).Cancel(
				context.Background(), f.userID, id); err != nil {
				t.Errorf("call the row off: %v", err)
			}
			answer = gatewayAnswer(t, "stop", changeAnswer(id))
		}
		mu.Unlock()
		_, _ = w.Write(answer)
	})
	f.loop.Prescreen = Stage{Model: f.model, Name: "prescreen"}
	f.loop.Planner = Stage{Model: client, Name: "stub"}

	stored := f.runText(t, "make it 8pm on the 27th")
	if stored.Status != store.JobOpen || stored.RetryCount != 1 {
		t.Fatalf("job = %q retry %d, want the failed turn requeued", stored.Status, stored.RetryCount)
	}
	if detail := f.agentTrace(t, stored.JobID); detail["stale"] != true {
		t.Fatalf("trace = %+v, want the lost row recorded", detail)
	}
	if got := f.localDateTime(t, id); got != "2026-10-25T18:00" {
		t.Fatalf("local = %q, want the row untouched", got)
	}
	if n := f.count(t, `SELECT count(*) FROM Reminder`); n != 1 {
		t.Fatalf("reminders = %d, want no row invented for a change that could not land", n)
	}
}

// A call-off travels the wire too: the answer every real one is — an intent and nothing else —
// passes the schema, the linter and the commit with no row to describe. The seeded row keeps its
// status, because the agent answering "cancel" is not the app cancelling it.
func TestWorkerAnswersACallOffFromAWireAnswer(t *testing.T) {
	f := newFixture(t, answerWith(oneOff("Dentist", "2026-10-25", "18:00")))
	wantStatus(t, f.runText(t, "dentist at 6pm tomorrow"), store.JobComplete)
	id := f.reminderID(t)

	f.withWire(t,
		toolCallAnswer(t, toolSubmitPlan, `{}`),
		gatewayAnswer(t, "stop", `{"kind":"records","ops":[{"intent":"cancel","reminder_id":0,"text":"","zone":"",`+
			`"day":"","time":"","recurrence":""}],"memories":[],"evidence":"","question":""}`),
	)

	stored := wantStatus(t, f.runText(t, "cancel the dentist reminder"), store.JobComplete)
	if got := stored.Conversation[len(stored.Conversation)-1].Text; got != cancelReply {
		t.Fatalf("reply = %q, want the call-off's own sentence", got)
	}
	if n := f.count(t, `SELECT count(*) FROM Reminder`); n != 1 {
		t.Fatalf("reminders = %d, want the seeded row left alone", n)
	}
	var status string
	if err := f.handle.Read.QueryRow(
		`SELECT Status FROM Reminder WHERE ReminderId = ?`, id).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != store.ReminderResolved {
		t.Fatalf("status = %q, want the row still resolved", status)
	}
}

// A change names only the field it changes: the clock arrives on its own, and the row keeps its
// words, its zone, the day it fell on and its repeat, because the server fills the rest.
func TestWorkerChangesWithOnlyTheClock(t *testing.T) {
	f := newFixture(t, answerWith(oneOff("Buy glue", "2026-10-27", "18:00")))
	wantStatus(t, f.runText(t, "buy glue at 6pm tomorrow"), store.JobComplete)
	id := f.reminderID(t)

	f.model.turns = []Answer{change(id, Record{Kind: KindReminder, Time: "20:00"})}
	f.model.lookUp = "glue"

	wantStatus(t, f.runText(t, "make it 8pm"), store.JobComplete)
	if got := f.localDateTime(t, id); got != "2026-10-27T20:00" {
		t.Fatalf("local = %q, want the clock moved and the day kept", got)
	}
	var text, timezone string
	if err := f.handle.Read.QueryRow(
		`SELECT Text, Timezone FROM Reminder WHERE ReminderId = ?`, id).Scan(&text, &timezone); err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if text != "Buy glue" || timezone != zone {
		t.Fatalf("row = %q in %q, want the row's own words and zone", text, timezone)
	}
	if n := f.count(t, `SELECT count(*) FROM Reminder`); n != 1 {
		t.Fatalf("reminders = %d, want the change in place", n)
	}
}

// A capture that names no place rings in the user's own zone: the model does not have to write
// back a zone it was handed.
func TestWorkerCreatesInTheUsersOwnZone(t *testing.T) {
	f := newFixture(t, answerWith(Record{Kind: KindReminder, Text: "Buy glue",
		Date: "2026-10-27", Time: "18:00", Recurrence: RecurrenceNone}))
	wantStatus(t, f.runText(t, "buy glue at 6pm tomorrow"), store.JobComplete)

	var timezone, local string
	if err := f.handle.Read.QueryRow(
		`SELECT Timezone, LocalDateTime FROM Reminder`).Scan(&timezone, &local); err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if timezone != zone || local != "2026-10-27T18:00" {
		t.Fatalf("row = %q %q, want the user's own zone", timezone, local)
	}
}

// A change is a delta, but it is still linted: every field it does name is checked by the same
// rules, on the record the fill handed lint, so a clock or a zone the server cannot place is
// refused before anything is written.
func TestWorkerLintsTheFieldsAChangeNames(t *testing.T) {
	cases := []struct {
		name   string
		record Record
		want   string
	}{
		{"a clock that is not one", Record{Kind: KindReminder, Time: "19:60"}, `time "19:60" is not HH:MM`},
		{"a clock that has gone", Record{Kind: KindReminder, Day: "today", Time: "09:00"}, `time "09:00" is in the past`},
		{"a zone that is not one", Record{Kind: KindReminder, Zone: "Mars/Olympus"}, `zone "Mars/Olympus" is not an IANA zone`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, answerWith(oneOff("Buy glue", "2026-10-27", "18:00")))
			wantStatus(t, f.runText(t, "buy glue at 6pm tomorrow"), store.JobComplete)
			id := f.reminderID(t)

			refused := change(id, test.record)
			f.model.turns = []Answer{refused, refused}
			f.model.lookUp = "glue"

			job := wantStatus(t, f.runText(t, "make it later"), store.JobFailed)
			if faults := strings.Join(f.lintFaults(t, job.JobID), "\n"); !strings.Contains(faults, test.want) {
				t.Fatalf("lint = %q, want it to name %q", faults, test.want)
			}
			if got := f.localDateTime(t, id); got != "2026-10-27T18:00" {
				t.Fatalf("local = %q, want the row untouched by a refused change", got)
			}
		})
	}
}

// An ask exit commits nothing, so it costs no tag call: tagging runs at commit, not at accept.
func TestWorkerDoesNotTagOnAnAskExit(t *testing.T) {
	f := newFixture(t, asked("When should I remind you about buying glue?"))
	f.model.tags = []string{"milk"}
	wantStatus(t, f.runText(t, "buy glue when I'm home"), store.JobClarifying)
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
	f := newFixture(t, Answer{Kind: AnswerRecords,
		Ops:      []Op{{Intent: IntentCreate, Record: oneOff("Lunch at Ikea", "2026-10-30", "13:00")}},
		Memories: []string{"I have an Ikea voucher"},
	})
	f.model.tags = []string{"ikea", "voucher", "lunch"}
	stored := wantStatus(t, f.runText(t, "lunch at Ikea this Friday at 1pm, and I have an Ikea voucher"), store.JobComplete)
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

// Tagger is off the run's path: a tagger that never answers still leaves the Memory committed
// and the Job closed, because by then the user has already read the reply.
func TestWorkerCommitsWithoutWaitingForTheTagger(t *testing.T) {
	f := newFixture(t, aboutAFact("Milk is cheaper at store A"))
	f.model.tags = []string{"milk", "grocery", "price"}
	f.model.tagBlock = true
	f.loop.AttemptTimeout = 50 * time.Millisecond

	// The run returns with the tagger still held.
	stored := wantStatus(t, f.runText(t, "remember that milk is cheaper at store A"), store.JobComplete)
	if got := stored.Conversation[len(stored.Conversation)-1].Text; got != memoryReply {
		t.Fatalf("reply = %q, want %q", got, memoryReply)
	}
	// The row is there and its features are not: they arrive later, or not at all.
	f.tagCalls(t, 1)
	if text, tags := f.memoryRow(t); text != "Milk is cheaper at store A" || tags != "" {
		t.Fatalf("memory = %q %q, want the fact committed untagged", text, tags)
	}
}
