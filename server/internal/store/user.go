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

// NewUserStore takes the writer and the read pool beside it.
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

// ModelSettings is the model a user configured on the setup page. The key is the
// ciphertext at rest, not the secret: whoever calls this decides how to open it.
// Empty strings mean unconfigured, not broken.
type ModelSettings struct {
	Provider string
	Model    string
	Endpoint string
	APIKey   string
}

// ModelSettings reads one user's model configuration. It is a separate read
// because the identity returned to the client never carries a secret.
func (s *UserStore) ModelSettings(ctx context.Context, userID int64) (ModelSettings, error) {
	settings := ModelSettings{}
	err := s.read.QueryRowContext(ctx, `
		SELECT COALESCE(ModelProvider, ''), COALESCE(ModelName, ''),
		       COALESCE(ModelEndpoint, ''), COALESCE(ModelApiKey, '')
		FROM User WHERE UserId = ?`, userID).
		Scan(&settings.Provider, &settings.Model, &settings.Endpoint, &settings.APIKey)
	if err != nil {
		return ModelSettings{}, fmt.Errorf("read model settings: %w", err)
	}
	return settings, nil
}

// SaveModelSettings writes the model a user configured on the setup page. The key
// arrives sealed: the store never sees a plaintext secret, the same way it never
// returns one. An empty key leaves the stored one alone, so changing the model does
// not mean re-entering the secret.
func (s *UserStore) SaveModelSettings(ctx context.Context, userID int64, settings ModelSettings) error {
	result, err := s.write.ExecContext(ctx, `
		UPDATE User SET ModelProvider = ?, ModelName = ?, ModelEndpoint = ?, ModelApiKey = ?
		WHERE UserId = ?`,
		settings.Provider, settings.Model, settings.Endpoint, settings.APIKey, userID)
	if err != nil {
		return fmt.Errorf("save model settings: %w", err)
	}
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
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
