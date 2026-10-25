package store

import (
	"context"
	"testing"
	"time"

	"blurt/internal/db"
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

	window, err := NewReminderStore(handle.Write, handle.Read).Window(ctx, userID,
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

// reminderID reads one committed Reminder's id back, so a test can name the row the
// emitter would have read.
func reminderID(t *testing.T, handle *db.DB, text string) int64 {
	t.Helper()
	var id int64
	if err := handle.Read.QueryRow(
		`SELECT ReminderId FROM Reminder WHERE Text = ?`, text).Scan(&id); err != nil {
		t.Fatalf("read reminder id for %q: %v", text, err)
	}
	return id
}

// commitThree commits the rows the emitter tests need: a one-off that has arrived, a
// one-off still ahead, and a repeat whose next occurrence has arrived.
func commitThree(t *testing.T, handle *db.DB, jobID int64) {
	t.Helper()
	if _, err := NewJobStore(handle.Write, handle.Read).CommitAgentStep(
		context.Background(), jobID, AgentStep{
			Reminders: []Reminder{
				{Text: "Buy glue", Timezone: "Asia/Singapore",
					LocalDateTime: "2026-10-25T18:00", EmitAtUtc: "2026-10-25T10:00:00Z"},
				{Text: "Later glue", Timezone: "Asia/Singapore",
					LocalDateTime: "2026-10-26T18:00", EmitAtUtc: "2026-10-26T10:00:00Z"},
				{Text: "Take pills", Timezone: "Asia/Singapore",
					Rule:          &Rule{Frequency: FrequencyDaily, Time: "09:00"},
					NextEmitAtUtc: "2026-10-25T01:00:00Z"},
			},
			Reply:  Message{Text: "Reminder set."},
			Status: JobComplete,
			Detail: map[string]any{"outcome": JobComplete},
		}); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// Due is the emitter's query, so it is the one read that filters on Status: a
// Reminder still resolved and already arrived is due, and one that fired, was called
// off, or is still ahead is not. The repeat is due on its rule's next occurrence,
// because its own instant columns are null.
func TestDuePicksUpArrivedResolvedReminders(t *testing.T) {
	jobs, handle, userID := newJobStack(t)
	ctx := context.Background()
	reminders := NewReminderStore(handle.Write, handle.Read)

	job, err := jobs.Create(ctx, userID, Message{Role: "user", Text: "three things"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	commitThree(t, handle, job.JobID)

	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	due, err := reminders.Due(ctx, now, 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	// Oldest first, and the one still ahead is not in it.
	if len(due) != 2 || due[0].Text != "Take pills" || due[1].Text != "Buy glue" {
		t.Fatalf("due = %+v, want the repeat then the arrived one-off", due)
	}
	if due[0].Rule == nil || due[0].Rule.Frequency != FrequencyDaily {
		t.Fatalf("rule = %+v, want the repeat's pattern", due[0].Rule)
	}

	// One sweep's budget is a limit, not a promise to return everything.
	if one, err := reminders.Due(ctx, now, 1); err != nil || len(one) != 1 {
		t.Fatalf("due limit = %+v (%v), want one row", one, err)
	}

	// A fired one-off and a called-off repeat are both done being due.
	if _, claimed, err := reminders.Fire(ctx, due[1], time.Time{}, nil); err != nil || !claimed {
		t.Fatalf("fire one-off = %v (%v), want it claimed", claimed, err)
	}
	if cancelled, err := reminders.Cancel(ctx, userID, due[0].ReminderID); err != nil || !cancelled {
		t.Fatalf("cancel repeat = %v (%v), want it called off", cancelled, err)
	}
	if left, err := reminders.Due(ctx, now, 10); err != nil || len(left) != 0 {
		t.Fatalf("due after firing = %+v (%v), want none", left, err)
	}
}

// Fire is a claim, so it can only be won once: the one-off moves out of resolved, and
// the repeat moves to the instant it fires next. A second caller holding the row it
// read before loses that swap, which is what stops a restart or a second sweep from
// putting the same firing on the wire twice. The attempt's Trace row is written with
// the claim, because the push carries its id.
func TestFireClaimsOnceAndAdvancesARepeat(t *testing.T) {
	jobs, handle, userID := newJobStack(t)
	ctx := context.Background()
	reminders := NewReminderStore(handle.Write, handle.Read)

	job, err := jobs.Create(ctx, userID, Message{Role: "user", Text: "three things"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	commitThree(t, handle, job.JobID)

	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	due, err := reminders.Due(ctx, now, 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	repeat, oneOff := due[0], due[1]

	traceID, claimed, err := reminders.Fire(ctx, oneOff, time.Time{}, map[string]any{"outcome": "attempt"})
	if err != nil || !claimed || traceID == 0 {
		t.Fatalf("fire = %d %v (%v), want a claim with a trace row", traceID, claimed, err)
	}
	if _, again, err := reminders.Fire(ctx, oneOff, time.Time{}, nil); err != nil || again {
		t.Fatalf("second fire = %v (%v), want the claim refused", again, err)
	}
	var status string
	if err := handle.Read.QueryRow(
		`SELECT Status FROM Reminder WHERE ReminderId = ?`, oneOff.ReminderID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != ReminderFired {
		t.Fatalf("status = %q, want fired", status)
	}

	// The attempt row hangs off the Job that committed the Reminder and names the
	// Reminder it was about: that pair is what a reader traces a firing back through.
	var jobID, reminderID int64
	if err := handle.Read.QueryRow(
		`SELECT JobId, ReminderId FROM Trace WHERE TraceId = ?`, traceID).Scan(&jobID, &reminderID); err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if jobID != job.JobID || reminderID != oneOff.ReminderID {
		t.Fatalf("trace = job %d reminder %d, want job %d reminder %d",
			jobID, reminderID, job.JobID, oneOff.ReminderID)
	}

	// A repeat stays resolved and moves instead, derived from its rule rather than by
	// adding a day to the instant it just fired.
	next := time.Date(2026, 10, 26, 1, 0, 0, 0, time.UTC)
	if _, claimed, err := reminders.Fire(ctx, repeat, next, nil); err != nil || !claimed {
		t.Fatalf("fire repeat = %v (%v), want it claimed", claimed, err)
	}
	if _, stale, err := reminders.Fire(ctx, repeat, next, nil); err != nil || stale {
		t.Fatalf("stale repeat fire = %v (%v), want the old instant to lose the swap", stale, err)
	}
	// A pattern the calendar cannot place has no next instant, so it must not fire and
	// must not move: otherwise it would ring on every sweep forever.
	if _, placed, err := reminders.Fire(ctx, repeat, time.Time{}, nil); err != nil || placed {
		t.Fatalf("unplaceable fire = %v (%v), want it refused", placed, err)
	}

	var storedNext, storedStatus string
	if err := handle.Read.QueryRow(`
		SELECT c.NextEmitAtUtc, r.Status FROM RecurringReminder c
		JOIN Reminder r ON r.ReminderId = c.ReminderId WHERE c.ReminderId = ?`,
		repeat.ReminderID).Scan(&storedNext, &storedStatus); err != nil {
		t.Fatalf("read repeat: %v", err)
	}
	if storedNext != "2026-10-26T01:00:00Z" || storedStatus != ReminderResolved {
		t.Fatalf("repeat = %q %q, want the next occurrence and still resolved", storedNext, storedStatus)
	}
}

// Cancelling is a status, not a delete: the Reminder leaves the upcoming list, keeps
// its row, and shows up in the past. A Reminder that already fired, or one that
// belongs to somebody else, is not this caller's to call off.
func TestCancelCallsOffWithoutDeleting(t *testing.T) {
	jobs, handle, userID := newJobStack(t)
	ctx := context.Background()
	reminders := NewReminderStore(handle.Write, handle.Read)

	job, err := jobs.Create(ctx, userID, Message{Role: "user", Text: "three things"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	commitThree(t, handle, job.JobID)
	id := reminderID(t, handle, "Buy glue")
	if _, claimed, err := reminders.Fire(ctx,
		Reminder{ReminderID: reminderID(t, handle, "Later glue"), SourceJobID: job.JobID},
		time.Time{}, nil); err != nil || !claimed {
		t.Fatalf("fire = %v (%v), want it claimed", claimed, err)
	}

	if cancelled, err := reminders.Cancel(ctx, userID, id); err != nil || !cancelled {
		t.Fatalf("cancel = %v (%v), want it called off", cancelled, err)
	}
	upcoming, err := reminders.Upcoming(ctx, userID)
	if err != nil {
		t.Fatalf("upcoming: %v", err)
	}
	for _, reminder := range upcoming {
		if reminder.ReminderID == id {
			t.Fatalf("cancelled Reminder still upcoming: %+v", reminder)
		}
	}
	// The one-off that fired is newest, so it leads the past list.
	past, err := reminders.Past(ctx, userID)
	if err != nil {
		t.Fatalf("past: %v", err)
	}
	if len(past) != 2 || past[0].Status != ReminderFired || past[1].Status != ReminderCancelled {
		t.Fatalf("past = %+v, want the fired one-off then the cancelled one", past)
	}

	if again, err := reminders.Cancel(ctx, userID, id); err != nil || again {
		t.Fatalf("second cancel = %v (%v), want a no-op", again, err)
	}
	other, err := NewUserStore(handle.Write, handle.Read).EnsureUser(ctx, "Other", "other@example.com")
	if err != nil {
		t.Fatalf("ensure other: %v", err)
	}
	if foreign, err := reminders.Cancel(ctx, other.UserID, reminderID(t, handle, "Take pills")); err != nil || foreign {
		t.Fatalf("foreign cancel = %v (%v), want it refused", foreign, err)
	}
	var count int
	if err := handle.Read.QueryRow(
		`SELECT count(*) FROM Reminder WHERE ReminderId = ?`, id).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows = %d, want the cancelled Reminder kept", count)
	}
}
