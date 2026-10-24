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
	LastQuestionnaireVersion int    `json:"lastQuestionnaireVersion"`
}

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

func NewUserStore(write, read *sql.DB) *UserStore {
	return &UserStore{write: write, read: read}
}

// EnsureUser creates the user on first login or refreshes the name.
func (s *UserStore) EnsureUser(ctx context.Context, name, email string) (User, error) {
	row := s.write.QueryRowContext(ctx, `
		INSERT INTO User (Name, Email, CreatedAt) VALUES (?, ?, ?)
		ON CONFLICT(Email) DO UPDATE SET Name = excluded.Name
		RETURNING UserId, Name, Email, LastQuestionnaireVersion`,
		name, email, now())
	return scanUser(row)
}

func (s *UserStore) ByID(ctx context.Context, userID int64) (User, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT UserId, Name, Email, LastQuestionnaireVersion FROM User WHERE UserId = ?`, userID)
	return scanUser(row)
}

// SaveQuestionnaire records the completed version and replaces this user's
// questionnaire Memory with the new answers. It lives here because the
// version on User is the record of progress; the Memory rows are the payload.
// The version only moves forward.
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
		RETURNING UserId, Name, Email, LastQuestionnaireVersion`, version, userID)
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
	err := row.Scan(&user.UserID, &user.Name, &user.Email, &user.LastQuestionnaireVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("scan user: %w", err)
	}
	return user, nil
}
