package agent

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"blurt/internal/store"
)

// Retrieval is Blurt's two read stores behind Recall, so a lookup and the context block
// read the same indexes a test can replace in one move.
type Retrieval struct {
	Reminders *store.ReminderStore
	Memories  *store.MemoryStore
}

func (r Retrieval) FindReminders(ctx context.Context, userID int64, query string, limit int) ([]store.Reminder, error) {
	return r.Reminders.Search(ctx, userID, query, limit)
}
func (r Retrieval) FindMemories(ctx context.Context, userID int64, query string, limit int) ([]store.Memory, error) {
	return r.Memories.Search(ctx, userID, query, limit)
}

func (r Retrieval) FindReminder(ctx context.Context, userID, reminderID int64) (store.Reminder, bool, error) {
	return r.Reminders.ByID(ctx, userID, reminderID)
}

// prescreenModel is one bounded call to the prescreen stage: its own stage model
// when a stack has split the stages, the loop's Model otherwise (every test double).
func (l *AgentLoop) prescreenModel(ctx context.Context, capture string) (Verdict, error) {
	callCtx, cancel := context.WithTimeout(ctx, l.modelTimeout())
	defer cancel()
	return l.model(l.Prescreen).Prescreen(callCtx, capture)
}

// agentModel is one bounded free-form turn of the agent loop, on the Job's own model: the
// turn that may look something up.
func (l *AgentLoop) agentModel(ctx context.Context, model Model, in PlannerInput, sent []Message) (Message, error) {
	callCtx, cancel := context.WithTimeout(ctx, l.modelTimeout())
	defer cancel()
	return model.Chat(callCtx, in, sent, agentTools)
}

// finalizeModel is one bounded strict call: the one that writes the answer the linter then
// reads, on the Job's own model.
func (l *AgentLoop) finalizeModel(ctx context.Context, model Model, in PlannerInput, sent []Message) (Answer, error) {
	callCtx, cancel := context.WithTimeout(ctx, l.modelTimeout())
	defer cancel()
	return model.Finalize(callCtx, in, sent)
}

// Input assembles the context one turn is built from: the Job's conversation, the anchor zone
// and clock, and the memories worth having to hand. It is built once per request, because a
// lint retry reads the same context. A retrieval failure is logged and dropped rather than
// failing the Job: context is advisory, so a working capture never depends on a working index.
// Exported because the L1 layer pins this as a golden, rather than a hand-built stand-in.
func (l *AgentLoop) Input(ctx context.Context, job store.Job, now time.Time) PlannerInput {
	in := PlannerInput{Conversation: job.Conversation, CurrentTZ: deviceZone(job), Now: now}
	remembered, err := l.Recall.FindMemories(ctx, job.UserID, userText(job), 0)
	if err != nil {
		slog.Warn("agent memory search", "job_id", job.JobID, "err", err)
	}
	in.Remembered = remembered
	return in
}

// input is the running Job's own view of it.
func (r *jobRun) input(ctx context.Context) PlannerInput {
	return r.loop.Input(ctx, r.job, r.started)
}

// userText is everything the user has said in this Job, which is what Memory
// retrieval is searched with.
func userText(job store.Job) string {
	var said strings.Builder
	for _, turn := range job.Conversation {
		if turn.Role == "user" && turn.Text != "" {
			said.WriteString(turn.Text)
			said.WriteString(" ")
		}
	}
	return said.String()
}

// deviceZone is the newest user turn's device zone: the anchor a relative time
// resolves against, and the zone the model is told to copy.
func deviceZone(job store.Job) string {
	for i := len(job.Conversation) - 1; i >= 0; i-- {
		if turn := job.Conversation[i]; turn.Role == "user" && turn.TZ != "" {
			return turn.TZ
		}
	}
	return ""
}
