package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// A notification's feedback is one choice from a closed set, because the form asks
// "what went wrong?" rather than inviting a free answer. The slugs are what the
// client sends; the label lives in the UI.
const (
	FeedbackWrongReminder     = "wrong_reminder"
	FeedbackWrongDateTimeZone = "wrong_date_time_zone"
	FeedbackWrongContext      = "wrong_or_irrelevant_context"
	FeedbackDuplicate         = "duplicate_too_many"
	FeedbackOther             = "other"
)

// FailureModes is the whole allowed set, in the order the form offers it.
var FailureModes = []string{
	FeedbackWrongReminder,
	FeedbackWrongDateTimeZone,
	FeedbackWrongContext,
	FeedbackDuplicate,
	FeedbackOther,
}

// ValidFailureMode reports whether a submitted slug is one the form offered.
func ValidFailureMode(mode string) bool {
	return slices.Contains(FailureModes, mode)
}

// NotificationFeedback is one report. It has its own id rather than being keyed by
// the Reminder: a repeat fires many times, so the same ReminderId is expected on many
// reports, and each submission is a new row.
type NotificationFeedback struct {
	NotificationFeedbackID int64  `json:"notificationFeedbackId"`
	ReminderID             int64  `json:"-"`
	FailureMode            string `json:"failureMode"`
	Comment                string `json:"comment,omitempty"`
	CreatedAt              string `json:"createdAt"`
}

// Notification is the Reminder as the page and the calendar show it: its own words,
// its zone, and the instant it is (next) about. At is derived from the rule or the
// one-off, so it needs no instance row.
type Notification struct {
	ReminderID int64  `json:"reminderId"`
	Text       string `json:"text"`
	Timezone   string `json:"timezone"`
	At         string `json:"at"`
}

// NotificationStore owns notification reads and feedback. It never reads Trace: the
// Reminder is the record the product shows, and feedback is its own row.
type NotificationStore struct {
	write *sql.DB
	read  *sql.DB
}

func NewNotificationStore(write, read *sql.DB) *NotificationStore {
	return &NotificationStore{write: write, read: read}
}

// calendarLimit bounds one month's read, so a pathological account cannot make one
// page unbounded.
const calendarLimit = 500

// Detail loads one of the caller's Reminders as the notification page shows it. A
// missing Reminder, somebody else's, and one that was called off are all ErrNotFound:
// the page has nothing to show or collect feedback about in any of those cases.
func (s *NotificationStore) Detail(ctx context.Context, userID, reminderID int64) (Notification, error) {
	var notification Notification
	err := s.read.QueryRowContext(ctx, `
		SELECT r.ReminderId, r.Text, r.Timezone, COALESCE(c.NextEmitAtUtc, r.EmitAtUtc, '')
		FROM Reminder r LEFT JOIN RecurringReminder c ON c.ReminderId = r.ReminderId
		WHERE r.ReminderId = ? AND r.UserId = ? AND r.Status <> ?`,
		reminderID, userID, ReminderCancelled).
		Scan(&notification.ReminderID, &notification.Text, &notification.Timezone, &notification.At)
	if errors.Is(err, sql.ErrNoRows) {
		return Notification{}, ErrNotFound
	}
	if err != nil {
		return Notification{}, fmt.Errorf("load notification: %w", err)
	}
	return notification, nil
}

// AddFeedback records one report and returns its id. The insert is guarded by an
// EXISTS on the caller's own Reminder, so ownership and the write are one atomic
// statement. There is no uniqueness on ReminderId: a repeat fires again, and the next
// report is a new row.
func (s *NotificationStore) AddFeedback(ctx context.Context, userID, reminderID int64, mode, comment string) (int64, error) {
	var id int64
	err := s.write.QueryRowContext(ctx, `
		INSERT INTO NotificationFeedback (ReminderId, FailureMode, Comment, CreatedAt)
		SELECT ?, ?, ?, ?
		WHERE EXISTS (
			SELECT 1 FROM Reminder WHERE ReminderId = ? AND UserId = ? AND Status <> ?
		)
		RETURNING NotificationFeedbackId`,
		reminderID, mode, nullText(comment), now(), reminderID, userID, ReminderCancelled).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("insert feedback: %w", err)
	}
	return id, nil
}

// Month is the calendar's read: the caller's Reminders whose instant falls inside
// [from, to), soonest first. A repeat contributes its next occurrence, which is the
// only instant the server knows without inventing one.
func (s *NotificationStore) Month(ctx context.Context, userID int64, from, to time.Time) ([]Notification, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT r.ReminderId, r.Text, r.Timezone, COALESCE(c.NextEmitAtUtc, r.EmitAtUtc)
		FROM Reminder r LEFT JOIN RecurringReminder c ON c.ReminderId = r.ReminderId
		WHERE r.UserId = ? AND r.Status <> ?
		  AND COALESCE(c.NextEmitAtUtc, r.EmitAtUtc) >= ?
		  AND COALESCE(c.NextEmitAtUtc, r.EmitAtUtc) < ?
		ORDER BY 4 LIMIT ?`,
		userID, ReminderCancelled, timestamp(from), timestamp(to), calendarLimit)
	if err != nil {
		return nil, fmt.Errorf("calendar month: %w", err)
	}
	defer rows.Close()

	entries := []Notification{}
	for rows.Next() {
		var entry Notification
		if err := rows.Scan(&entry.ReminderID, &entry.Text, &entry.Timezone, &entry.At); err != nil {
			return nil, fmt.Errorf("scan calendar entry: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read calendar month: %w", err)
	}
	return entries, nil
}
