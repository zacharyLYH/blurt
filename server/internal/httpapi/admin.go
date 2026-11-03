package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"blurt/internal/store"
)

// listApplicants is the owner's queue: everyone waiting for a decision first,
// then everyone already in. It is the whole table on purpose — the owner is
// deciding who exists, so hiding rows would hide the decision.
func (s *Server) listApplicants(w http.ResponseWriter, r *http.Request, _ store.User) {
	users, err := s.Users.List(r.Context())
	if err != nil {
		s.fail(w, "list applicants", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// approveApplicant is the owner's yes: the row becomes active, and the
// applicant is mailed the way in. The mail is a courtesy — the account works
// either way, so a failed send is logged rather than rolled back.
func (s *Server) approveApplicant(w http.ResponseWriter, r *http.Request, _ store.User) {
	user, ok := s.pendingApplicant(w, r)
	if !ok {
		return
	}
	user, err := s.Users.Approve(r.Context(), user.UserID)
	if !s.decided(w, err, "approve") {
		return
	}
	if s.Mailer != nil {
		s.verdictMail("approval", user, s.Mailer.SendApproved)
	}
	writeJSON(w, http.StatusOK, userEnvelope{User: s.view(user)})
}

// rejectApplicant is the owner's no. The row is deleted, not marked, so the
// address is free to apply again and nothing about the decision lingers.
func (s *Server) rejectApplicant(w http.ResponseWriter, r *http.Request, _ store.User) {
	user, ok := s.pendingApplicant(w, r)
	if !ok {
		return
	}
	user, err := s.Users.Reject(r.Context(), user.UserID)
	if !s.decided(w, err, "reject") {
		return
	}
	if s.Mailer != nil {
		s.verdictMail("rejection", user, s.Mailer.SendRejected)
	}
	w.WriteHeader(http.StatusNoContent)
}

// decided turns the store's answer into a response: a row that went away is a
// 404, anything else is a failure. Both verdicts read the same answer.
func (s *Server) decided(w http.ResponseWriter, err error, verb string) bool {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "That application is no longer waiting.")
		return false
	}
	if err != nil {
		s.fail(w, verb+" applicant", err)
		return false
	}
	return true
}

// verdictMail sends the decision as a courtesy: the account works either way,
// so a failed send is logged, never rolled back.
func (s *Server) verdictMail(verb string, user store.User, send func(string, string) error) {
	if err := send(user.Email, user.Name); err != nil {
		slog.Error(verb+" mail", "err", err, "user_id", user.UserID)
	}
}

// pendingApplicant resolves the path id and refuses a row that is not waiting
// for a decision, so the two verdicts cannot be applied to the wrong account.
func (s *Server) pendingApplicant(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	userID, err := strconv.ParseInt(r.PathValue("userId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_user", "That is not a user id.")
		return store.User{}, false
	}
	user, err := s.Users.ByID(r.Context(), userID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "No such account.")
		return store.User{}, false
	}
	if err != nil {
		s.fail(w, "load applicant", err)
		return store.User{}, false
	}
	if user.Status != store.StatusPending {
		writeError(w, http.StatusConflict, "already_decided", "That account is already decided.")
		return store.User{}, false
	}
	return user, true
}
