package agent

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"blurt/internal/store"
)

// Reads is Blurt's two read stores behind Recall, so the loop asks one thing for
// context and a test can replace both reads in one move.
type Reads struct {
	Reminders *store.ReminderStore
	Memories  *store.MemoryStore
}

func (r Reads) Nearby(ctx context.Context, userID int64, from, to time.Time) ([]store.Reminder, error) {
	return r.Reminders.Window(ctx, userID, from, to)
}
func (r Reads) Remembered(ctx context.Context, userID int64, query string) ([]store.Memory, error) {
	return r.Memories.Search(ctx, userID, query, 0)
}

// propose is one bounded model call.
func (l *AgentLoop) propose(ctx context.Context, in Input) (Turn, error) {
	callCtx, cancel := context.WithTimeout(ctx, l.proposeTimeout())
	defer cancel()
	return l.Model.Propose(callCtx, in)
}

// questionModel is one bounded call to the question writer.
func (l *AgentLoop) questionModel(ctx context.Context, in Input, missing []Field) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, l.proposeTimeout())
	defer cancel()
	return l.Model.Question(callCtx, in, missing)
}

// input assembles one turn's context. A retrieval failure is logged and dropped
// rather than failing the Job: context is advisory, so a working capture never
// depends on a working index.
func (r *run) input(ctx context.Context) Input {
	in := r.askInput()
	nearby, err := r.loop.Recall.Nearby(ctx, r.job.UserID,
		r.started.Add(-windowBefore), r.started.Add(windowAfter))
	if err != nil {
		slog.Warn("agent reminder window", "job_id", r.job.JobID, "err", err)
	}
	in.Nearby = nearby
	remembered, err := r.loop.Recall.Remembered(ctx, r.job.UserID, captureText(r.job))
	if err != nil {
		slog.Warn("agent memory search", "job_id", r.job.JobID, "err", err)
	}
	in.Remembered = remembered
	return in
}

// askInput is the part of a turn that needs no retrieval: what the user said, the
// zone anchor and the clock, and what is still missing. The records turn starts
// from it and adds what recall found; the question writer reads it as it is.
func (r *run) askInput() Input {
	return Input{Conversation: r.job.Conversation, CurrentTZ: currentTZ(r.job),
		Now: r.started, Missing: r.missing}
}

// captureText is everything the user has said in this Job, which is what Memory
// retrieval is searched with.
func captureText(job store.Job) string {
	var said strings.Builder
	for _, turn := range job.Conversation {
		if turn.Role == "user" && turn.Text != "" {
			said.WriteString(turn.Text)
			said.WriteString(" ")
		}
	}
	return said.String()
}

// currentTZ is the newest user turn's device zone: the anchor a relative time
// resolves against, and the zone the model is told to copy.
func currentTZ(job store.Job) string {
	for i := len(job.Conversation) - 1; i >= 0; i-- {
		if turn := job.Conversation[i]; turn.Role == "user" && turn.TZ != "" {
			return turn.TZ
		}
	}
	return ""
}
