package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// A Reminder is never deleted, so its Status is the whole lifecycle: it starts
// resolved, the emitter turns a one-off fired the moment it rings, and a repeat
// stays resolved because the instant it moves to is what carries it forward.
// Cancelling is a status too, which is what lets the Past list still say what was
// called off instead of the row simply vanishing.
const (
	ReminderResolved  = "resolved"
	ReminderFired     = "fired"
	ReminderCancelled = "cancelled"
)

// windowLimit caps the retrieval window so a busy user's fortnight of reminders
// cannot make one model prompt unbounded.
const windowLimit = 20

// Rule frequencies. A rule keeps only the calendar parts its frequency needs.
const (
	FrequencyDaily   = "daily"
	FrequencyWeekly  = "weekly"
	FrequencyMonthly = "monthly"
	FrequencyYearly  = "yearly"
)

// Rule is a RecurringReminder's repeat pattern, stored as JSON: the frequency,
// the local time of day, and only the calendar parts that frequency names —
// weekday for weekly, day for monthly, month and day for yearly. The zone lives
// on the parent Reminder and the next instant is derived from both, so neither
// is stored here.
type Rule struct {
	Frequency string `json:"frequency"`
	Time      string `json:"time"`
	Weekday   int    `json:"weekday,omitempty"`
	Day       int    `json:"day,omitempty"`
	Month     int    `json:"month,omitempty"`
}

// Reminder is one thing to interrupt the user about. Timezone and LocalDateTime
// are the human truth the model supplied; EmitAtUtc is derived from them by
// the server and is rebuildable, which is why the UTC column is never the
// source. A recurring Reminder's LocalDateTime is empty and its Rule lives in
// RecurringReminder; the stab has neither.
type Reminder struct {
	ReminderID    int64  `json:"reminderId"`
	UserID        int64  `json:"-"`
	Text          string `json:"text"`
	Timezone      string `json:"timezone"`
	LocalDateTime string `json:"localDateTime,omitempty"`
	EmitAtUtc     string `json:"emitAtUtc,omitempty"`
	// Rule and NextEmitAtUtc are set on a recurring Reminder only. Both are
	// derived from the rule and the zone, so the next instant is rebuilt rather
	// than carried forward by a fixed duration.
	Rule          *Rule  `json:"rule,omitempty"`
	NextEmitAtUtc string `json:"nextEmitAtUtc,omitempty"`
	Status        string `json:"status"`
	SourceJobID   int64  `json:"sourceJobId,omitempty"`
	CreatedAt     string `json:"createdAt"`
}

// ReminderStore owns Reminder reads and the two writes this slice's own lifecycle
// needs: the emitter's claim on a due Reminder, and a cancelled status. Creating a
// Reminder is still the Job's transaction, because that is what makes it durable.
type ReminderStore struct {
	write *sql.DB
	read  *sql.DB
}

// insertReminder runs inside the Job's own transaction, and records the row only
// if this Job has not already committed the same text. A resumed turn re-reads
// the Conversation from scratch, so the model re-proposes what it proposed
// before; without this the same Job would leave two identical Reminders behind,
// which is the PRD's duplicate-notification failure mode.
func insertReminder(ctx context.Context, tx execer, reminder Reminder) error {
	if reminder.Status == "" {
		reminder.Status = ReminderResolved
	}
	if reminder.CreatedAt == "" {
		reminder.CreatedAt = now()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO Reminder (UserId, Text, Timezone, LocalDateTime, EmitAtUtc, Status, SourceJobId, CreatedAt)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM Reminder WHERE SourceJobId = ? AND Text = ?)`,
		reminder.UserID, reminder.Text, reminder.Timezone, nullText(reminder.LocalDateTime),
		nullText(reminder.EmitAtUtc),
		reminder.Status, nullID(reminder.SourceJobID), reminder.CreatedAt,
		nullID(reminder.SourceJobID), reminder.Text); err != nil {
		return fmt.Errorf("insert reminder: %w", err)
	}
	if reminder.Rule != nil {
		return insertRecurring(ctx, tx, reminder)
	}
	return nil
}

// insertRecurring writes a recurring Reminder's rule beside its Reminder row, in
// the same transaction: it selects the ReminderId by the same predicate that
// created the row rather than assuming one, and IGNORE keeps a repeated step from
// adding a second rule. Which instant fires next is derived from the rule and the
// zone, so the rule is the only thing stored.
func insertRecurring(ctx context.Context, tx execer, reminder Reminder) error {
	raw, err := json.Marshal(reminder.Rule)
	if err != nil {
		return fmt.Errorf("encode rule: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO RecurringReminder (ReminderId, Rule, NextEmitAtUtc)
		SELECT ReminderId, ?, ? FROM Reminder WHERE SourceJobId = ? AND Text = ?`,
		string(raw), reminder.NextEmitAtUtc, nullID(reminder.SourceJobID), reminder.Text); err != nil {
		return fmt.Errorf("insert recurring reminder: %w", err)
	}
	return nil
}

func NewReminderStore(write, read *sql.DB) *ReminderStore {
	return &ReminderStore{write: write, read: read}
}

// reminderColumns is the row shape every Reminder read shares: the Reminder's own
// columns, the repeat's rule and next occurrence beside them, and the Job the
// Reminder came from. A repeat leaves its own instant columns null, so the instant
// it is about is the rule's next occurrence and the COALESCE is what reads it —
// without that a repeat would look like it never comes due.
const reminderColumns = `r.ReminderId, r.UserId, r.Text, r.Timezone, COALESCE(r.LocalDateTime, ''),
		COALESCE(r.EmitAtUtc, ''), r.Status, r.CreatedAt, COALESCE(c.NextEmitAtUtc, ''), COALESCE(c.Rule, ''),
		COALESCE(r.SourceJobId, 0)`

// scanReminders reads the columns above. A repeat comes back with its rule decoded,
// so a caller can tell a rule from a one-off without reading the extension table
// itself.
func scanReminders(rows *sql.Rows) ([]Reminder, error) {
	reminders := []Reminder{}
	for rows.Next() {
		var reminder Reminder
		var rule string
		if err := rows.Scan(&reminder.ReminderID, &reminder.UserID, &reminder.Text, &reminder.Timezone,
			&reminder.LocalDateTime, &reminder.EmitAtUtc, &reminder.Status, &reminder.CreatedAt,
			&reminder.NextEmitAtUtc, &rule, &reminder.SourceJobID); err != nil {
			return nil, fmt.Errorf("scan reminder: %w", err)
		}
		if rule != "" {
			if err := json.Unmarshal([]byte(rule), &reminder.Rule); err != nil {
				return nil, fmt.Errorf("decode rule: %w", err)
			}
		}
		reminders = append(reminders, reminder)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read reminders: %w", err)
	}
	return reminders, nil
}

// Window returns the user's Reminders whose next instant falls inside [from, to).
// The PRD builds retrieval on this, from two days before the target so a correction can
// still find its Reminder, to fourteen days after so a relative one lands inside it.
// Status is deliberately not filtered: a fired or cancelled Reminder is still context the
// model should see.
//
// A repeat has no one-off instant: its own EmitAtUtc is null, and the instant it is about
// is the rule's next occurrence, so the filter and the order both read the effective
// instant. Without that a recurring Reminder would be invisible to retrieval.
func (s *ReminderStore) Window(ctx context.Context, userID int64, from, to time.Time) ([]Reminder, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT `+reminderColumns+`
		FROM Reminder r LEFT JOIN RecurringReminder c ON c.ReminderId = r.ReminderId
		WHERE r.UserId = ? AND COALESCE(c.NextEmitAtUtc, r.EmitAtUtc) >= ?
		  AND COALESCE(c.NextEmitAtUtc, r.EmitAtUtc) < ?
		ORDER BY COALESCE(c.NextEmitAtUtc, r.EmitAtUtc) LIMIT ?`,
		userID, timestamp(from), timestamp(to), windowLimit)
	if err != nil {
		return nil, fmt.Errorf("reminder window: %w", err)
	}
	defer rows.Close()
	return scanReminders(rows)
}

// Due returns the Reminders whose instant has arrived, oldest first: every resolved
// one-off whose EmitAtUtc and every resolved repeat whose next occurrence is at or
// before now. This is the emitter's query, so it is the one read that filters on
// Status — a fired or cancelled Reminder is still context the model should see, but
// it is never due a second time.
func (s *ReminderStore) Due(ctx context.Context, now time.Time, limit int) ([]Reminder, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT `+reminderColumns+`
		FROM Reminder r LEFT JOIN RecurringReminder c ON c.ReminderId = r.ReminderId
		WHERE r.Status = ? AND COALESCE(c.NextEmitAtUtc, r.EmitAtUtc) <= ?
		ORDER BY COALESCE(c.NextEmitAtUtc, r.EmitAtUtc) LIMIT ?`,
		ReminderResolved, timestamp(now), limit)
	if err != nil {
		return nil, fmt.Errorf("due reminders: %w", err)
	}
	defer rows.Close()
	return scanReminders(rows)
}

// Fire is the emitter's claim on one Reminder plus the Trace row for the attempt,
// in one transaction, and reports whether this caller is the one that got it. The
// claim is a compare-and-swap on the single thing that makes the Reminder due: a
// one-off moves resolved to fired, and a repeat moves to the instant it fires next.
// Only the caller that reads the old value goes on to send, so a second sweep, a
// restart, or a cancel that landed first cannot put the same firing on the wire
// twice. The returned TraceId is the notification id the push carries, and the row
// the delivery outcome then points back at.
func (s *ReminderStore) Fire(ctx context.Context, reminder Reminder, next time.Time, detail map[string]any) (int64, bool, error) {
	if reminder.Rule != nil && next.IsZero() {
		// A repeat cannot advance without the instant it moves to. Refusing here is
		// what stops a pattern the calendar cannot place from firing every sweep.
		return 0, false, nil
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("begin reminder fire: %w", err)
	}
	defer tx.Rollback()

	var claimed bool
	if reminder.Rule == nil {
		res, err := tx.ExecContext(ctx,
			`UPDATE Reminder SET Status = ? WHERE ReminderId = ? AND Status = ?`,
			ReminderFired, reminder.ReminderID, ReminderResolved)
		if err != nil {
			return 0, false, fmt.Errorf("claim reminder: %w", err)
		}
		if claimed, err = claimedRow(res); err != nil {
			return 0, false, err
		}
	} else {
		res, err := tx.ExecContext(ctx, `
			UPDATE RecurringReminder SET NextEmitAtUtc = ?
			WHERE ReminderId = ? AND NextEmitAtUtc = ?
			  AND EXISTS (SELECT 1 FROM Reminder WHERE ReminderId = ? AND Status = ?)`,
			timestamp(next), reminder.ReminderID, reminder.NextEmitAtUtc,
			reminder.ReminderID, ReminderResolved)
		if err != nil {
			return 0, false, fmt.Errorf("claim recurring reminder: %w", err)
		}
		if claimed, err = claimedRow(res); err != nil {
			return 0, false, err
		}
	}
	if !claimed {
		return 0, false, nil
	}

	traceID, err := recordTrace(ctx, tx, NotificationStage, reminder.SourceJobID, reminder.ReminderID, detail)
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit reminder fire: %w", err)
	}
	return traceID, true, nil
}

// claimedRow reports whether a claim's UPDATE matched a row, which is how a claim
// says this caller is the one that got it.
func claimedRow(res sql.Result) (bool, error) {
	matched, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read affected rows: %w", err)
	}
	return matched > 0, nil
}

// RecordDelivery appends the outcome of one firing attempt that already has its
// Trace row. Trace is append-only, so the outcome is a second row pointing back at
// the attempt rather than an edit of it — which is what keeps a failed delivery
// visible instead of silent.
func (s *ReminderStore) RecordDelivery(ctx context.Context, reminder Reminder, detail map[string]any) error {
	_, err := recordTrace(ctx, s.write, NotificationStage, reminder.SourceJobID, reminder.ReminderID, detail)
	return err
}

// Cancel calls off one of the user's Reminders and reports whether it was still
// scheduled. A Reminder that already fired, or one that belongs to somebody else, is
// not this caller's to call off — so a false keeps a fired Reminder's history honest
// and stops one user reaching another's ids.
func (s *ReminderStore) Cancel(ctx context.Context, userID, reminderID int64) (bool, error) {
	res, err := s.write.ExecContext(ctx,
		`UPDATE Reminder SET Status = ? WHERE ReminderId = ? AND UserId = ? AND Status = ?`,
		ReminderCancelled, reminderID, userID, ReminderResolved)
	if err != nil {
		return false, fmt.Errorf("cancel reminder: %w", err)
	}
	return claimedRow(res)
}

// listLimit caps each side of the Reminders page, the way openJobsLimit caps the
// sidebar: a long-running account must not make one page unbounded.
const listLimit = 50

// Upcoming is what the Reminders page shows first: everything still resolved,
// soonest first. An overdue one-off sits at the top rather than in the past, because
// nothing has fired it yet — the emitter's own next sweep is what moves it.
func (s *ReminderStore) Upcoming(ctx context.Context, userID int64) ([]Reminder, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT `+reminderColumns+`
		FROM Reminder r LEFT JOIN RecurringReminder c ON c.ReminderId = r.ReminderId
		WHERE r.UserId = ? AND r.Status = ?
		ORDER BY COALESCE(c.NextEmitAtUtc, r.EmitAtUtc) LIMIT ?`,
		userID, ReminderResolved, listLimit)
	if err != nil {
		return nil, fmt.Errorf("upcoming reminders: %w", err)
	}
	defer rows.Close()
	return scanReminders(rows)
}

// Past is what already happened, newest first: a one-off the emitter fired and
// anything the user called off. A repeat never lands here — it stays resolved between
// firings, so what it did is in its Trace rows rather than in its status.
func (s *ReminderStore) Past(ctx context.Context, userID int64) ([]Reminder, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT `+reminderColumns+`
		FROM Reminder r LEFT JOIN RecurringReminder c ON c.ReminderId = r.ReminderId
		WHERE r.UserId = ? AND r.Status <> ?
		ORDER BY COALESCE(c.NextEmitAtUtc, r.EmitAtUtc) DESC LIMIT ?`,
		userID, ReminderResolved, listLimit)
	if err != nil {
		return nil, fmt.Errorf("past reminders: %w", err)
	}
	defer rows.Close()
	return scanReminders(rows)
}
