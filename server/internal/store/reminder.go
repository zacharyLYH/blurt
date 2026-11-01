package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrReminderStale is a rewrite that matched no row: the Reminder fired, or was called off,
// between the lookup and the commit, so the change has nowhere to land.
var ErrReminderStale = errors.New("reminder is no longer resolved")

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

// rowLimit caps one text lookup, so a busy user's rows cannot make one model prompt
// unbounded.
const rowLimit = 20

// maxQueryWords caps one lookup's ORs: a sentence of a query is a handful of words worth
// searching, not a page of them.
const maxQueryWords = 5

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
// are the local notification time the model supplied; EmitAtUtc is the same
// instant in UTC, derived by the server and rebuildable, which is why the UTC
// column is never the source. The event's own clock, when it differs, lives in
// Text. A recurring Reminder's LocalDateTime is empty and its Rule lives in
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

// NewReminderStore takes the writer and the read pool beside it.
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

// Search is the reminder lookup the agent's find_reminders tool runs: the user's own rows
// whose text carries any word of the query, soonest first. It is LIKE rather than BM25 because a
// Reminder is not indexed, and a leading-wildcard LIKE cannot use an index either way: the owner
// is what bounds the read, and idx_reminder_window's leading column is what serves that
// (`SEARCH r USING INDEX idx_reminder_window (UserId=?)`), so a scan is over one user's own rows.
// The model reads the five rows and decides which one a capture means, so recall matters more
// than ranking.
func (s *ReminderStore) Search(ctx context.Context, userID int64, query string, limit int) ([]Reminder, error) {
	words := queryWords(query, maxQueryWords)
	if len(words) == 0 {
		return nil, nil
	}
	if limit < 1 || limit > rowLimit {
		limit = rowLimit
	}
	clauses := make([]string, 0, len(words))
	args := []any{userID}
	for _, word := range words {
		clauses = append(clauses, `r.Text LIKE ?`)
		args = append(args, "%"+word+"%")
	}
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, `
		SELECT `+reminderColumns+`
		FROM Reminder r LEFT JOIN RecurringReminder c ON c.ReminderId = r.ReminderId
		WHERE r.UserId = ? AND (`+strings.Join(clauses, " OR ")+`)
		ORDER BY COALESCE(c.NextEmitAtUtc, r.EmitAtUtc) LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("reminder search: %w", err)
	}
	defer rows.Close()
	return scanReminders(rows)
}

// ByID reads one of the user's own Reminders. The owner is in the predicate, so an id that
// belongs to somebody else is simply not there: whatever a model wrote, it cannot reach a row
// the caller does not own.
func (s *ReminderStore) ByID(ctx context.Context, userID, reminderID int64) (Reminder, bool, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT `+reminderColumns+`
		FROM Reminder r LEFT JOIN RecurringReminder c ON c.ReminderId = r.ReminderId
		WHERE r.ReminderId = ? AND r.UserId = ?`, reminderID, userID)
	if err != nil {
		return Reminder{}, false, fmt.Errorf("read reminder: %w", err)
	}
	defer rows.Close()
	reminders, err := scanReminders(rows)
	if err != nil {
		return Reminder{}, false, err
	}
	if len(reminders) == 0 {
		return Reminder{}, false, nil
	}
	return reminders[0], true, nil
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

// Fire is the emitter's claim on one Reminder plus the Trace row for the attempt, in
// one transaction, and reports whether this caller is the one that got it. The claim
// is a compare-and-swap on the single thing that makes the Reminder due: a one-off
// moves resolved to fired, and a repeat moves to the instant it fires next. Only the
// caller that reads the old value goes on to send, so a second sweep, a restart, or a
// cancel that landed first cannot put the same firing on the wire twice.
func (s *ReminderStore) Fire(ctx context.Context, reminder Reminder, next time.Time, detail map[string]any) (bool, error) {
	if reminder.Rule != nil && next.IsZero() {
		// A repeat cannot advance without the instant it moves to. Refusing here is
		// what stops a pattern the calendar cannot place from firing every sweep.
		return false, nil
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin reminder fire: %w", err)
	}
	defer tx.Rollback()

	var claimed bool
	if reminder.Rule == nil {
		res, err := tx.ExecContext(ctx,
			`UPDATE Reminder SET Status = ? WHERE ReminderId = ? AND Status = ?`,
			ReminderFired, reminder.ReminderID, ReminderResolved)
		if err != nil {
			return false, fmt.Errorf("claim reminder: %w", err)
		}
		if claimed, err = claimedRow(res); err != nil {
			return false, err
		}
	} else {
		res, err := tx.ExecContext(ctx, `
			UPDATE RecurringReminder SET NextEmitAtUtc = ?
			WHERE ReminderId = ? AND NextEmitAtUtc = ?
			  AND EXISTS (SELECT 1 FROM Reminder WHERE ReminderId = ? AND Status = ?)`,
			timestamp(next), reminder.ReminderID, reminder.NextEmitAtUtc,
			reminder.ReminderID, ReminderResolved)
		if err != nil {
			return false, fmt.Errorf("claim recurring reminder: %w", err)
		}
		if claimed, err = claimedRow(res); err != nil {
			return false, err
		}
	}
	if !claimed {
		return false, nil
	}

	if _, err := recordTrace(ctx, tx, NotificationStage, reminder.SourceJobID, reminder.ReminderID, detail); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit reminder fire: %w", err)
	}
	return true, nil
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

// rewriteReminder replaces one resolved Reminder's own fields in place, keeping its id, its
// status, its CreatedAt and the Job that made it. The predicate is Cancel's — this user's own
// row, still resolved — so a row that has left the schedule cannot be changed behind the user's
// back, and a change that finds nothing to change is refused rather than invented.
func rewriteReminder(ctx context.Context, tx execer, reminder Reminder) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE Reminder SET Text = ?, Timezone = ?, LocalDateTime = ?, EmitAtUtc = ?
		WHERE ReminderId = ? AND UserId = ? AND Status = ?`,
		reminder.Text, reminder.Timezone, nullText(reminder.LocalDateTime), nullText(reminder.EmitAtUtc),
		reminder.ReminderID, reminder.UserID, ReminderResolved)
	if err != nil {
		return fmt.Errorf("rewrite reminder: %w", err)
	}
	matched, err := claimedRow(res)
	if err != nil {
		return err
	}
	if !matched {
		return ErrReminderStale
	}
	if reminder.Rule == nil {
		// A repeat that became a one-off keeps no rule: the rule's next instant would
		// otherwise outlive the row's own and read as the later of the two.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM RecurringReminder WHERE ReminderId = ?`, reminder.ReminderID); err != nil {
			return fmt.Errorf("clear reminder rule: %w", err)
		}
		return nil
	}
	raw, err := json.Marshal(reminder.Rule)
	if err != nil {
		return fmt.Errorf("encode rule: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO RecurringReminder (ReminderId, Rule, NextEmitAtUtc) VALUES (?, ?, ?)
		ON CONFLICT (ReminderId) DO UPDATE SET Rule = excluded.Rule, NextEmitAtUtc = excluded.NextEmitAtUtc`,
		reminder.ReminderID, string(raw), reminder.NextEmitAtUtc); err != nil {
		return fmt.Errorf("rewrite reminder rule: %w", err)
	}
	return nil
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
