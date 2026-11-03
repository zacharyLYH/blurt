package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"blurt/internal/store"
)

const (
	cookieName = "blurt_session"
	sessionTTL = 30 * 24 * time.Hour
)

type userEnvelope struct {
	User store.User `json:"user"`
}

// requestPin mints a PIN and mails it. With no mailer the stack is in dev mode
// (BLURT_FAKE_PIN): the PIN is fixed, nothing is sent, and any address can sign
// in. With a mailer, only a seeded User row can receive a PIN — answering 200
// either way keeps the endpoint from revealing the guest list.
func (s *Server) requestPin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	address, ok := parseEmail(body.Email)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_email", "That email address does not look right.")
		return
	}
	// Request a PIN only without a valid session.
	if _, err := s.sessionUser(r); err == nil {
		writeError(w, http.StatusConflict, "already_signed_in", "You are already signed in.")
		return
	}
	// Only an approved account can be mailed a PIN, in dev mode too: the app is
	// apply-then-approve, so an address with no active row has nothing to sign
	// into yet. Answering 200 either way keeps the guest list private.
	user, err := s.Users.ByEmail(r.Context(), address)
	if errors.Is(err, store.ErrNotFound) || (err == nil && user.Status != store.StatusActive) {
		writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
		return
	}
	if err != nil {
		s.fail(w, "look up user", err)
		return
	}

	pin, err := s.Pins.Issue(address)
	if err != nil {
		s.fail(w, "issue pin", err)
		return
	}
	if s.Mailer == nil {
		// Dev mode (BLURT_FAKE_PIN): the PIN is fixed and never mailed.
		writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
		return
	}
	if err := s.Mailer.SendPIN(address, pin); err != nil {
		s.Pins.Discard(address)
		s.fail(w, "send pin mail", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

// verifyPin checks the emailed PIN, resolves the user, and mints the session.
// Signing in never creates an account: applying is its own step, and the owner
// is the one who says yes.
func (s *Server) verifyPin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
		Pin   string `json:"pin"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	address, ok := parseEmail(body.Email)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_email", "That email address does not look right.")
		return
	}
	if strings.TrimSpace(body.Pin) == "" {
		writeError(w, http.StatusBadRequest, "missing_pin", "Enter the PIN from your email.")
		return
	}
	if !s.Pins.Verify(address, strings.TrimSpace(body.Pin)) {
		writeError(w, http.StatusUnauthorized, "invalid_pin", "That PIN is wrong or expired.")
		return
	}

	user, err := s.Users.ByEmail(r.Context(), address)
	if errors.Is(err, store.ErrNotFound) {
		// The row went away between the PIN and this call — a rejection, or a
		// reset volume. Same answer as a wrong code, so nothing leaks.
		writeError(w, http.StatusUnauthorized, "invalid_pin", "That PIN is wrong or expired.")
		return
	}
	if err != nil {
		s.fail(w, "load user", err)
		return
	}
	if user.Status != store.StatusActive {
		writeError(w, http.StatusForbidden, "not_approved", "Your application is still being reviewed.")
		return
	}

	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		s.fail(w, "mint session", err)
		return
	}
	expires := time.Now().Add(sessionTTL)
	if err := s.Sessions.Create(r.Context(), user.UserID, hashToken(token), expires); err != nil {
		s.fail(w, "create session", err)
		return
	}
	setSessionCookie(w, r, token, expires)
	writeJSON(w, http.StatusOK, userEnvelope{User: s.view(user)})
}

// apply records an application: a pending row the owner can see and answer.
// The reply is the same whether or not the address was already known, so the
// form cannot be used to find out who has an account.
func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	address, ok := parseEmail(body.Email)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_email", "That email address does not look right.")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing_name", "Tell us what to call you.")
		return
	}
	if len([]rune(name)) > 80 {
		writeError(w, http.StatusBadRequest, "invalid_name", "That name is a little too long.")
		return
	}
	if _, err := s.Users.Apply(r.Context(), name, address); err != nil {
		s.fail(w, "record application", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": store.StatusPending})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if raw, ok := sessionToken(r); ok {
		if err := s.Sessions.Delete(r.Context(), hashToken(raw)); err != nil {
			s.fail(w, "delete session", err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: usesHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// requireUser resolves the session cookie and hands the handler a real user.
// Renewal lives here because this is the only path that owns the response and
// can therefore refresh both the store row and the browser cookie together.
func (s *Server) requireUser(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := sessionToken(r)
		if !ok {
			unauthorized(w)
			return
		}
		now := time.Now()
		renewedUntil := now.Add(sessionTTL)
		userID, err := s.Sessions.Resolve(r.Context(), hashToken(raw), now, renewedUntil)
		if errors.Is(err, store.ErrNotFound) {
			unauthorized(w)
			return
		}
		if err != nil {
			s.fail(w, "resolve session", err)
			return
		}
		user, err := s.Users.ByID(r.Context(), userID)
		if errors.Is(err, store.ErrNotFound) {
			unauthorized(w)
			return
		}
		if err != nil {
			s.fail(w, "load user", err)
			return
		}
		// Sessions slide on use: the store row and the browser copy get the
		// same expiry, so an active user is never signed out mid-use.
		setSessionCookie(w, r, raw, renewedUntil)
		next(w, r, user)
	}
}

// requireOwner is requireUser plus the one address BLURT_SMTP_USERNAME names.
// There is no role column to drift from it: the owner is deployment config.
func (s *Server) requireOwner(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request, user store.User) {
		if !s.isOwner(user) {
			writeError(w, http.StatusForbidden, "not_owner", "Only the owner can manage accounts.")
			return
		}
		next(w, r, user)
	})
}

// isOwner compares against the configured address, case-insensitively because
// email is NOCASE in the database too.
func (s *Server) isOwner(user store.User) bool {
	return s.Owner != "" && strings.EqualFold(user.Email, s.Owner)
}

// view fills the computed Owner flag every user response carries, so the client
// never has to know which address the deployment names.
func (s *Server) view(user store.User) store.User {
	user.Owner = s.isOwner(user)
	return user
}

// sessionUser resolves the cookie without touching the session row. Probes
// such as request-pin's "already signed in" check use this: they cannot
// refresh the browser cookie, so they must not slide the store row either.
func (s *Server) sessionUser(r *http.Request) (store.User, error) {
	raw, ok := sessionToken(r)
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	userID, err := s.Sessions.Lookup(r.Context(), hashToken(raw), time.Now())
	if err != nil {
		return store.User{}, err
	}
	return s.Users.ByID(r.Context(), userID)
}

// sessionToken decodes the opaque cookie value. Keeping the encoding here
// means resolve, probe, and logout all hash exactly the same bytes.
func sessionToken(r *http.Request) ([]byte, bool) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func unauthorized(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, "unauthorized", "Session expired. Sign in again.")
}

func parseEmail(value string) (string, bool) {
	address, err := mail.ParseAddress(strings.TrimSpace(value))
	if err != nil {
		return "", false
	}
	return strings.ToLower(address.Address), true
}

func hashToken(token []byte) []byte {
	sum := sha256.Sum256(token)
	return sum[:]
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token []byte, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    base64.RawURLEncoding.EncodeToString(token),
		Path:     "/",
		MaxAge:   int(time.Until(expires).Seconds()),
		Expires:  expires,
		HttpOnly: true,
		Secure:   usesHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// usesHTTPS trusts nginx's forwarded scheme; locally that is plain http.
func usesHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
