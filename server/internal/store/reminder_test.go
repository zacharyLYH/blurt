package store

import (
	"context"
	"testing"
	"time"
)

// A repeat is retrieval context like any other Reminder, and its own instant column is
// null: the window has to read the rule's next occurrence instead, or a recurring Reminder
// would be invisible to the agent exactly when the user is most likely to correct it.
func TestWindowCarriesARecurringRemindersNextOccurrence(t *testing.T) {
	jobs, handle, userID := newJobStack(t)
	ctx := context.Background()

	job, err := jobs.Create(ctx, userID,
		Message{Role: "user", Text: "take pills every month on the 5th at 9am"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, AgentStep{
		Reminders: []Reminder{
			{Text: "Take pills", Timezone: "Asia/Singapore",
				Rule:          &Rule{Frequency: FrequencyMonthly, Time: "09:00", Day: 5},
				NextEmitAtUtc: "2026-11-05T01:00:00Z"},
			{Text: "Buy glue", Timezone: "Asia/Singapore",
				LocalDateTime: "2026-10-25T18:00", EmitAtUtc: "2026-10-25T10:00:00Z"},
			{Text: "Far pills", Timezone: "Asia/Singapore",
				Rule:          &Rule{Frequency: FrequencyMonthly, Time: "09:00", Day: 5},
				NextEmitAtUtc: "2026-12-05T01:00:00Z"},
		},
		Reply:  Message{Text: "Reminder set."},
		Status: JobComplete,
		Detail: map[string]any{"outcome": JobComplete},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	window, err := NewReminderStore(handle.Read).Window(ctx, userID,
		time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	// The one-off and the repeat whose next occurrence falls inside it, ordered by the
	// instant each is about; the repeat a month later is out of range.
	if len(window) != 2 || window[0].Text != "Buy glue" || window[1].Text != "Take pills" {
		t.Fatalf("window = %+v, want the one-off and the repeat in range", window)
	}
	next := window[1]
	if next.Rule == nil || next.Rule.Frequency != FrequencyMonthly || next.Rule.Day != 5 {
		t.Fatalf("rule = %+v, want the pattern the row carries", next.Rule)
	}
	if next.NextEmitAtUtc != "2026-11-05T01:00:00Z" || next.LocalDateTime != "" {
		t.Fatalf("repeat = %q %q, want its next occurrence and no local time of its own",
			next.NextEmitAtUtc, next.LocalDateTime)
	}
}

// A recurring Reminder is two rows written by one step, so a resumed step that
// re-proposes the same rule must leave both at one: the rule is inserted by the
// same predicate that creates the Reminder, and the second write is ignored.
func TestCommitRecurringReminderSkipsARepeatedStep(t *testing.T) {
	jobs, handle, userID := newJobStack(t)
	ctx := context.Background()

	job, err := jobs.Create(ctx, userID,
		Message{Role: "user", Text: "take the trash out every tuesday at 7pm"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	step := AgentStep{
		Reminders: []Reminder{{
			Text: "Take the trash out", Timezone: "Asia/Singapore",
			Rule:          &Rule{Frequency: FrequencyWeekly, Time: "19:00", Weekday: 2},
			NextEmitAtUtc: "2026-10-27T11:00:00Z",
		}},
		Reply:  Message{Text: "Reminder set."},
		Status: JobComplete,
		Detail: map[string]any{"outcome": JobComplete},
	}
	for i := range 2 {
		if _, err := jobs.CommitAgentStep(ctx, job.JobID, step); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}

	for _, table := range []string{"Reminder", "RecurringReminder"} {
		var count int
		if err := handle.Read.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("%s rows = %d, want 1 after a repeated step", table, count)
		}
	}

	// The rule stores only the parts its frequency needs, and the next instant
	// is derived rather than carried forward.
	var rule, next string
	if err := handle.Read.QueryRow(
		`SELECT Rule, NextEmitAtUtc FROM RecurringReminder`).Scan(&rule, &next); err != nil {
		t.Fatalf("read rule: %v", err)
	}
	if rule != `{"frequency":"weekly","time":"19:00","weekday":2}` {
		t.Fatalf("rule = %s", rule)
	}
	if next != "2026-10-27T11:00:00Z" {
		t.Fatalf("next emit = %s, want 2026-10-27T11:00:00Z", next)
	}
}

// A one-off still stores its human time and derived instant, which is the row
// shape a recurrence must not quietly take over.
func TestCommitOneOffReminderKeepsItsHumanTime(t *testing.T) {
	jobs, handle, userID := newJobStack(t)
	ctx := context.Background()

	job, err := jobs.Create(ctx, userID, Message{Role: "user", Text: "buy glue at 6pm tomorrow"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, AgentStep{
		Reminders: []Reminder{{
			Text: "Buy glue", Timezone: "Asia/Singapore",
			LocalDateTime: "2026-10-25T18:00", EmitAtUtc: "2026-10-25T10:00:00Z",
		}},
		Reply:  Message{Text: "Reminder set."},
		Status: JobComplete,
		Detail: map[string]any{"outcome": JobComplete},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var local, emit string
	if err := handle.Read.QueryRow(
		`SELECT LocalDateTime, EmitAtUtc FROM Reminder`).Scan(&local, &emit); err != nil {
		t.Fatalf("read reminder: %v", err)
	}
	if local != "2026-10-25T18:00" || emit != "2026-10-25T10:00:00Z" {
		t.Fatalf("reminder = %q %q, want the local truth and its derived instant", local, emit)
	}
	var count int
	if err := handle.Read.QueryRow(`SELECT count(*) FROM RecurringReminder`).Scan(&count); err != nil {
		t.Fatalf("count recurring: %v", err)
	}
	if count != 0 {
		t.Fatalf("recurring rows = %d, want none for a one-off", count)
	}
}
