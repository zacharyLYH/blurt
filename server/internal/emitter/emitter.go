// Package emitter fires due Reminders. It is the one part of Blurt that runs on a
// clock rather than on something the user just did: a sweep turns a scheduled
// instant into a push that opens the Reminder.
package emitter

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"blurt/internal/agent"
	"blurt/internal/push"
	"blurt/internal/queue"
	"blurt/internal/store"
)

const (
	// defaultPoll is the PRD's once-a-second sweep of due Reminders.
	defaultPoll = time.Second
	// dueLimit bounds one sweep, so a backlog cannot make a single tick unbounded.
	dueLimit = 50
	// clockLayout is how a notification renders the instant it is about.
	clockLayout = "3:04 PM MST"
)

// Emitter fires due Reminders until its context ends. Reminders and Pusher are the
// wiring; the rest has a default, so a caller sets only what it wants to change.
type Emitter struct {
	Reminders *store.ReminderStore
	// Pusher owns device selection: an empty result means nobody is subscribed,
	// which is recorded as a skip rather than a failed delivery.
	Pusher push.Notifier
	// Poll is how often due Reminders are looked for; zero means defaultPoll.
	Poll time.Duration
	// Now is the clock the sweep reads. Nil means time.Now.
	Now func() time.Time
}

// Run blocks until ctx is cancelled.
func (e *Emitter) Run(ctx context.Context) {
	queue.Every(ctx, e.poll(), func(ctx context.Context) { e.sweep(ctx) })
}

// sweep fires everything that has arrived. Each due Reminder gets its own pipe: one
// slow relay cannot hold up the rest of the batch, and a firing claims its Reminder
// with a compare-and-swap, so concurrent sweeps cannot send the same occurrence
// twice. The sweep waits for its pipes before returning, which keeps one tick from
// overlapping the next.
func (e *Emitter) sweep(ctx context.Context) {
	due, err := e.Reminders.Due(ctx, e.now(), dueLimit)
	if err != nil {
		slog.Error("emitter due query", "err", err)
		return
	}
	var pipes sync.WaitGroup
	for _, reminder := range due {
		pipes.Add(1)
		go func(reminder store.Reminder) {
			defer pipes.Done()
			e.fire(ctx, reminder)
		}(reminder)
	}
	pipes.Wait()
}

// fire claims one Reminder, records the attempt, and sends the push that opens it.
// Claiming and the attempt commit together: a crash after that costs this one firing
// rather than leaving a Reminder that rings again on every restart. A Reminder that
// already fired, was called off, or has no Job to write its Trace row against is left
// alone.
func (e *Emitter) fire(ctx context.Context, reminder store.Reminder) {
	if reminder.SourceJobID == 0 {
		// A Trace row hangs off the Job that committed the Reminder. With no Job there
		// is nothing to record the attempt against, so this is left alone rather than
		// fired unrecorded.
		slog.Warn("emitter reminder has no job", "reminder_id", reminder.ReminderID)
		return
	}
	next, ok := e.next(reminder)
	if !ok {
		slog.Warn("emitter cannot place a reminder", "reminder_id", reminder.ReminderID,
			"timezone", reminder.Timezone)
		return
	}
	claimed, err := e.Reminders.Fire(ctx, reminder, next, attemptDetail(reminder))
	if err != nil {
		slog.Error("emitter fire", "reminder_id", reminder.ReminderID, "err", err)
		return
	}
	if !claimed {
		return
	}

	sentAt := e.now().UTC().Format(time.RFC3339)

	// The fan-out owns device selection. A zero result means nobody is
	// subscribed: the Reminder still fires, so its lifecycle does not depend on
	// being signed in. A subscription outlives its login cookie; explicit logout
	// deletes the row that owns it.
	result, err := e.Pusher.NotifyUser(ctx, reminder.UserID, message(reminder))
	detail := map[string]any{
		"outcome": "sent", "sentAt": sentAt,
		"sent": result.Sent, "failed": result.Failed, "pruned": result.Pruned,
	}
	switch {
	case err != nil:
		// Best effort by design: a relay that refused leaves the Reminder fired, which
		// is the PRD's own call — a stale delivery is worse than a miss.
		detail["outcome"], detail["error"] = "failed", err.Error()
	case result == (push.Result{}):
		detail["outcome"] = "skipped_no_device"
	}
	if err := e.Reminders.RecordDelivery(ctx, reminder, detail); err != nil {
		slog.Error("emitter delivery trace", "reminder_id", reminder.ReminderID, "err", err)
	}
	if detail["outcome"] == "skipped_no_device" {
		slog.Info("emitter skipped push: no subscribed device", "reminder_id", reminder.ReminderID,
			"user_id", reminder.UserID)
		return
	}
	slog.Info("emitter fired", "reminder_id", reminder.ReminderID, "user_id", reminder.UserID,
		"sent", result.Sent, "failed", result.Failed, "pruned", result.Pruned)
}

// next is the instant a repeat moves to, derived from its rule and the zone the user
// meant. A one-off has nothing to move to: it is the status change alone. A reminder
// is advanced from now rather than from the occurrence it is firing, so a server that
// was down for a day comes back to one firing and the next real occurrence, not to a
// backlog of missed ones.
func (e *Emitter) next(reminder store.Reminder) (time.Time, bool) {
	if reminder.Rule == nil {
		return time.Time{}, true
	}
	loc, err := time.LoadLocation(reminder.Timezone)
	if err != nil {
		return time.Time{}, false
	}
	return agent.NextEmit(*reminder.Rule, e.now(), loc)
}

// message is what the device shows: the Reminder's own words as the title, and the
// instant it is about rendered in the zone the user meant. The tag is per Reminder
// rather than per firing, so a repeat replaces its previous notification instead of
// stacking one per occurrence. The page it opens is the Reminder itself, because a
// notification is just a Reminder the poll found due.
func message(reminder store.Reminder) push.Message {
	return push.Message{
		Title:      reminder.Text,
		Body:       localTime(reminder),
		Tag:        fmt.Sprintf("reminder-%d", reminder.ReminderID),
		Navigate:   fmt.Sprintf("/notifications/%d", reminder.ReminderID),
		ReminderID: reminder.ReminderID,
	}
}

// localTime renders the instant the notification is about in the user's own zone,
// with the zone beside it: "6:00 PM SGT". The zone comes from the Reminder rather than
// from the device, because a Reminder is scheduled in the zone the user meant.
func localTime(reminder store.Reminder) string {
	at, err := time.Parse(time.RFC3339, effective(reminder))
	if err != nil {
		// A committed Reminder always carries an instant, so this is a corrupted row:
		// the local time it was stored with is a truer body than nothing at all.
		return reminder.LocalDateTime
	}
	if loc, err := time.LoadLocation(reminder.Timezone); err == nil {
		at = at.In(loc)
	}
	return at.Format(clockLayout)
}

// effective is the instant a Reminder is about: a repeat's next occurrence, or a
// one-off's derived instant. It is the same COALESCE the queries read, in Go.
func effective(reminder store.Reminder) string {
	if reminder.Rule != nil {
		return reminder.NextEmitAtUtc
	}
	return reminder.EmitAtUtc
}

// attemptDetail is the Trace payload for one firing: what was scheduled, in the user's
// own words and zone, so a wrong firing can be read back against the Reminder that
// caused it rather than guessed at.
func attemptDetail(reminder store.Reminder) map[string]any {
	return map[string]any{
		"outcome": "attempt", "text": reminder.Text, "timezone": reminder.Timezone,
		"instant": effective(reminder), "recurring": reminder.Rule != nil,
	}
}

func (e *Emitter) poll() time.Duration {
	if e.Poll > 0 {
		return e.Poll
	}
	return defaultPoll
}

func (e *Emitter) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}
	return e.Now()
}
