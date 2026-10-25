package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"blurt/internal/config"
	"blurt/internal/db"
	"blurt/internal/push"
	"blurt/internal/queue"
	"blurt/internal/store"
)

// pinned is the anchor every phrase resolves against: a Saturday evening in
// Singapore. Singapore is UTC+8 year round, so a fixed zone stands in for it and
// the test never leans on the tz database.
var pinned = time.Date(2026, 10, 24, 18, 30, 0, 0, time.FixedZone("Asia/Singapore", 8*60*60))

// scriptedModel answers a fixed list of turns, so the loop's checking and
// committing are testable without a model. It keeps every Input it was handed,
// which is how the retry-with-refusals behaviour is asserted.
type scriptedModel struct {
	turns  []Turn
	inputs []Input
	err    error
	// question is what the question writer answers, and askErr what it fails with. Neither is scripted
	// by default, which leaves the canned sentence standing wherever a test is not about the wording.
	question string
	asks     []Input
	askErr   error
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

// Ask answers with what the test scripted, and with nothing when it scripted nothing: an empty
// question is unusable, so the canned sentence stands.
func (p *scriptedModel) Ask(_ context.Context, in Input) (string, error) {
	p.asks = append(p.asks, in)
	if p.askErr != nil {
		return "", p.askErr
	}
	return p.question, nil
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
				Recall:    Reads{Reminders: store.NewReminderStore(handle.Read), Memories: store.NewMemoryStore(handle.Read)},
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
		store.Message{Role: "user", Text: text, TZ: "Asia/Singapore"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := f.worker.AgentQueue.Publish(context.Background(), job.JobID, 1); err != nil {
		t.Fatalf("publish job: %v", err)
	}
	return job
}

func (f *fixture) reply(t *testing.T, jobID int64, text, zone string) {
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

// refusalCodes reads the codes the newest run refused a write with. It is how a test names the
// lint that fired: two of them share one user-facing question.
func (f *fixture) refusalCodes(t *testing.T, jobID int64) []string {
	t.Helper()
	refused, ok := f.agentTrace(t, jobID)["refused"].([]any)
	if !ok {
		t.Fatal("the trace recorded no refusals")
	}
	codes := make([]string, 0, len(refused))
	for _, item := range refused {
		refusal, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("refusal = %T, want an object", item)
		}
		code, ok := refusal["Code"].(string)
		if !ok {
			t.Fatalf("refusal = %v, want a code", refusal)
		}
		codes = append(codes, code)
	}
	return codes
}

// reminderWrite is the shape the model produces for a clear capture.
func reminderWrite(text, phrase string) Write {
	return Write{Kind: KindReminder, Text: text, Timezone: "Asia/Singapore", TimePhrase: phrase}
}

// TestWorkerCommitsAReminder is the happy path: a clear capture becomes one
// Reminder whose UTC instant the server derived, and the Job closes.
func TestWorkerCommitsAReminder(t *testing.T) {
	f := newFixture(t, Turn{Say: "Reminder set for tomorrow at 6pm.",
		Writes: []Write{reminderWrite("Buy glue", "tomorrow at 6pm")}})
	job := f.textJob(t, "buy glue at 6pm tomorrow")

	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	if len(stored.Conversation) != 2 || stored.Conversation[1].Role != "assistant" ||
		stored.Conversation[1].Text != "Reminder set for tomorrow at 6pm." {
		t.Fatalf("conversation = %+v", stored.Conversation)
	}

	var text, zone, local, emit, status string
	var source int64
	err := f.handle.Read.QueryRow(
		`SELECT Text, Timezone, LocalDateTime, EmitAtUtc, Status, SourceJobId FROM Reminder`).
		Scan(&text, &zone, &local, &emit, &status, &source)
	if err != nil {
		t.Fatalf("read reminder: %v", err)
	}
	// Human time is what was stored; the UTC instant is derived from it and is
	// rebuildable, which is the whole time model in one assertion.
	if local != "2026-10-25T18:00" {
		t.Fatalf("local date time = %q, want 2026-10-25T18:00", local)
	}
	if emit != "2026-10-25T10:00:00Z" {
		t.Fatalf("emit at utc = %q, want 2026-10-25T10:00:00Z", emit)
	}
	if text != "Buy glue" || zone != "Asia/Singapore" || status != store.ReminderResolved {
		t.Fatalf("reminder = %q %q %q", text, zone, status)
	}
	if source != job.JobID {
		t.Fatalf("source job = %d, want %d", source, job.JobID)
	}

	detail := f.agentTrace(t, job.JobID)
	if detail["outcome"] != store.JobComplete || detail["turns"] != float64(1) {
		t.Fatalf("trace = %v", detail)
	}
	if f.count(t, `SELECT count(*) FROM Trace WHERE JobId = ? AND Stage = 'agent'`, job.JobID) != 1 {
		t.Fatal("want exactly one agent trace for one committed step")
	}
	if f.pusher.calls != 0 {
		t.Fatalf("pushes = %d, want none for a settled Job", f.pusher.calls)
	}
}

// A fact becomes a Memory and is findable by BM25 straight away.
func TestWorkerCommitsAMemory(t *testing.T) {
	f := newFixture(t, Turn{Say: "Noted.",
		Writes: []Write{{Kind: KindMemory, Text: "Milk is cheaper at store A than store B"}}})
	job := f.textJob(t, "remember that milk is cheaper at store A than store B")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	if f.count(t, `SELECT count(*) FROM Reminder`) != 0 {
		t.Fatal("a Memory capture created a Reminder")
	}
	found, err := store.NewMemoryStore(f.handle.Read).Search(
		context.Background(), f.userID, "milk cheaper store A", 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 1 || found[0].Text != "Milk is cheaper at store A than store B" {
		t.Fatalf("search found %+v", found)
	}
}

// A capture that names no time, or nothing at all, is a question rather than a
// silent completion.
func TestWorkerAsksWhenItCannotAct(t *testing.T) {
	cases := []struct {
		name    string
		turn    Turn
		text    string
		refused int
		asks    int
	}{
		{"the time cannot be pinned",
			Turn{Say: "Hmm.", Writes: []Write{reminderWrite("Buy glue", "when I am home from school")}},
			"buy glue when I'm home from school", 1, 1},
		{"nothing was proposed", Turn{Say: "Hmm."}, "blah", 0, 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, test.turn)
			job := f.textJob(t, test.text)

			f.runOnce(t)

			stored := f.load(t, job.JobID)
			if stored.Status != store.JobClarifying {
				t.Fatalf("status = %q, want clarifying", stored.Status)
			}
			if last := stored.Conversation[len(stored.Conversation)-1]; last.Text != defaultQuestion {
				t.Fatalf("question = %+v", last)
			}
			if f.count(t, `SELECT count(*) FROM Reminder`) != 0 {
				t.Fatal("an unactionable capture committed a Reminder")
			}
			detail := f.agentTrace(t, job.JobID)
			if detail["outcome"] != store.JobClarifying {
				t.Fatalf("trace = %v", detail)
			}
			refused, _ := detail["refused"].([]any)
			if len(refused) != test.refused {
				t.Fatalf("refused = %v, want %d", detail["refused"], test.refused)
			}
			if len(f.model.asks) != test.asks {
				t.Fatalf("writer calls = %d, want %d", len(f.model.asks), test.asks)
			}
			if f.pusher.calls != 1 || f.pusher.title != "Blurt needs one thing" {
				t.Fatalf("pushes = %d %q", f.pusher.calls, f.pusher.title)
			}
		})
	}
}

// The ask exit drops writes that passed beside the question, logs the mixed
// outcome as an error, and keeps the dropped write in the Trace for prompt work.
func TestWorkerDropsPassingWritesAndLogsTheMixedOutcome(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(previous)

	f := newFixture(t, Turn{Say: "Noted, and when should I remind you?", Writes: []Write{
		{Kind: KindMemory, Text: "Milk is cheaper at store A"},
		reminderWrite("Buy glue", "when I am home"),
	}})
	job := f.textJob(t, "remember milk is cheaper at store A, and buy glue when I'm home")

	f.runOnce(t)

	if !strings.Contains(logged.String(), "agent mixed outcome") {
		t.Fatalf("log = %q, want the mixed-outcome error", logged.String())
	}
	if stored := f.load(t, job.JobID); stored.Status != store.JobClarifying {
		t.Fatalf("status = %q, want clarifying", stored.Status)
	}
	if f.count(t, `SELECT count(*) FROM Memory`) != 0 {
		t.Fatal("the ask exit committed the passing Memory")
	}
	if f.count(t, `SELECT count(*) FROM Reminder`) != 0 {
		t.Fatal("the ambiguous Reminder committed")
	}
	dropped, ok := f.agentTrace(t, job.JobID)["dropped"].([]any)
	if !ok || len(dropped) != 1 {
		t.Fatalf("dropped = %v, want the passing Memory", f.agentTrace(t, job.JobID)["dropped"])
	}
	if item, ok := dropped[0].(map[string]any); !ok || item["Text"] != "Milk is cheaper at store A" {
		t.Fatalf("dropped[0] = %v", dropped[0])
	}
}

// A second Reminder in the same turn is refused by the per-Job guardrail, not by
// the time lint: the first one holds the Job's single slot.
func TestWorkerRefusesASecondReminderInOneRun(t *testing.T) {
	f := newFixture(t, Turn{Say: "Reminder set.", Writes: []Write{
		reminderWrite("Buy glue", "tomorrow at 6pm"),
		reminderWrite("Take trash out", "tomorrow at 7pm"),
	}})
	job := f.textJob(t, "buy glue at 6pm tomorrow and take the trash out at 7pm")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobClarifying {
		t.Fatalf("status = %q, want clarifying", stored.Status)
	}
	if f.count(t, `SELECT count(*) FROM Reminder`) != 0 {
		t.Fatal("the ask exit committed the first Reminder")
	}
	if codes := f.refusalCodes(t, job.JobID); len(codes) != 1 || codes[0] != CheckJobReminderExists {
		t.Fatalf("refused = %v, want %s", codes, CheckJobReminderExists)
	}
	// The refusal is worth one revision; the empty turn after it asks the user.
	if len(f.model.inputs) != 2 {
		t.Fatalf("model calls = %d, want 2", len(f.model.inputs))
	}
}

// Memories are uncapped per Job: two in one crafted turn both land.
func TestWorkerCommitsSeveralMemoriesInOneTurn(t *testing.T) {
	f := newFixture(t, Turn{Say: "Noted.", Writes: []Write{
		{Kind: KindMemory, Text: "Milk is cheaper at store A"},
		{Kind: KindMemory, Text: "Bread is cheaper at store B"},
	}})
	job := f.textJob(t, "remember milk is cheaper at store A and bread is cheaper at store B")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	if f.count(t, `SELECT count(*) FROM Memory`) != 2 {
		t.Fatal("want both Memories")
	}
}

// A zero-writes turn whose say is a usable question is the ask signal: the split
// question renders as written, with no second model call.
func TestWorkerRendersTheSplitQuestionWithoutTheModel(t *testing.T) {
	const split = "I can only take one reminder at a time. Which one should I set first?"
	f := newFixture(t, Turn{Say: split})
	job := f.textJob(t, "dentist at 5pm and trash at 9pm")

	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobClarifying {
		t.Fatalf("status = %q, want clarifying", stored.Status)
	}
	if last := stored.Conversation[len(stored.Conversation)-1]; last.Text != split {
		t.Fatalf("question = %q, want the model's split question", last.Text)
	}
	if len(f.model.asks) != 0 {
		t.Fatalf("writer calls = %d, want none: say was usable", len(f.model.asks))
	}
	if f.count(t, `SELECT count(*) FROM Reminder`) != 0 || f.count(t, `SELECT count(*) FROM Memory`) != 0 {
		t.Fatal("a split question committed a record")
	}
}

// A clarification answer is the next run's capture, so the time the user answers
// with is the one that commits.
func TestWorkerCommitsTheAnsweredTime(t *testing.T) {
	f := newFixture(t,
		Turn{Say: "Hmm.", Writes: []Write{reminderWrite("Buy glue", "when I am home")}},
		Turn{},
		Turn{Say: "Reminder set.", Writes: []Write{reminderWrite("Buy glue", "tomorrow at 7pm")}},
	)
	job := f.textJob(t, "buy glue when I'm home")

	f.runOnce(t)
	if f.count(t, `SELECT count(*) FROM Reminder`) != 0 {
		t.Fatal("the unanswerable first run committed a Reminder")
	}
	f.reply(t, job.JobID, "at 7pm tomorrow", "Asia/Singapore")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	var local string
	if err := f.handle.Read.QueryRow(`SELECT LocalDateTime FROM Reminder`).Scan(&local); err != nil {
		t.Fatalf("read reminder: %v", err)
	}
	if local != "2026-10-25T19:00" {
		t.Fatalf("local date time = %q, want the answered 7pm", local)
	}
}

// The scripted model's split branch rides the same path end to end: it proposes
// nothing, the say renders as the question, and no record is committed.
func TestWorkerRunsTheFakeSplitBranch(t *testing.T) {
	f := newFixture(t)
	f.worker.Loop.Model = Fake{}
	job := f.textJob(t, "dentist at 5pm and trash at 9pm")

	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobClarifying {
		t.Fatalf("status = %q, want clarifying", stored.Status)
	}
	if last := stored.Conversation[len(stored.Conversation)-1]; last.Text != FakeSplitQuestion {
		t.Fatalf("question = %q, want the split question", last.Text)
	}
	if f.count(t, `SELECT count(*) FROM Reminder`) != 0 || f.count(t, `SELECT count(*) FROM Memory`) != 0 {
		t.Fatal("the split branch committed a record")
	}
}

// A refusal goes back to the model before it goes to the user: the second turn
// sees the check result, and a revised write still commits on the first try.
func TestWorkerRetriesWithTheRefusal(t *testing.T) {
	f := newFixture(t,
		Turn{Say: "When should I remind you?",
			Writes: []Write{reminderWrite("Buy glue", "when I am home")}},
		Turn{Say: "Reminder set for tomorrow at 6pm.",
			Writes: []Write{reminderWrite("Buy glue", "tomorrow at 6pm")}},
	)
	job := f.textJob(t, "buy glue at 6pm tomorrow")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	if len(f.model.inputs) != 2 {
		t.Fatalf("model calls = %d, want 2", len(f.model.inputs))
	}
	if len(f.model.inputs[0].Checks) != 0 {
		t.Fatal("the first turn was handed a refusal before any check had run")
	}
	checks := f.model.inputs[1].Checks
	if len(checks) != 1 || checks[0].Code != CheckTimeUnsupported || checks[0].Field != "timePhrase" {
		t.Fatalf("second turn checks = %+v", checks)
	}
	if f.count(t, `SELECT count(*) FROM Reminder`) != 1 {
		t.Fatal("want exactly one Reminder once the second turn passed")
	}
	if detail := f.agentTrace(t, job.JobID); detail["turns"] != float64(2) {
		t.Fatalf("trace turns = %v, want 2", detail["turns"])
	}
	if f.pusher.calls != 0 {
		t.Fatal("a Job that resolved on its second turn pushed")
	}
}

// A turn that refuses exactly what the last turn refused has nothing new to go on, so the
// Job asks instead of spending another model call on the same wall.
func TestWorkerAsksWhenTheModelRepeatsARefusal(t *testing.T) {
	bad := Turn{Say: "Hmm.", Writes: []Write{reminderWrite("Buy glue", "when I am home")}}
	f := newFixture(t, bad, bad, bad, bad)
	job := f.textJob(t, "buy glue when I'm home")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobClarifying {
		t.Fatalf("status = %q, want clarifying", stored.Status)
	}
	// The first refusal is worth revising against; the second is the same one, so the
	// loop stops there rather than at the budget of 3.
	if len(f.model.inputs) != 2 {
		t.Fatalf("model calls = %d, want 2", len(f.model.inputs))
	}
}

// The turn budget is the backstop: a model whose refusals keep *changing* never repeats
// itself, so only the cap ends the loop.
func TestWorkerStopsAtTheTurnBudget(t *testing.T) {
	noTime := Turn{Say: "Hmm.", Writes: []Write{reminderWrite("Buy glue", "when i'm home")}}
	badZone := Turn{Say: "Hmm.", Writes: []Write{{Kind: KindReminder, Text: "Buy glue",
		Timezone: "Mars/Olympus", TimePhrase: "tomorrow at 6pm"}}}
	f := newFixture(t, noTime, badZone, noTime, badZone, noTime)
	f.worker.Loop.MaxTurns = 3
	job := f.textJob(t, "buy glue when I'm home at 6pm tomorrow")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobClarifying {
		t.Fatalf("status = %q, want clarifying", stored.Status)
	}
	if len(f.model.inputs) != 3 {
		t.Fatalf("model calls = %d, want the budget of 3", len(f.model.inputs))
	}
}

// A transient failure retries, the last one trips the poison pill, and a refusal
// no retry can fix fails the Job at once.
func TestWorkerFailurePaths(t *testing.T) {
	transient := errors.New("agent: call model: connection refused")
	cases := []struct {
		name     string
		err      error
		atLimit  bool
		status   string
		attempt  int
		outcome  string
		fallback bool
		pushes   int
	}{
		{"transient", transient, false, store.JobOpen, 1, "retry", false, 0},
		{"poison pill", transient, true, store.JobFailed, maxAttempts, "error", true, 1},
		{"permanent refusal", &Error{Status: http.StatusUnauthorized, Message: "missing or invalid api key"},
			false, store.JobFailed, 1, "rejected", true, 1},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			f.model.err = test.err
			job := f.textJob(t, "buy glue")
			if test.atLimit {
				if err := f.jobs.Retry(context.Background(), store.AgentStage, job.JobID, maxAttempts-1,
					map[string]any{"outcome": "retry"}); err != nil {
					t.Fatalf("seed attempts: %v", err)
				}
			}

			f.runOnce(t)

			stored := f.load(t, job.JobID)
			if stored.Status != test.status || stored.RetryCount != test.attempt {
				t.Fatalf("stored = %+v, want %s at attempt %d", stored, test.status, test.attempt)
			}
			if test.fallback {
				if got := stored.Conversation[len(stored.Conversation)-1].Text; got != FallbackMessage {
					t.Fatalf("fallback = %q, want %q", got, FallbackMessage)
				}
			}
			if f.pusher.calls != test.pushes {
				t.Fatalf("push calls = %d, want %d", f.pusher.calls, test.pushes)
			}
			if detail := f.agentTrace(t, job.JobID); detail["outcome"] != test.outcome {
				t.Fatalf("trace = %v", detail)
			}
		})
	}
}

// A Job that already holds a Reminder and is re-run — which only failed-job recovery
// can do — refuses a second one. The per-Job guardrail has to hold across runs even
// though the insert would also skip an identical row.
func TestWorkerRefusesASecondReminderOnRecovery(t *testing.T) {
	f := newFixture(t,
		Turn{Say: "Reminder set for tomorrow at 6pm.",
			Writes: []Write{reminderWrite("Buy glue", "tomorrow at 6pm")}},
		Turn{Say: "Reminder set for tomorrow at 7pm.",
			Writes: []Write{reminderWrite("Take trash out", "tomorrow at 7pm")}},
	)
	job := f.textJob(t, "buy glue at 6pm tomorrow")

	f.runOnce(t)
	if f.count(t, `SELECT count(*) FROM Reminder`) != 1 {
		t.Fatal("want one Reminder after the first pass")
	}
	// A failed Job takes a reply and is requeued, which is the one way a Job that
	// already committed can run again.
	if err := f.jobs.Fail(context.Background(), store.AgentStage, job.JobID, 3, "I couldn't process that.", nil); err != nil {
		t.Fatalf("fail the job: %v", err)
	}
	f.reply(t, job.JobID, "and take the trash out at 7pm", "Asia/Singapore")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobClarifying {
		t.Fatalf("status = %q, want clarifying", stored.Status)
	}
	if f.count(t, `SELECT count(*) FROM Reminder`) != 1 {
		t.Fatal("the recovered pass committed a second Reminder")
	}
	if codes := f.refusalCodes(t, job.JobID); len(codes) != 1 || codes[0] != CheckJobReminderExists {
		t.Fatalf("refused = %v, want %s", codes, CheckJobReminderExists)
	}
}

// The event is the half of a Reminder a model can invent, and a clock inside it
// is the leak the prompt forbids; both are refused before anything is stored.
func TestWorkerRefusesAnUnsupportedEvent(t *testing.T) {
	cases := []struct {
		name  string
		event string
		code  string
	}{
		{"words the user never said", "Call the plumber", CheckEventUnsupported},
		{"a clock in the event", "Buy glue at 6pm", CheckEventHasTime},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, Turn{Say: "Reminder set.",
				Writes: []Write{reminderWrite(test.event, "tomorrow at 6pm")}})
			job := f.textJob(t, "buy glue at 6pm tomorrow")

			f.runOnce(t)

			if stored := f.load(t, job.JobID); stored.Status != store.JobClarifying {
				t.Fatalf("status = %q, want clarifying", stored.Status)
			}
			if f.count(t, `SELECT count(*) FROM Reminder`) != 0 {
				t.Fatal("an unsupported event committed a Reminder")
			}
			if codes := f.refusalCodes(t, job.JobID); len(codes) != 1 || codes[0] != test.code {
				t.Fatalf("refused = %v, want %s", codes, test.code)
			}
		})
	}
}

// One event at two different times is a conflict, not a duplicate: both variants
// reach the question writer, and neither is durable at the ask exit. The answer
// re-plans the Job and the variant it names is the one that lands.
func TestWorkerDropsAConflictedEventAndCommitsTheAnswer(t *testing.T) {
	f := newFixture(t,
		Turn{Say: "Reminder set.", Writes: []Write{
			reminderWrite("Buy glue", "tomorrow at 6pm"),
			reminderWrite("Buy glue", "tomorrow at 7pm"),
		}},
		Turn{},
		Turn{Say: "Reminder set for tomorrow at 7pm.", Writes: []Write{
			reminderWrite("Buy glue", "tomorrow at 7pm"),
		}},
	)
	job := f.textJob(t, "buy glue at 6pm tomorrow")

	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobClarifying {
		t.Fatalf("status = %q, want clarifying", stored.Status)
	}
	if reminders := f.count(t, `SELECT count(*) FROM Reminder`); reminders != 0 {
		t.Fatalf("reminders = %d, want none: the first variant dies with the conflict", reminders)
	}
	if codes := f.refusalCodes(t, job.JobID); len(codes) != 1 || codes[0] != CheckConflict {
		t.Fatalf("refused = %v, want %s", codes, CheckConflict)
	}
	// Both variants reach the question writer: the refused one in Checks, and the
	// sibling that passed before the conflict in Accepted.
	ask := f.model.asks[0]
	if len(ask.Checks) != 1 || ask.Checks[0].Write.TimePhrase != "tomorrow at 7pm" {
		t.Fatalf("ask checks = %+v", ask.Checks)
	}
	if len(ask.Accepted) != 1 || ask.Accepted[0].TimePhrase != "tomorrow at 6pm" {
		t.Fatalf("ask accepted = %+v", ask.Accepted)
	}

	// The answer re-plans the Job and the chosen variant is what lands.
	f.reply(t, job.JobID, "7pm", "Asia/Singapore")
	f.runOnce(t)

	stored = f.load(t, job.JobID)
	if stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	var local string
	if err := f.handle.Read.QueryRow(`SELECT LocalDateTime FROM Reminder`).Scan(&local); err != nil {
		t.Fatalf("read reminder: %v", err)
	}
	if local != "2026-10-25T19:00" {
		t.Fatalf("local date time = %q, want the answered 7pm", local)
	}
}

// blockingModel waits until its context is done, so the run's hard deadline is
// what ends it.
type blockingModel struct{}

func (blockingModel) Propose(ctx context.Context, _ Input) (Turn, error) {
	<-ctx.Done()
	return Turn{}, ctx.Err()
}

func (blockingModel) Ask(ctx context.Context, _ Input) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// The hard deadline bounds the run itself, not just a revision: a model that waits
// past it retries, or fails when it trips the poison pill.
func TestWorkerStopsAtTheHardDeadline(t *testing.T) {
	cases := []struct {
		name    string
		atLimit bool
		status  string
		attempt int
	}{
		{"before the poison pill", false, store.JobOpen, 1},
		{"at the poison pill", true, store.JobFailed, maxAttempts},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			f.worker.Loop.Model = blockingModel{}
			f.worker.Loop.Timeout = 20 * time.Millisecond
			job := f.textJob(t, "buy glue at 6pm tomorrow")
			if test.atLimit {
				if err := f.jobs.Retry(context.Background(), store.AgentStage, job.JobID, maxAttempts-1,
					map[string]any{"outcome": "retry"}); err != nil {
					t.Fatalf("seed attempts: %v", err)
				}
			}

			f.runOnce(t)

			stored := f.load(t, job.JobID)
			if stored.Status != test.status || stored.RetryCount != test.attempt {
				t.Fatalf("stored = %+v, want %s at attempt %d", stored, test.status, test.attempt)
			}
			if last := stored.Conversation[len(stored.Conversation)-1]; test.atLimit && last.Text != FallbackMessage {
				t.Fatalf("fallback = %q, want %q", last.Text, FallbackMessage)
			}
		})
	}
}

// A loop built without a turn budget reads config's single default.
func TestMaxTurnsFallsBackToConfig(t *testing.T) {
	if got := (&AgentLoop{}).maxTurns(); got != config.DefaultMaxTurns {
		t.Fatalf("maxTurns = %d, want %d", got, config.DefaultMaxTurns)
	}
}

// The question is the model's own wording where it writes a usable one: a capture's edge case is
// exactly what three canned sentences cannot cover.
func TestWorkerWritesTheQuestionWithTheModel(t *testing.T) {
	f := newFixture(t, Turn{Say: "Hmm.", Writes: []Write{reminderWrite("Buy glue", "when I am home")}})
	f.model.question = "When should I remind you about the glue run?"
	job := f.textJob(t, "buy glue when I'm home")

	f.runOnce(t)

	stored := f.load(t, job.JobID)
	if last := stored.Conversation[len(stored.Conversation)-1]; last.Text != f.model.question {
		t.Fatalf("question = %q, want the model's own", last.Text)
	}
	// The writer is handed what the question is about and nothing else: no retrieval, because the
	// records turn is not the one being answered.
	if len(f.model.asks) != 1 || len(f.model.asks[0].Checks) != 1 || len(f.model.asks[0].Nearby) != 0 {
		t.Fatalf("ask inputs = %+v", f.model.asks)
	}
}

// A written question that would mislead the user never reaches them, and neither does a failure: the
// canned sentence stands.
func TestWorkerFallsBackWhenTheWrittenQuestionIsUnusable(t *testing.T) {
	cases := []struct {
		name     string
		question string
		askErr   error
	}{
		{"names a clock the user did not say", "Should I set it for 6pm?", nil},
		{"not a question", "I could not tell what you meant.", nil},
		{"nothing written", "", nil},
		{"the writer failed", "", errors.New("agent: call model: connection refused")},
		{"a paragraph", strings.Repeat("when ", 50) + "?", nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, Turn{Say: "Hmm.", Writes: []Write{reminderWrite("Buy glue", "when I am home")}})
			f.model.question, f.model.askErr = test.question, test.askErr
			job := f.textJob(t, "buy glue when I'm home")

			f.runOnce(t)

			stored := f.load(t, job.JobID)
			want := defaultQuestion
			if last := stored.Conversation[len(stored.Conversation)-1]; last.Text != want {
				t.Fatalf("question = %q, want the canned %q", last.Text, want)
			}
		})
	}
}

// repeated is the loop's terminator, tested as the rule it is rather than through the
// model: the same refusal by code and field stops the revision, and any change means the
// model has something new to work with.
func TestRepeated(t *testing.T) {
	cases := []struct {
		name     string
		previous []Check
		current  []Check
		want     bool
	}{
		{"the same refusal", []Check{{Code: CheckTimeUnresolved, Field: "timePhrase"}},
			[]Check{{Code: CheckTimeUnresolved, Field: "timePhrase"}}, true},
		{"the same code on another field", []Check{{Code: CheckTimeUnresolved, Field: "timePhrase"}},
			[]Check{{Code: CheckTimeUnresolved, Field: "timezone"}}, false},
		{"another code", []Check{{Code: CheckTimeUnresolved, Field: "timePhrase"}},
			[]Check{{Code: CheckTimezoneInvalid, Field: "timezone"}}, false},
		{"a shorter refusal list", []Check{{Code: CheckTimeUnresolved, Field: "timePhrase"},
			{Code: CheckTextMissing, Field: "text"}},
			[]Check{{Code: CheckTimeUnresolved, Field: "timePhrase"}}, false},
		{"no previous turn", nil, []Check{{Code: CheckTimeUnresolved, Field: "timePhrase"}}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := repeated(test.previous, test.current); got != test.want {
				t.Fatalf("repeated = %v, want %v", got, test.want)
			}
		})
	}
}

// fakeTagger is the Tagger port with a scripted answer, so the loop's tagging is
// tested without a model. It keeps what it was asked about, which is how an
// unnecessary call is caught.
type fakeTagger struct {
	tags  []string
	err   error
	calls []string
}

func (f *fakeTagger) Tags(_ context.Context, text string) ([]string, error) {
	f.calls = append(f.calls, text)
	if f.err != nil {
		return nil, f.err
	}
	return f.tags, nil
}

// The generated features land on the Memory the run commits, comma-joined onto
// the row the retriever indexes.
func TestWorkerTagsTheMemoryItCommits(t *testing.T) {
	f := newFixture(t, Turn{Say: "Noted.",
		Writes: []Write{{Kind: KindMemory, Text: "Milk is cheaper at store A than store B"}}})
	tagger := &fakeTagger{tags: []string{"milk", "grocery", "price"}}
	f.worker.Loop.Tagger = tagger
	job := f.textJob(t, "remember that milk is cheaper at store A than store B")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	var tags string
	if err := f.handle.Read.QueryRow(`SELECT Tags FROM Memory`).Scan(&tags); err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if tags != "milk,grocery,price" {
		t.Fatalf("tags = %q, want the generated features", tags)
	}
	if len(tagger.calls) != 1 || tagger.calls[0] != "Milk is cheaper at store A than store B" {
		t.Fatalf("tagger saw %v, want one call about the Memory", tagger.calls)
	}
}

// Tagging is advisory, like the gate and recall: a tagger that fails leaves the
// Memory without features rather than failing a capture the user already made.
func TestWorkerDropsTagsWhenTheTaggerFails(t *testing.T) {
	f := newFixture(t, Turn{Say: "Noted.",
		Writes: []Write{{Kind: KindMemory, Text: "Milk is cheaper at store A than store B"}}})
	f.worker.Loop.Tagger = &fakeTagger{err: errors.New("agent: call model: connection refused")}
	job := f.textJob(t, "remember that milk is cheaper at store A than store B")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want the capture to survive a failing tagger", stored.Status)
	}
	var tags string
	if err := f.handle.Read.QueryRow(`SELECT Tags FROM Memory`).Scan(&tags); err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if tags != "" {
		t.Fatalf("tags = %q, want none", tags)
	}
}

// An ask exit commits nothing, so it costs no tag call: tagging runs at commit,
// not at accept.
func TestWorkerDoesNotTagOnAnAskExit(t *testing.T) {
	f := newFixture(t, Turn{Say: "When should I remind you?"})
	tagger := &fakeTagger{tags: []string{"milk"}}
	f.worker.Loop.Tagger = tagger
	job := f.textJob(t, "buy glue when I'm home")

	f.runOnce(t)

	if stored := f.load(t, job.JobID); stored.Status != store.JobClarifying {
		t.Fatalf("status = %q, want clarifying", stored.Status)
	}
	if len(tagger.calls) != 0 {
		t.Fatalf("tagger saw %v, want no call for a run that committed nothing", tagger.calls)
	}
}
