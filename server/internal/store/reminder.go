package store

import (
	"context"
	"database/sql"
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
		reminder.UserID, reminder.Text, reminder.Timezone, reminder.LocalDateTime, reminder.EmitAtUtc,
		reminder.Status, nullID(reminder.SourceJobID), reminder.CreatedAt,
		nullID(reminder.SourceJobID), reminder.Text); err != nil {
		return fmt.Errorf("insert reminder: %w", err)
	}
	return nil
}

func NewReminderStore(read *sql.DB) *ReminderStore {
	return &ReminderStore{read: read}
}

// Window returns the user's Reminders whose derived instant falls inside
// [from, to). The PRD builds retrieval on this, from two days before the target
// so a correction can still find its Reminder, to fourteen days after so a
// relative one lands inside it. Status is deliberately not filtered: a fired or
// cancelled Reminder is still context the model should see.
func (s *ReminderStore) Window(ctx context.Context, userID int64, from, to time.Time) ([]Reminder, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT ReminderId, UserId, Text, Timezone, LocalDateTime, EmitAtUtc, Status, CreatedAt
		FROM Reminder
		WHERE UserId = ? AND EmitAtUtc >= ? AND EmitAtUtc < ?
		ORDER BY EmitAtUtc LIMIT ?`,
		userID, timestamp(from), timestamp(to), windowLimit)
	if err != nil {
		return nil, fmt.Errorf("reminder window: %w", err)
	}
	defer rows.Close()

	reminders := []Reminder{}
	for rows.Next() {
		var reminder Reminder
		if err := rows.Scan(&reminder.ReminderID, &reminder.UserID, &reminder.Text, &reminder.Timezone,
			&reminder.LocalDateTime, &reminder.EmitAtUtc, &reminder.Status, &reminder.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan reminder: %w", err)
		}
		reminders = append(reminders, reminder)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reminder window: %w", err)
	}
	return reminders, nil
}
