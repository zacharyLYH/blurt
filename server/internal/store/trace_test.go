package store

import (
	"context"
	"errors"
	"testing"
)

// A reminder's history is its firings plus the capture that made it: a firing names the
// row, and the agent row names only the Job. Reading by ReminderId alone would lose the
// plan, so the join covers both — and it answers for the caller's own rows only.
func TestTraceStoreReadsOneRemindersWholeHistory(t *testing.T) {
	jobs, handle, userID := newJobStack(t)
	ctx := context.Background()

	job, err := jobs.Create(ctx, userID, Message{Role: "user", Text: "pay the bill on the 3rd at 8pm"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, AgentStep{
		Reminders: []Reminder{{Text: "Pay the bill", Timezone: "Asia/Singapore",
			LocalDateTime: "2026-11-03T20:00", EmitAtUtc: "2026-11-03T12:00:00Z"}},
		Reply:  Message{Text: "Reminder set."},
		Status: JobComplete,
		Detail: map[string]any{"outcome": JobComplete, "durationMs": 1200},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var reminderID int64
	if err := handle.Read.QueryRow(
		`SELECT ReminderId FROM Reminder WHERE SourceJobId = ?`, job.JobID).Scan(&reminderID); err != nil {
		t.Fatalf("read reminder id: %v", err)
	}
	if err := NewReminderStore(handle.Write, handle.Read).RecordDelivery(ctx,
		Reminder{ReminderID: reminderID, SourceJobID: job.JobID},
		map[string]any{"outcome": "sent"}); err != nil {
		t.Fatalf("record delivery: %v", err)
	}

	traces := NewTraceStore(handle.Read)
	reminder, history, err := traces.Traces(ctx, userID, reminderID)
	if err != nil {
		t.Fatalf("traces: %v", err)
	}
	if len(history) != 2 || history[0].Stage != AgentStage || history[1].Stage != NotificationStage {
		t.Fatalf("history = %+v, want the plan then the firing", history)
	}
	if reminder.Text != "Pay the bill" || reminder.Traces != 2 || reminder.LastAt == "" {
		t.Fatalf("reminder = %+v, want its own words and its rows counted", reminder)
	}
	if history[0].Detail["durationMs"] != float64(1200) {
		t.Fatalf("detail = %+v, want the stage's own payload", history[0].Detail)
	}

	// The sidebar reads the same rows, and another user's id is not one this caller can read.
	traced, err := traces.Traced(ctx, userID)
	if err != nil || len(traced) != 1 || traced[0].ReminderID != reminderID {
		t.Fatalf("traced = %+v (%v), want the one reminder", traced, err)
	}
	other, err := NewUserStore(handle.Write, handle.Read).EnsureUser(ctx, "Sam", "sam@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	if _, _, err := traces.Traces(ctx, other.UserID, reminderID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("traces for another user = %v, want ErrNotFound", err)
	}
}
