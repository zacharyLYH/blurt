package agent

import (
	"context"
	"log/slog"
	"strings"

	"blurt/internal/store"
)

// step is the whole durable outcome of a run: the rows it inserts, the rows it rewrites, the
// assistant turn the user reads, the Status that turn implies, and the Trace row, committed
// together or not at all.
func (r *jobRun) saveStep(ctx context.Context, status, reply string, reminders, updates []store.Reminder, memories []store.Memory) error {
	detail := r.trace(status)
	detail["reminders"], detail["memories"] = len(reminders), len(memories)
	if len(updates) > 0 {
		detail["updates"] = len(updates)
	}
	if _, err := r.loop.Store.CommitAgentStep(ctx, r.job.JobID, store.AgentStep{
		Reminders: reminders,
		Updates:   updates,
		Memories:  memories,
		Reply:     store.Message{Text: reply},
		Status:    status,
		Detail:    detail,
	}); err != nil {
		return err
	}
	slog.Info("agent step", "job_id", r.job.JobID, "status", status, "turns", r.turns,
		"reminders", len(reminders), "updates", len(updates), "memories", len(memories))
	return nil
}

// trace is the Trace row's payload: the model, how the run went, how long it took,
// what each stage decided, what the model said it read the time and zone from, and
// what the user is still owed. Every verdict rides here rather than on the record,
// because they are diagnostics about one attempt and not part of what was
// committed. A stage that changes behaviour should show up in a query, not on the
// user's screen.
func (r *jobRun) trace(outcome string) map[string]any {
	detail := map[string]any{
		"model":      r.modelName,
		"attempt":    r.attempt,
		"turns":      r.turns,
		"outcome":    outcome,
		"durationMs": r.loop.now().Sub(r.started).Milliseconds(),
	}
	put(detail, "prescreenModel", r.loop.Prescreen.Name)
	put(detail, "tagsModel", r.loop.Tags.Name)
	put(detail, "prescreen", r.prescreenVerdict.Outcome)
	put(detail, "prescreenReason", r.prescreenVerdict.Reason)
	put(detail, "kind", r.answer.Kind)
	intents := make([]string, 0, len(r.answer.Ops))
	ids := []int64{}
	for _, op := range r.answer.Ops {
		intents = append(intents, op.Intent)
		if op.Intent == IntentUpdate {
			ids = append(ids, op.ReminderID)
		}
	}
	put(detail, "intent", strings.Join(intents, ","))
	if len(ids) > 0 {
		detail["reminderIds"] = ids
	}
	if r.stale {
		detail["stale"] = true
	}
	// What the model said it read the time from rides the Trace, so a reader can
	// check the answer against the user's own words. The day token and the date it
	// resolved to are two different facts: the token is what the model copied, the
	// date is what the server's calendar made of it.
	put(detail, "evidence", r.evidence())
	for _, op := range r.answer.Ops {
		put(detail, "day", op.Record.Day)
		put(detail, "date", op.Record.Date)
	}
	if len(r.lint) > 0 {
		detail["lint"] = r.lint
	}
	return detail
}

// put keeps an empty diagnostic out of the Trace: a key that is there means the run had
// something to say about it.
func put(detail map[string]any, key, value string) {
	if value != "" {
		detail[key] = value
	}
}
