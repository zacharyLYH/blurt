package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// User is the identity returned to the client. Secrets never live here.
type User struct {
	UserID                   int64  `json:"userId"`
	Name                     string `json:"name"`
	Email                    string `json:"email"`
	Status                   string `json:"status"`
	LastQuestionnaireVersion int    `json:"lastQuestionnaireVersion"`
	// Owner is not a column: it is the one address BLURT_SMTP_USERNAME names,
	// decided per request, because an owner is deployment configuration.
	Owner bool `json:"owner"`
}

// The two states a row can be in. Only Active can sign in.
const (
	StatusPending = "pending"
	StatusActive  = "active"
)

// Answer is one completed questionnaire question.
type Answer struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// UserStore owns the User table. Reads use the read pool so they never queue
// behind the single writer.
type UserStore struct {
	write *sql.DB
	read  *sql.DB
}

// NewUserStore takes the writer and the read pool beside it.
func NewUserStore(write, read *sql.DB) *UserStore {
	return &UserStore{write: write, read: read}
}

// Apply records an application. An address that already has a row keeps it, so
// re-applying cannot resurrect a rejection or reset a review in progress.
func (s *UserStore) Apply(ctx context.Context, name, email string) (User, error) {
	if _, err := s.write.ExecContext(ctx, `
		INSERT INTO User (Name, Email, Status, CreatedAt) VALUES (?, ?, 'pending', ?)
		ON CONFLICT(Email) DO NOTHING`, name, email, now()); err != nil {
		return User{}, fmt.Errorf("apply: %w", err)
	}
	return s.ByEmail(ctx, email)
}

// EnsureActive creates the owner's row, or re-activates it. Boot calls this
// with BLURT_SMTP_USERNAME: the owner cannot apply to themselves, and a
// re-provisioned volume must still let them in.
func (s *UserStore) EnsureActive(ctx context.Context, name, email string) (User, error) {
	row := s.write.QueryRowContext(ctx, `
		INSERT INTO User (Name, Email, Status, CreatedAt) VALUES (?, ?, 'active', ?)
		ON CONFLICT(Email) DO UPDATE SET Name = excluded.Name, Status = 'active'
		RETURNING UserId, Name, Email, Status, LastQuestionnaireVersion`,
		name, email, now())
	return scanUser(row)
}

// Approve is the owner's yes: a pending row becomes active. Any other row is
// left alone, so a double tap is harmless.
func (s *UserStore) Approve(ctx context.Context, userID int64) (User, error) {
	row := s.write.QueryRowContext(ctx, `
		UPDATE User SET Status = 'active' WHERE UserId = ? AND Status = 'pending'
		RETURNING UserId, Name, Email, Status, LastQuestionnaireVersion`, userID)
	return scanUser(row)
}

// Reject is the owner's no. The row is deleted rather than marked, so the
// address can apply again later and nothing about it lingers.
func (s *UserStore) Reject(ctx context.Context, userID int64) (User, error) {
	row := s.write.QueryRowContext(ctx, `
		DELETE FROM User WHERE UserId = ? AND Status = 'pending'
		RETURNING UserId, Name, Email, Status, LastQuestionnaireVersion`, userID)
	return scanUser(row)
}

// List returns every applicant, oldest decision still owed first, so the
// owner's page opens on the work.
func (s *UserStore) List(ctx context.Context) ([]User, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT UserId, Name, Email, Status, LastQuestionnaireVersion FROM User
		ORDER BY CASE Status WHEN 'pending' THEN 0 ELSE 1 END, CreatedAt`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	users := []User{}
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *UserStore) ByID(ctx context.Context, userID int64) (User, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT UserId, Name, Email, Status, LastQuestionnaireVersion FROM User WHERE UserId = ?`, userID)
	return scanUser(row)
}

// ByEmail finds the row an application or the owner seed created. Email is
// NOCASE, so the parser's lowercasing is belt and braces.
func (s *UserStore) ByEmail(ctx context.Context, email string) (User, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT UserId, Name, Email, Status, LastQuestionnaireVersion FROM User WHERE Email = ?`, email)
	return scanUser(row)
}

// SaveQuestionnaire records the completed version and replaces this user's
// questionnaire Memory with the new answers. It lives here because the
// version on User is the record of progress; the Memory rows are the payload.
// The version only moves forward; the answers are a snapshot, so the last
// submission wins even when it carries an older version. A stale client
// therefore updates the text without rolling the version back.
func (s *UserStore) SaveQuestionnaire(ctx context.Context, userID int64, version int, answers []Answer) (User, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("begin questionnaire: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM Memory WHERE UserId = ? AND Tags = 'questionnaire'`, userID); err != nil {
		return User{}, fmt.Errorf("clear questionnaire memory: %w", err)
	}
	for _, answer := range answers {
		text := strings.TrimSpace(answer.Question) + "\n" + strings.TrimSpace(answer.Answer)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO Memory (UserId, Text, Tags, CreatedAt) VALUES (?, ?, 'questionnaire', ?)`,
			userID, text, now()); err != nil {
			return User{}, fmt.Errorf("save answer: %w", err)
		}
	}

	row := tx.QueryRowContext(ctx, `
		UPDATE User SET LastQuestionnaireVersion = MAX(LastQuestionnaireVersion, ?)
		WHERE UserId = ?
		RETURNING UserId, Name, Email, Status, LastQuestionnaireVersion`, version, userID)
	user, err := scanUser(row)
	if err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("commit questionnaire: %w", err)
	}
	return user, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanUser(row scanner) (User, error) {
	var user User
	err := row.Scan(&user.UserID, &user.Name, &user.Email, &user.Status, &user.LastQuestionnaireVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("scan user: %w", err)
	}
	return user, nil
}
