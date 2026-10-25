package agent

import (
	"context"
	"log/slog"

	"blurt/internal/store"
)

// step is the whole durable outcome of a run: the records, the assistant turn the
// user reads, the Status that turn implies, and the Trace row, committed together or
// not at all.
func (r *run) step(ctx context.Context, status, reply string, reminders []store.Reminder, memories []store.Memory) error {
	detail := r.trace(status)
	detail["reminders"], detail["memories"] = len(reminders), len(memories)
	if _, err := r.loop.Store.CommitAgentStep(ctx, r.job.JobID, store.AgentStep{
		Reminders: reminders,
		Memories:  memories,
		Reply:     store.Message{Text: reply},
		Status:    status,
		Detail:    detail,
	}); err != nil {
		return err
	}
	slog.Info("agent step", "job_id", r.job.JobID, "status", status, "turns", r.turns,
		"reminders", len(reminders), "memories", len(memories))
	return nil
}

// trace is the Trace row's payload: the model, how the run went, and how long it
// took. Refusals ride along because they are what a re-plan or a question is about.
func (r *run) trace(outcome string) map[string]any {
	detail := map[string]any{
		"model":      r.loop.ModelName,
		"attempt":    r.attempt,
		"turns":      r.turns,
		"outcome":    outcome,
		"durationMs": r.loop.now().Sub(r.started).Milliseconds(),
	}
	if len(r.refused) > 0 {
		detail["refused"] = r.refused
	}
	if len(r.dropped) > 0 {
		detail["dropped"] = r.dropped
	}
	if len(r.readings) > 0 {
		detail["fallback"] = r.readings
	}
	return detail
}

// replyFor stands in when the model committed records but said nothing about them, so a completed Job
// does not close on the user's own words alone: the Conversation is what the Job's history shows, and
// with no Reminders view yet it is the only place a commit is visible.
func (r *run) replyFor() string {
	if len(r.reminders) > 0 {
		return "Reminder set."
	}
	// Committing is reached only with something accepted, so this is the Memory case.
	return "Noted."
}
