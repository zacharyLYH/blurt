package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SessionStore owns the Session table. It only deals in user ids; callers
// combine it with UserStore when they need the identity. Probes read from the
// read pool; anything that mutates a session takes the writer.
type SessionStore struct {
	write *sql.DB
	read  *sql.DB
}

func NewSessionStore(write, read *sql.DB) *SessionStore {
	return &SessionStore{write: write, read: read}
}

// Create stores the hash of an opaque session token.
func (s *SessionStore) Create(ctx context.Context, userID int64, tokenHash []byte, expires time.Time) error {
	issued := now()
	_, err := s.write.ExecContext(ctx, `
		INSERT INTO Session (UserId, TokenHash, ExpiresAt, CreatedAt, LastSeenAt)
		VALUES (?, ?, ?, ?, ?)`,
		userID, tokenHash, timestamp(expires), issued, issued)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// Lookup maps a cookie hash to a user without changing the row. Use it for
// read-only probes that cannot refresh the browser cookie; otherwise the store
// row would outlive the cookie it was resolved from.
func (s *SessionStore) Lookup(ctx context.Context, tokenHash []byte, nowTime time.Time) (int64, error) {
	var userID int64
	err := s.read.QueryRowContext(ctx, `
		SELECT UserId FROM Session
		WHERE TokenHash = ? AND ExpiresAt > ?`,
		tokenHash, timestamp(nowTime)).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("lookup session: %w", err)
	}
	return userID, nil
}

// Resolve maps a cookie hash to a user and slides the expiry forward. Call it
// only from a path that can also refresh the browser cookie; use Lookup when
// the caller cannot.
func (s *SessionStore) Resolve(ctx context.Context, tokenHash []byte, nowTime, renewedUntil time.Time) (int64, error) {
	var userID int64
	err := s.write.QueryRowContext(ctx, `
		UPDATE Session SET LastSeenAt = ?, ExpiresAt = ?
		WHERE TokenHash = ? AND ExpiresAt > ?
		RETURNING UserId`,
		timestamp(nowTime), timestamp(renewedUntil),
		tokenHash, timestamp(nowTime)).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("resolve session: %w", err)
	}
	return userID, nil
}

// SetPushSubscription records the browser subscription for one device. A
// re-subscribe overwrites the previous endpoint, which is the usual rotation
// path.
func (s *SessionStore) SetPushSubscription(ctx context.Context, tokenHash, subscription []byte) error {
	_, err := s.write.ExecContext(ctx, `
		UPDATE Session SET PushSubscription = ?
		WHERE TokenHash = ?`,
		string(subscription), tokenHash)
	if err != nil {
		return fmt.Errorf("set push subscription: %w", err)
	}
	return nil
}

// PushDevice is one device's stored subscription. Only the session id and the
// subscription leave the store, so the sender never sees an identity.
type PushDevice struct {
	SessionID    int64
	Subscription []byte
}

// ListPushSubscriptions returns every live device of this user that has a
// subscription. Expired sessions are skipped: their cookie is already gone,
// so a push into them would just be noise to clean up later.
func (s *SessionStore) ListPushSubscriptions(ctx context.Context, userID int64, nowTime time.Time) ([]PushDevice, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT SessionId, PushSubscription FROM Session
		WHERE UserId = ? AND PushSubscription IS NOT NULL AND ExpiresAt > ?
		ORDER BY SessionId`,
		userID, timestamp(nowTime))
	if err != nil {
		return nil, fmt.Errorf("list push subscriptions: %w", err)
	}
	defer rows.Close()

	var devices []PushDevice
	for rows.Next() {
		var device PushDevice
		if err := rows.Scan(&device.SessionID, &device.Subscription); err != nil {
			return nil, fmt.Errorf("scan push subscription: %w", err)
		}
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list push subscriptions: %w", err)
	}
	return devices, nil
}

// ClearPushSubscription forgets one device's endpoint but keeps the session:
// the cookie is still valid, only push is off until the browser re-subscribes.
func (s *SessionStore) ClearPushSubscription(ctx context.Context, sessionID int64) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE Session SET PushSubscription = NULL WHERE SessionId = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("clear push subscription: %w", err)
	}
	return nil
}

// Delete revokes one device.
func (s *SessionStore) Delete(ctx context.Context, tokenHash []byte) error {
	_, err := s.write.ExecContext(ctx, `DELETE FROM Session WHERE TokenHash = ?`, tokenHash)
	return err
}
