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

// trace is the Trace row's payload: the model, how the run went, how long it took,
// what the model said it read the time and zone from, and what the user is still
// owed. Evidence and missing ride here rather than on the record, because they are
// diagnostics about one attempt and not part of what was committed.
func (r *run) trace(outcome string) map[string]any {
	detail := map[string]any{
		"model":      r.loop.ModelName,
		"attempt":    r.attempt,
		"turns":      r.turns,
		"outcome":    outcome,
		"durationMs": r.loop.now().Sub(r.started).Milliseconds(),
	}
	// What the model said it read the time from rides the Trace, so a reader can
	// check the answer against the user's own words. Only a Reminder carries it.
	for _, write := range r.writes {
		if write.Evidence != "" {
			detail["evidence"] = write.Evidence
			break
		}
	}
	if len(r.missing) > 0 {
		detail["missing"] = r.missing
	}
	return detail
}
