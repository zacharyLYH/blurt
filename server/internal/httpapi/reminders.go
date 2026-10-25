package httpapi

import (
	"net/http"
	"strconv"

	"blurt/internal/store"
)

// listReminders backs the Reminders page: what is still scheduled, soonest first, and
// what already happened, newest first. Both are scoped to the caller, and Status is
// what splits them, so a fired one-off and a called-off Reminder land under Past
// rather than disappearing.
func (s *Server) listReminders(w http.ResponseWriter, r *http.Request, user store.User) {
	if s.Reminders == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Reminders are not configured.")
		return
	}
	upcoming, err := s.Reminders.Upcoming(r.Context(), user.UserID)
	if err != nil {
		s.fail(w, "list upcoming reminders", err)
		return
	}
	past, err := s.Reminders.Past(r.Context(), user.UserID)
	if err != nil {
		s.fail(w, "list past reminders", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"upcoming": upcoming, "past": past})
}

// cancelReminder calls one of the caller's Reminders off. A Reminder that already
// fired, or one that belongs to somebody else, is not found: the status is all this
// changes, so its history stays readable under Past.
func (s *Server) cancelReminder(w http.ResponseWriter, r *http.Request, user store.User) {
	if s.Reminders == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Reminders are not configured.")
		return
	}
	reminderID, err := strconv.ParseInt(r.PathValue("reminderId"), 10, 64)
	if err != nil || reminderID < 1 {
		writeError(w, http.StatusNotFound, "reminder_not_found", "Reminder not found.")
		return
	}
	cancelled, err := s.Reminders.Cancel(r.Context(), user.UserID, reminderID)
	if err != nil {
		s.fail(w, "cancel reminder", err)
		return
	}
	if !cancelled {
		writeError(w, http.StatusNotFound, "reminder_not_found", "Reminder not found.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
