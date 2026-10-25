package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ReminderResolved is the status a committed Reminder starts in. The schema's CHECK also allows fired
// and cancelled, which the emitter and cancellation will name when they land; a Reminder is never
// deleted, so cancelling keeps its history.
const ReminderResolved = "resolved"

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

// ReminderStore reads Reminders. Writes belong to the store that owns the
// lifecycle — Job for the AgentLoop, and the emitter's own status updates land
// with the emission slice — so this one holds the read pool only.
type ReminderStore struct {
	read *sql.DB
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

func NewReminderStore(read *sql.DB) *ReminderStore {
	return &ReminderStore{read: read}
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
		SELECT r.ReminderId, r.UserId, r.Text, r.Timezone, COALESCE(r.LocalDateTime, ''),
		       COALESCE(r.EmitAtUtc, ''), r.Status, r.CreatedAt,
		       COALESCE(c.NextEmitAtUtc, ''), COALESCE(c.Rule, '')
		FROM Reminder r LEFT JOIN RecurringReminder c ON c.ReminderId = r.ReminderId
		WHERE r.UserId = ? AND COALESCE(c.NextEmitAtUtc, r.EmitAtUtc) >= ?
		  AND COALESCE(c.NextEmitAtUtc, r.EmitAtUtc) < ?
		ORDER BY COALESCE(c.NextEmitAtUtc, r.EmitAtUtc) LIMIT ?`,
		userID, timestamp(from), timestamp(to), windowLimit)
	if err != nil {
		return nil, fmt.Errorf("reminder window: %w", err)
	}
	defer rows.Close()

	reminders := []Reminder{}
	for rows.Next() {
		var reminder Reminder
		var rule string
		if err := rows.Scan(&reminder.ReminderID, &reminder.UserID, &reminder.Text, &reminder.Timezone,
			&reminder.LocalDateTime, &reminder.EmitAtUtc, &reminder.Status, &reminder.CreatedAt,
			&reminder.NextEmitAtUtc, &rule); err != nil {
			return nil, fmt.Errorf("scan reminder: %w", err)
		}
		if rule != "" {
			// A repeat comes back with the pattern it repeats on, so a caller can tell
			// a rule from a one-off without reading the extension table itself.
			if err := json.Unmarshal([]byte(rule), &reminder.Rule); err != nil {
				return nil, fmt.Errorf("decode rule: %w", err)
			}
		}
		reminders = append(reminders, reminder)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reminder window: %w", err)
	}
	return reminders, nil
}
