package l1

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"blurt/internal/agent"
	"blurt/internal/db"
	"blurt/internal/store"
)

// caseRun is one case's world: a fresh database, the case's rows already in it, the Job
// the loop reads, and the clock the case pinned.
type caseRun struct {
	c      Case
	handle *db.DB
	job    store.Job
	jobs   *store.JobStore
	loop   *agent.AgentLoop
	now    time.Time
}

// newCaseRun seeds one case and wires the loop the way production does, with the two
// gates stubbed and the model passed in. A nil model is the offline path: the golden test
// reads the prompt the loop would build without ever running it.
func newCaseRun(t *testing.T, c Case, model agent.Model, modelName string) *caseRun {
	t.Helper()
	ctx := context.Background()
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })

	user, err := store.NewUserStore(handle.Write, handle.Read).EnsureUser(ctx, "Eval", "eval@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	if err := seedCase(ctx, handle, user.UserID, c); err != nil {
		t.Fatalf("seed %s: %v", c.Name, err)
	}
	jobs := store.NewJobStore(handle.Write, handle.Read)
	job, err := jobs.Create(ctx, user.UserID, caseTurns(c)...)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	now, _, err := c.anchor()
	if err != nil {
		t.Fatalf("anchor %s: %v", c.Name, err)
	}
	return &caseRun{
		c: c, handle: handle, job: job, jobs: jobs, now: now,
		loop: &agent.AgentLoop{
			Store: jobs,
			Recall: agent.Retrieval{
				Reminders: store.NewReminderStore(handle.Write, handle.Read),
				Memories:  store.NewMemoryStore(handle.Read),
			},
			Planner:        agent.Stage{Model: model, Name: modelName},
			Prescreen:      agent.Stage{Model: sideStage{}},
			Verify:         agent.Stage{Model: sideStage{}},
			Tags:           agent.Stage{Model: sideStage{}},
			Pusher:         &recordedPush{},
			Timeout:        evalTimeout,
			AttemptTimeout: evalAttemptTimeout,
			Now:            func() time.Time { return now },
		},
	}
}

// caseTurns is the Conversation the model reads: the case's history, then the capture as
// the newest user turn. Only a user turn carries the device zone, because that is the
// anchor a relative time resolves against.
func caseTurns(c Case) []store.Message {
	turns := make([]store.Message, 0, len(c.History)+1)
	for _, turn := range c.History {
		message := store.Message{Role: turn.Role, Text: turn.Text}
		if turn.Role == "user" {
			message.TZ = c.CurrentTZ
		}
		turns = append(turns, message)
	}
	return append(turns, store.Message{Role: "user", Text: c.Capture, TZ: c.CurrentTZ})
}

// block is what the model reads beside the capture. It is the loop's own input assembly,
// so the golden pins the real prefetch rather than a hand-built stand-in.
func (r *caseRun) block(t *testing.T) string {
	t.Helper()
	in := r.loop.Input(context.Background(), r.job, r.now)
	return agent.ContextBlock(in, r.c.Capture)
}

// lastReply is the newest assistant turn: the server's own sentence on a commit, and the
// model's on a question.
func lastReply(job store.Job) string {
	for i := len(job.Conversation) - 1; i >= 0; i-- {
		turn := job.Conversation[i]
		if turn.Role == "assistant" && turn.Text != "" {
			return turn.Text
		}
	}
	return ""
}

// outcome is what one run left behind, read back out of SQLite: the Job's close, the rows
// it committed, the sentence the user reads, and the Trace row it wrote. None of it is the
// model's own account of itself.
type outcome struct {
	status    string
	reply     string
	reminders []store.Reminder
	memories  []string
	tools     []string
	trace     map[string]any
}

func (r *caseRun) outcome(t *testing.T, rec *recorder) outcome {
	t.Helper()
	ctx := context.Background()
	job, err := r.jobs.Get(ctx, r.job.JobID)
	if err != nil {
		t.Fatalf("reload job: %v", err)
	}
	got := outcome{status: job.Status, reply: lastReply(job)}
	if rec != nil {
		got.tools = rec.tools()
	}
	got.reminders = r.reminders(t, ctx)
	got.memories = r.memories(t, ctx)
	got.trace = r.lastTrace(t, ctx)
	return got
}

// reminders reads the rows the run committed for its own Job. A seeded row carries no
// SourceJobId, so this reads the commit and nothing else.
func (r *caseRun) reminders(t *testing.T, ctx context.Context) []store.Reminder {
	t.Helper()
	rows, err := r.handle.Read.QueryContext(ctx, `
		SELECT r.ReminderId, r.Text, r.Timezone, COALESCE(r.LocalDateTime, ''),
		       COALESCE(r.EmitAtUtc, ''), r.Status, COALESCE(c.Rule, ''), COALESCE(c.NextEmitAtUtc, '')
		FROM Reminder r
		LEFT JOIN RecurringReminder c ON c.ReminderId = r.ReminderId
		WHERE r.SourceJobId = ? ORDER BY r.ReminderId`, r.job.JobID)
	if err != nil {
		t.Fatalf("read reminders: %v", err)
	}
	defer rows.Close()
	var found []store.Reminder
	for rows.Next() {
		var reminder store.Reminder
		var raw string
		if err := rows.Scan(&reminder.ReminderID, &reminder.Text, &reminder.Timezone,
			&reminder.LocalDateTime, &reminder.EmitAtUtc, &reminder.Status, &raw,
			&reminder.NextEmitAtUtc); err != nil {
			t.Fatalf("scan reminder: %v", err)
		}
		if raw != "" {
			rule := store.Rule{}
			if err := json.Unmarshal([]byte(raw), &rule); err != nil {
				t.Fatalf("decode rule: %v", err)
			}
			reminder.Rule = &rule
		}
		found = append(found, reminder)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read reminders: %v", err)
	}
	return found
}

// memories reads the facts the run committed for its own Job, oldest first.
func (r *caseRun) memories(t *testing.T, ctx context.Context) []string {
	t.Helper()
	rows, err := r.handle.Read.QueryContext(ctx,
		`SELECT Text FROM Memory WHERE SourceJobId = ? ORDER BY MemoryId`, r.job.JobID)
	if err != nil {
		t.Fatalf("read memories: %v", err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatalf("scan memory: %v", err)
		}
		found = append(found, text)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read memories: %v", err)
	}
	return found
}

// lastTrace is the newest Trace row's detail: what the run recorded about itself, whatever
// it decided. A Job that never reached a commit has one too, because failing writes one.
func (r *caseRun) lastTrace(t *testing.T, ctx context.Context) map[string]any {
	t.Helper()
	var raw string
	err := r.handle.Read.QueryRowContext(ctx,
		`SELECT Detail FROM Trace WHERE JobId = ? AND Stage = ? ORDER BY TraceId DESC LIMIT 1`,
		r.job.JobID, store.AgentStage).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	return detail
}
