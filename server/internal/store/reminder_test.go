package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"blurt/internal/db"
)

// A repeat is context like any other Reminder, and its own instant column is null: the
// lookup has to read the rule's next occurrence instead, or a recurring Reminder would be
// invisible to the agent exactly when the user is most likely to correct it.
func TestSearchCarriesARecurringRemindersRule(t *testing.T) {
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
		},
		Reply:  Message{Text: "Reminder set."},
		Status: JobComplete,
		Detail: map[string]any{"outcome": JobComplete},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	found, err := NewReminderStore(handle.Write, handle.Read).Search(ctx, userID, "take pills", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found = %+v, want the repeat alone", found)
	}
	next := found[0]
	if next.Rule == nil || next.Rule.Frequency != FrequencyMonthly || next.Rule.Day != 5 {
		t.Fatalf("rule = %+v, want the pattern the row carries", next.Rule)
	}
	if next.NextEmitAtUtc != "2026-11-05T01:00:00Z" || next.LocalDateTime != "" {
		t.Fatalf("repeat = %q %q, want its next occurrence and no local time of its own",
			next.NextEmitAtUtc, next.LocalDateTime)
	}
	// The one-off is read back verbatim, so the row the tool prints is the row Blurt stored.
	found, err = NewReminderStore(handle.Write, handle.Read).Search(ctx, userID, "glue", 5)
	if err != nil || len(found) != 1 || found[0].Text != "Buy glue" ||
		found[0].LocalDateTime != "2026-10-25T18:00" || found[0].Status != ReminderResolved {
		t.Fatalf("found = %+v (%v), want the one-off as stored", found, err)
	}
}

// The agent's own reminder lookup reads by text rather than by time: a capture names words,
// not an instant, and the model decides which of the rows it is about. It is the owner's
// rows, it is capped, and it matches on any word rather than every one.
func TestReminderSearchReadsByTextInsideTheOwner(t *testing.T) {
	_, handle, userID := newJobStack(t)
	ctx := context.Background()
	sam, err := NewUserStore(handle.Write, handle.Read).EnsureUser(ctx, "Sam", "sam@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	other := sam.UserID

	seed := []struct {
		userID int64
		text   string
	}{
		{userID, "Fetch kids from school"},
		{userID, "Dentist appointment"},
		{userID, "Return the library books"},
		{other, "Dentist for Sam"},
	}
	for _, row := range seed {
		if err := insertReminder(ctx, handle.Write, Reminder{
			UserID: row.userID, Text: row.text, Timezone: "Asia/Singapore",
			LocalDateTime: "2026-10-25T09:00", EmitAtUtc: "2026-10-25T01:00:00Z",
		}); err != nil {
			t.Fatalf("seed %q: %v", row.text, err)
		}
	}
	reminders := NewReminderStore(handle.Write, handle.Read)
	found, err := reminders.Search(ctx, userID, "dentist appointment", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 1 || found[0].Text != "Dentist appointment" {
		t.Fatalf("found = %+v, want the owner's one matching row", found)
	}
	// Punctuation and filler are the query's own problem, not the row's: the words are
	// OR-ed, so a capture worded around the event still finds it.
	if found, err = reminders.Search(ctx, userID, "What about the library?", 5); err != nil || len(found) != 1 {
		t.Fatalf("found = %+v (%v), want the library row", found, err)
	}
	if found, err = reminders.Search(ctx, userID, "nothing here matches", 5); err != nil || len(found) != 0 {
		t.Fatalf("found = %+v (%v), want an empty result to be a legal answer", found, err)
	}
	if found, err = reminders.Search(ctx, userID, "", 5); err != nil || found != nil {
		t.Fatalf("found = %+v (%v), want an empty query to read nothing", found, err)
	}
}

// A change rewrites the row it names and nothing else: the id, the CreatedAt, the status and the
// Job that made it all stay, so nothing pointing at the row goes stale. A row that has left the
// schedule has nowhere to put the change, and the write says so rather than resurrecting it.
func TestRewriteReminderKeepsTheRowAndItsHistory(t *testing.T) {
	jobs, handle, userID := newJobStack(t)
	ctx := context.Background()

	job, err := jobs.Create(ctx, userID, Message{Role: "user", Text: "buy glue at 6pm tomorrow"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, AgentStep{
		Reminders: []Reminder{{Text: "Buy glue", Timezone: "Asia/Singapore",
			LocalDateTime: "2026-10-25T18:00", EmitAtUtc: "2026-10-25T10:00:00Z"}},
		Reply:  Message{Text: "Reminder set."},
		Status: JobComplete,
		Detail: map[string]any{"outcome": JobComplete},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	reminders := NewReminderStore(handle.Write, handle.Read)
	before, ok, err := reminders.ByID(ctx, userID, 1)
	if err != nil || !ok {
		t.Fatalf("by id = %+v (%v), want the committed row", before, err)
	}

	change := before
	change.Text, change.LocalDateTime, change.EmitAtUtc = "Buy glue sticks", "2026-10-27T20:00", "2026-10-27T12:00:00Z"
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, AgentStep{
		Updates: []Reminder{change},
		Reply:   Message{Text: "Reminder updated."},
		Status:  JobComplete,
		Detail:  map[string]any{"outcome": JobComplete},
	}); err != nil {
		t.Fatalf("change: %v", err)
	}

	after, ok, err := reminders.ByID(ctx, userID, before.ReminderID)
	if err != nil || !ok {
		t.Fatalf("by id after the change = %+v (%v)", after, err)
	}
	if after.ReminderID != before.ReminderID || after.CreatedAt != before.CreatedAt ||
		after.Status != ReminderResolved || after.SourceJobID != job.JobID {
		t.Fatalf("row = %+v, want the id, the CreatedAt, the status and the Job kept", after)
	}
	if after.Text != "Buy glue sticks" || after.LocalDateTime != "2026-10-27T20:00" {
		t.Fatalf("row = %+v, want the change written in place", after)
	}

	if _, err := reminders.Cancel(ctx, userID, before.ReminderID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, AgentStep{
		Updates: []Reminder{change},
		Reply:   Message{Text: "Reminder updated."},
		Status:  JobComplete,
		Detail:  map[string]any{"outcome": JobComplete},
	}); !errors.Is(err, ErrReminderStale) {
		t.Fatalf("change of a called-off row = %v, want ErrReminderStale", err)
	}
}

// A change can make a one-off repeat, and a repeat a one-off: the rule row follows the row it
// belongs to, so the store writes it, replaces it and clears it while the reminder itself stays
// where it is.
func TestRewriteReminderConvertsBetweenOneOffAndRepeat(t *testing.T) {
	jobs, handle, userID := newJobStack(t)
	ctx := context.Background()

	job, err := jobs.Create(ctx, userID, Message{Role: "user", Text: "pay the card at 8pm on the 3rd"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, AgentStep{
		Reminders: []Reminder{{Text: "Pay the card", Timezone: "Asia/Singapore",
			LocalDateTime: "2026-11-03T20:00", EmitAtUtc: "2026-11-03T12:00:00Z"}},
		Reply:  Message{Text: "Reminder set."},
		Status: JobComplete,
		Detail: map[string]any{"outcome": JobComplete},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	reminders := NewReminderStore(handle.Write, handle.Read)
	once, ok, err := reminders.ByID(ctx, userID, 1)
	if err != nil || !ok || once.Rule != nil {
		t.Fatalf("row = %+v (%v), want the one-off it started as", once, err)
	}

	// One-off to repeat: the row loses its local time and gains a rule.
	repeating := once
	repeating.LocalDateTime, repeating.EmitAtUtc = "", ""
	repeating.Rule, repeating.NextEmitAtUtc = &Rule{Frequency: FrequencyMonthly, Time: "20:00", Day: 3},
		"2026-11-03T12:00:00Z"
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, AgentStep{
		Updates: []Reminder{repeating},
		Reply:   Message{Text: "Reminder updated."},
		Status:  JobComplete,
		Detail:  map[string]any{"outcome": JobComplete},
	}); err != nil {
		t.Fatalf("make it repeat: %v", err)
	}
	after, ok, err := reminders.ByID(ctx, userID, once.ReminderID)
	if err != nil || !ok {
		t.Fatalf("by id = %+v (%v), want the row", after, err)
	}
	if after.Rule == nil || after.Rule.Frequency != FrequencyMonthly || after.Rule.Day != 3 ||
		after.NextEmitAtUtc != "2026-11-03T12:00:00Z" || after.LocalDateTime != "" {
		t.Fatalf("row = %+v, want the rule in place and the local time gone", after)
	}

	// And back: the rule row is cleared rather than left pointing at a one-off.
	justOnce := after
	justOnce.Rule, justOnce.NextEmitAtUtc = nil, ""
	justOnce.LocalDateTime, justOnce.EmitAtUtc = "2026-10-28T20:00", "2026-10-28T12:00:00Z"
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, AgentStep{
		Updates: []Reminder{justOnce},
		Reply:   Message{Text: "Reminder updated."},
		Status:  JobComplete,
		Detail:  map[string]any{"outcome": JobComplete},
	}); err != nil {
		t.Fatalf("make it a one-off: %v", err)
	}
	back, ok, err := reminders.ByID(ctx, userID, once.ReminderID)
	if err != nil || !ok {
		t.Fatalf("by id = %+v (%v), want the row", back, err)
	}
	var rules int
	if err := handle.Read.QueryRow(`SELECT count(*) FROM RecurringReminder WHERE ReminderId = ?`,
		once.ReminderID).Scan(&rules); err != nil {
		t.Fatalf("count rules: %v", err)
	}
	if back.Rule != nil || rules != 0 || back.LocalDateTime != "2026-10-28T20:00" ||
		back.NextEmitAtUtc != "" {
		t.Fatalf("row = %+v (%d rules), want a one-off with no rule behind it", back, rules)
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
	if claimed, err := reminders.Fire(ctx, due[1], time.Time{}, nil); err != nil || !claimed {
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

	claimed, err := reminders.Fire(ctx, oneOff, time.Time{}, map[string]any{"outcome": "attempt"})
	if err != nil || !claimed {
		t.Fatalf("fire = %v (%v), want a claim with a trace row", claimed, err)
	}
	if again, err := reminders.Fire(ctx, oneOff, time.Time{}, nil); err != nil || again {
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
	// Reminder and firing it was about: that is what a reader traces it through.
	var jobID, reminderID int64
	if err := handle.Read.QueryRow(
		`SELECT JobId, ReminderId FROM Trace WHERE ReminderId = ? AND Stage = ? ORDER BY TraceId DESC LIMIT 1`,
		oneOff.ReminderID, NotificationStage).Scan(&jobID, &reminderID); err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if jobID != job.JobID || reminderID != oneOff.ReminderID {
		t.Fatalf("trace = job %d reminder %d, want job %d reminder %d",
			jobID, reminderID, job.JobID, oneOff.ReminderID)
	}

	// A repeat stays resolved and moves instead, derived from its rule rather than by
	// adding a day to the instant it just fired.
	next := time.Date(2026, 10, 26, 1, 0, 0, 0, time.UTC)
	if claimed, err := reminders.Fire(ctx, repeat, next, nil); err != nil || !claimed {
		t.Fatalf("fire repeat = %v (%v), want it claimed", claimed, err)
	}
	if stale, err := reminders.Fire(ctx, repeat, next, nil); err != nil || stale {
		t.Fatalf("stale repeat fire = %v (%v), want the old instant to lose the swap", stale, err)
	}
	// A pattern the calendar cannot place has no next instant, so it must not fire and
	// must not move: otherwise it would ring on every sweep forever.
	if placed, err := reminders.Fire(ctx, repeat, time.Time{}, nil); err != nil || placed {
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
	if claimed, err := reminders.Fire(ctx, Reminder{
		ReminderID: reminderID(t, handle, "Later glue"), UserID: userID, SourceJobID: job.JobID,
		Timezone: "Asia/Singapore", EmitAtUtc: "2026-10-26T10:00:00Z",
	}, time.Time{}, nil); err != nil || !claimed {
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
