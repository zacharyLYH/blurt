package emitter

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"blurt/internal/db"
	"blurt/internal/push"
	"blurt/internal/store"
)

// anchor is the instant every test schedules against: an afternoon in UTC, which is
// the same day at 18:00 in Asia/Singapore — the zone the fake model schedules in.
var anchor = time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)

// stack is one test's wiring: real SQLite, a Job that owns the Reminders, a clock the
// test moves by hand, and a relay that records what was fired at it.
type stack struct {
	handle    *db.DB
	jobs      *store.JobStore
	reminders *store.ReminderStore
	at        *clock
	relay     *relay
	userID    int64
	jobID     int64
}

// clock is the emitter's own time, so a test schedules an instant in the past or a
// week ahead and watches one sweep decide it.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

// relay stands in for the push service: it keeps every message, notices when it
// arrived, and answers with whatever status the test set.
type relay struct {
	mu       sync.Mutex
	messages []push.Message
	arrived  []time.Time
	result   push.Result
	err      error
}

func (r *relay) NotifyUser(_ context.Context, _ int64, msg push.Message) (push.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, msg)
	r.arrived = append(r.arrived, time.Now())
	return r.result, r.err
}

// sent is what the relay saw, copied out so a test never races an in-flight sweep.
func (r *relay) sent() ([]push.Message, []time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]push.Message(nil), r.messages...), append([]time.Time(nil), r.arrived...)
}

func newStack(t *testing.T) *stack {
	t.Helper()
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })

	ctx := context.Background()
	user, err := store.NewUserStore(handle.Write, handle.Read).EnsureUser(ctx, "Lam", "lam@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	jobs := store.NewJobStore(handle.Write, handle.Read)
	job, err := jobs.Create(ctx, user.UserID, store.Message{Role: "user", Text: "capture"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	return &stack{
		handle:    handle,
		jobs:      jobs,
		reminders: store.NewReminderStore(handle.Write, handle.Read),
		at:        &clock{at: anchor},
		relay:     &relay{result: push.Result{Sent: 1}},
		userID:    user.UserID,
		jobID:     job.JobID,
	}
}

// commit stores the Reminders as one agent step, which is how they land in production:
// created inside the Job's transaction, with no emitter involved.
func (s *stack) commit(t *testing.T, reminders ...store.Reminder) {
	t.Helper()
	if _, err := s.jobs.CommitAgentStep(context.Background(), s.jobID, store.AgentStep{
		Reminders: reminders,
		Reply:     store.Message{Text: "Reminder set."},
		Status:    store.JobComplete,
		Detail:    map[string]any{"outcome": store.JobComplete},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// sweep runs one pass of the emitter loop, which is what one tick of Run does.
func (s *stack) sweep(t *testing.T, spacing time.Duration) {
	t.Helper()
	e := &Emitter{Reminders: s.reminders, Pusher: s.relay, Spacing: spacing, Now: s.at.now}
	e.sweep(context.Background())
}

// stored reads one Reminder back, so a test can assert the status it landed in and the
// instant it moved to.
func (s *stack) stored(t *testing.T, text string) store.Reminder {
	t.Helper()
	var reminder store.Reminder
	err := s.handle.Read.QueryRow(`
		SELECT r.ReminderId, r.Status, COALESCE(r.EmitAtUtc, ''), COALESCE(c.NextEmitAtUtc, '')
		FROM Reminder r LEFT JOIN RecurringReminder c ON c.ReminderId = r.ReminderId
		WHERE r.Text = ?`, text).Scan(&reminder.ReminderID, &reminder.Status,
		&reminder.EmitAtUtc, &reminder.NextEmitAtUtc)
	if err != nil {
		t.Fatalf("read %q: %v", text, err)
	}
	return reminder
}

// traces reads the notification Trace rows written for one Reminder, oldest first: the
// attempt, then the delivery result that points back at it.
func (s *stack) traces(t *testing.T, text string) []map[string]any {
	t.Helper()
	rows, err := s.handle.Read.Query(`
		SELECT t.Detail FROM Trace t JOIN Reminder r ON r.ReminderId = t.ReminderId
		WHERE r.Text = ? AND t.Stage = ? ORDER BY t.TraceId`, text, store.NotificationStage)
	if err != nil {
		t.Fatalf("read traces: %v", err)
	}
	defer rows.Close()

	var details []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan trace: %v", err)
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			t.Fatalf("decode trace: %v", err)
		}
		details = append(details, detail)
	}
	return details
}

// oneOff is a reminder that has already arrived at anchor.
func oneOff(text string) store.Reminder {
	return store.Reminder{Text: text, Timezone: "Asia/Singapore",
		LocalDateTime: "2026-10-25T18:00", EmitAtUtc: "2026-10-25T10:00:00Z"}
}

// wantBody asserts a notification carries the reminder's local clock and a short zone
// label. The label itself is the host's own tzdata — Singapore is "SGT" on one platform
// and "+08" on another — so only the clock and the label's presence are pinned.
func wantBody(t *testing.T, got, clock string) {
	t.Helper()
	if !strings.HasPrefix(got, clock+" ") || len(got) == len(clock)+1 {
		t.Fatalf("body = %q, want %q followed by a zone label", got, clock)
	}
}

// repeat is a daily rule whose next occurrence has already arrived at anchor.
func repeat(text, next string) store.Reminder {
	return store.Reminder{Text: text, Timezone: "Asia/Singapore",
		Rule:          &store.Rule{Frequency: store.FrequencyDaily, Time: "09:00"},
		NextEmitAtUtc: next}
}

// One arrived Reminder is one notification, and a second sweep sends nothing: the
// reminder it already fired is no longer resolved. The attempt is durable before the
// push — the payload's TraceId names the row the delivery result then points back at —
// because a crash in between must cost this firing rather than repeat it forever.
func TestSweepFiresAnArrivedReminderOnce(t *testing.T) {
	s := newStack(t)
	s.commit(t, oneOff("Buy glue"))

	s.sweep(t, time.Millisecond)
	s.sweep(t, time.Millisecond)

	sent, _ := s.relay.sent()
	if len(sent) != 1 {
		t.Fatalf("sent = %d, want one notification", len(sent))
	}
	// The user's own words, the instant in the zone they meant; the tag belongs to the
	// Reminder rather than the firing, so a repeat replaces instead of stacking.
	if sent[0].Title != "Buy glue" {
		t.Fatalf("title = %q, want the reminder's own words", sent[0].Title)
	}
	wantBody(t, sent[0].Body, "6:00 PM")
	if sent[0].Navigate != "/reminders" || sent[0].Tag == "" || sent[0].TraceID == 0 {
		t.Fatalf("notification = %+v, want a target, a tag and the attempt's TraceId", sent[0])
	}
	// Two Trace rows: the attempt the push pointed at, then what the relay answered.
	traces := s.traces(t, "Buy glue")
	if len(traces) != 2 || traces[0]["outcome"] != "attempt" || traces[1]["outcome"] != "sent" {
		t.Fatalf("traces = %+v, want an attempt then its delivery", traces)
	}
	if traces[1]["attemptTraceId"] != float64(sent[0].TraceID) {
		t.Fatalf("delivery = %+v, want it pointing at trace %d", traces[1], sent[0].TraceID)
	}
	if stored := s.stored(t, "Buy glue"); stored.Status != store.ReminderFired {
		t.Fatalf("status = %q, want fired", stored.Status)
	}
}

// A Reminder whose instant is still ahead is not due, whether it is a one-off or a
// repeat: firing it early would be the wrong-time failure the whole time model exists
// to avoid.
func TestSweepLeavesAReminderThatHasNotArrivedAlone(t *testing.T) {
	s := newStack(t)
	ahead := oneOff("Buy glue")
	ahead.LocalDateTime, ahead.EmitAtUtc = "2026-10-29T18:00", "2026-10-29T10:00:00Z"
	s.commit(t, ahead, repeat("Take pills", "2026-10-28T01:00:00Z"))

	s.sweep(t, time.Millisecond)

	if sent, _ := s.relay.sent(); len(sent) != 0 {
		t.Fatalf("sent = %+v, want nothing until the instant arrives", sent)
	}
	for _, text := range []string{"Buy glue", "Take pills"} {
		if stored := s.stored(t, text); stored.Status != store.ReminderResolved {
			t.Fatalf("%s status = %q, want resolved", text, stored.Status)
		}
	}
	if traces := s.traces(t, "Buy glue"); len(traces) != 0 {
		t.Fatalf("traces = %+v, want no attempt for a reminder that did not fire", traces)
	}
}

// A repeat fires and moves: its status stays resolved and its next occurrence is
// rebuilt from the rule and the zone. The moved instant is what stops the next sweep
// from firing the same occurrence again.
func TestSweepAdvancesARepeatInsteadOfFiringItAgain(t *testing.T) {
	s := newStack(t)
	s.commit(t, repeat("Take pills", "2026-10-25T01:00:00Z"))

	s.sweep(t, time.Millisecond)
	s.sweep(t, time.Millisecond)

	sent, _ := s.relay.sent()
	if len(sent) != 1 {
		t.Fatalf("sent = %d, want one notification", len(sent))
	}
	wantBody(t, sent[0].Body, "9:00 AM")
	stored := s.stored(t, "Take pills")
	if stored.Status != store.ReminderResolved {
		t.Fatalf("status = %q, want a repeat to stay resolved", stored.Status)
	}
	// 09:00 in Singapore on the 26th, derived rather than added to the fired instant.
	if stored.NextEmitAtUtc != "2026-10-26T01:00:00Z" {
		t.Fatalf("next = %q, want the occurrence after the one that fired", stored.NextEmitAtUtc)
	}
}

// A server that was down for three days comes back to one notification and the next
// real occurrence, not to three days of backlog: a pile of stale interruptions is the
// failure mode, and a missed daily dose is not made better by three of them.
func TestSweepCatchesUpOnceAfterDowntime(t *testing.T) {
	s := newStack(t)
	s.commit(t, repeat("Take pills", "2026-10-22T01:00:00Z"))

	s.sweep(t, time.Millisecond)
	s.sweep(t, time.Millisecond)

	if sent, _ := s.relay.sent(); len(sent) != 1 {
		t.Fatalf("sent = %d, want one catch-up notification", len(sent))
	}
	if stored := s.stored(t, "Take pills"); stored.NextEmitAtUtc != "2026-10-26T01:00:00Z" {
		t.Fatalf("next = %q, want the next occurrence after now, not after the missed one", stored.NextEmitAtUtc)
	}
}

// Calling a Reminder off is the user's stop button, and it stops the firing: cancel by
// status keeps the row, so the emitter's own query is what never sees it again.
func TestCancelledReminderNeverFires(t *testing.T) {
	s := newStack(t)
	s.commit(t, oneOff("Buy glue"))
	if cancelled, err := s.reminders.Cancel(
		context.Background(), s.userID, s.stored(t, "Buy glue").ReminderID); err != nil || !cancelled {
		t.Fatalf("cancel = %v (%v), want it called off", cancelled, err)
	}

	s.sweep(t, time.Millisecond)

	if sent, _ := s.relay.sent(); len(sent) != 0 {
		t.Fatalf("sent = %+v, want a called-off reminder to stay silent", sent)
	}
	if traces := s.traces(t, "Buy glue"); len(traces) != 0 {
		t.Fatalf("traces = %+v, want no attempt", traces)
	}
	if stored := s.stored(t, "Buy glue"); stored.Status != store.ReminderCancelled {
		t.Fatalf("status = %q, want cancelled", stored.Status)
	}
}

// A Reminder the emitter cannot place — a zone that no longer loads, a rule the
// calendar cannot read — is left alone, and the sweep carries on: one bad row must not
// cost every other reminder in the batch its notification.
func TestSweepSkipsRemindersItCannotPlace(t *testing.T) {
	s := newStack(t)
	unknownZone := repeat("Take pills", "2026-10-25T01:00:00Z")
	unknownZone.Timezone = "Mars/Olympus"
	unreadable := repeat("Water plants", "2026-10-25T01:00:00Z")
	unreadable.Rule = &store.Rule{Frequency: store.FrequencyDaily, Time: "half past nine"}
	s.commit(t, unknownZone, unreadable, oneOff("Buy glue"))

	s.sweep(t, time.Millisecond)

	sent, _ := s.relay.sent()
	if len(sent) != 1 || sent[0].Title != "Buy glue" {
		t.Fatalf("sent = %+v, want the placeable reminder to still fire", sent)
	}
	for _, text := range []string{"Take pills", "Water plants"} {
		if stored := s.stored(t, text); stored.Status != store.ReminderResolved {
			t.Fatalf("%s status = %q, want it left alone", text, stored.Status)
		}
		if traces := s.traces(t, text); len(traces) != 0 {
			t.Fatalf("%s traces = %+v, want no attempt", text, traces)
		}
	}
}

// Nobody subscribed is not a failure: the Reminder still fired, and the delivery row
// still says what the fan-out found — no devices.
func TestFiringIsRecordedWithNoDeviceSubscribed(t *testing.T) {
	s := newStack(t)
	s.relay.result = push.Result{}
	s.commit(t, oneOff("Buy glue"))

	s.sweep(t, time.Millisecond)

	if stored := s.stored(t, "Buy glue"); stored.Status != store.ReminderFired {
		t.Fatalf("status = %q, want fired", stored.Status)
	}
	traces := s.traces(t, "Buy glue")
	if len(traces) != 2 || traces[1]["outcome"] != "sent" || traces[1]["sent"] != float64(0) {
		t.Fatalf("traces = %+v, want a delivery row reporting nothing sent", traces)
	}
}

// A relay that refuses the delivery is recorded, and the Reminder stays fired: the PRD
// takes the miss over a retry, so a reminder never rings late to make up for it.
func TestARefusedDeliveryIsRecordedAndStaysFired(t *testing.T) {
	s := newStack(t)
	s.relay.result = push.Result{Failed: 1}
	s.relay.err = push.ErrVAPID
	s.commit(t, oneOff("Buy glue"))

	s.sweep(t, time.Millisecond)
	s.sweep(t, time.Millisecond)

	if sent, _ := s.relay.sent(); len(sent) != 1 {
		t.Fatalf("sent = %d, want no retry on the next sweep", len(sent))
	}
	traces := s.traces(t, "Buy glue")
	if len(traces) != 2 || traces[1]["outcome"] != "failed" || traces[1]["failed"] != float64(1) {
		t.Fatalf("traces = %+v, want a delivery row reporting the refusal", traces)
	}
	if stored := s.stored(t, "Buy glue"); stored.Status != store.ReminderFired {
		t.Fatalf("status = %q, want the reminder fired regardless", stored.Status)
	}
}

// Two arrived at once go out oldest first and spaced, so the user reads them one after
// the other rather than as a burst that looks like a single interruption.
func TestSweepSpacesDueNotifications(t *testing.T) {
	s := newStack(t)
	first := oneOff("Buy glue")
	first.LocalDateTime, first.EmitAtUtc = "2026-10-25T17:00", "2026-10-25T09:00:00Z"
	s.commit(t, first, oneOff("Post letter"))

	const spacing = 40 * time.Millisecond
	s.sweep(t, spacing)

	sent, arrived := s.relay.sent()
	if len(sent) != 2 || sent[0].Title != "Buy glue" || sent[1].Title != "Post letter" {
		t.Fatalf("sent = %+v, want both in the order they came due", sent)
	}
	if gap := arrived[1].Sub(arrived[0]); gap < spacing {
		t.Fatalf("gap = %v, want at least %v between two notifications", gap, spacing)
	}
}

// Every notification attempt carries a Trace row, and a Trace row hangs off the Job that
// committed the Reminder. A Reminder that outlived its Job has nothing to record the
// attempt against, so it is skipped rather than fired unrecorded.
func TestReminderWithNoJobIsLeftAlone(t *testing.T) {
	s := newStack(t)
	s.commit(t, oneOff("Buy glue"))
	if _, err := s.handle.Write.Exec(`UPDATE Reminder SET SourceJobId = NULL`); err != nil {
		t.Fatalf("clear source job: %v", err)
	}

	s.sweep(t, time.Millisecond)

	if sent, _ := s.relay.sent(); len(sent) != 0 {
		t.Fatalf("sent = %+v, want nothing fired without a trace to write", sent)
	}
	if stored := s.stored(t, "Buy glue"); stored.Status != store.ReminderResolved {
		t.Fatalf("status = %q, want it left resolved", stored.Status)
	}
}
