package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"blurt/internal/store"
)

// listTraces backs the debug page's sidebar: one row per reminder that has any
// history, most recent first. It is the caller's own reminders and nothing else.
func (s *Server) listTraces(w http.ResponseWriter, r *http.Request, user store.User) {
	if unavailable(w, s.Traces == nil, "Traces are not configured.") {
		return
	}
	traced, err := s.Traces.Traced(r.Context(), user.UserID)
	if err != nil {
		s.fail(w, "list traces", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reminders": traced})
}

// getTrace is one reminder's history: the capture that made it, every firing, and
// the job each row belongs to. A reminder that is not the caller's is not found,
// which is the same answer as one that never existed.
func (s *Server) getTrace(w http.ResponseWriter, r *http.Request, user store.User) {
	if unavailable(w, s.Traces == nil, "Traces are not configured.") {
		return
	}
	reminderID, err := strconv.ParseInt(r.PathValue("reminderId"), 10, 64)
	if err != nil || reminderID < 1 {
		writeError(w, http.StatusNotFound, "reminder_not_found", "Reminder not found.")
		return
	}
	reminder, traces, err := s.Traces.Traces(r.Context(), user.UserID, reminderID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "reminder_not_found", "Reminder not found.")
		return
	}
	if err != nil {
		s.fail(w, "read trace", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reminder": reminder, "traces": traces})
}
