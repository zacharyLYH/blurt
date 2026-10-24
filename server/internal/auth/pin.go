// Package auth holds login secrets. PINs never touch the database: they
// live in this in-memory map with a TTL, are hashed, single use, and
// attempt capped. A restart only invalidates outstanding PINs.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

const (
	pinDigits   = 6
	maxAttempts = 5
	// DevPIN is the code every login gets while BLURT_FAKE_PIN is on, so a
	// local stack signs in with no mailbox. A public host must never set it.
	DevPIN = "123456"
)

type record struct {
	hash     [32]byte
	expires  time.Time
	attempts int
}

type PinStore struct {
	mu       sync.Mutex
	ttl      time.Duration
	fixedPIN string
	pins     map[string]record
}

// NewPinStore mints random codes. A non-empty fixedPIN is issued for every
// address instead — the BLURT_FAKE_PIN path, which is dev only.
func NewPinStore(ttl time.Duration, fixedPIN string) *PinStore {
	return &PinStore{ttl: ttl, fixedPIN: fixedPIN, pins: map[string]record{}}
}

// Issue mints a fresh PIN for the email, replacing any outstanding one.
func (s *PinStore) Issue(email string) (string, error) {
	code := s.fixedPIN
	if code == "" {
		random, err := randomCode()
		if err != nil {
			return "", err
		}
		code = random
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(time.Now())
	s.pins[key(email)] = record{
		hash:    sha256.Sum256([]byte(code)),
		expires: time.Now().Add(s.ttl),
	}
	return code, nil
}

// Verify consumes the PIN when correct. Expired, used, or over-attempted
// PINs are dropped.
func (s *PinStore) Verify(email, code string) bool {
	nowTime := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.pins[key(email)]
	if !ok {
		return false
	}
	if nowTime.After(stored.expires) {
		delete(s.pins, key(email))
		return false
	}
	if stored.attempts+1 > maxAttempts {
		delete(s.pins, key(email))
		return false
	}

	candidate := sha256.Sum256([]byte(code))
	if subtle.ConstantTimeCompare(stored.hash[:], candidate[:]) != 1 {
		stored.attempts++
		s.pins[key(email)] = stored
		return false
	}
	delete(s.pins, key(email))
	return true
}

// Discard drops an outstanding PIN, e.g. when the mail could not be sent.
func (s *PinStore) Discard(email string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pins, key(email))
}

func (s *PinStore) sweep(nowTime time.Time) {
	for address, stored := range s.pins {
		if nowTime.After(stored.expires) {
			delete(s.pins, address)
		}
	}
}

func key(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func randomCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("generate pin: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
