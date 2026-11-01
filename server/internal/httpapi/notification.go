package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"blurt/internal/store"
)

// maxFeedbackComment bounds the optional comment, so one report stays one short
// answer rather than an essay the page has to render.
const maxFeedbackComment = 1000

// getNotification backs the page a push opens: the Reminder's own words, when it was
// set, and the feedback already given, if any. Ownership is the Reminder -> User
// join, so a guessed id is a 404 rather than a way to read somebody else's reminder.
func (s *Server) getNotification(w http.ResponseWriter, r *http.Request, user store.User) {
	if unavailable(w, s.Notifications == nil, "Notifications are not configured.") {
		return
	}
	reminderID, ok := pathID(r, "reminderId")
	if !ok {
		notificationNotFound(w)
		return
	}
	notification, err := s.Notifications.Detail(r.Context(), user.UserID, reminderID)
	if errors.Is(err, store.ErrNotFound) {
		notificationNotFound(w)
		return
	}
	if err != nil {
		s.fail(w, "load notification", err)
		return
	}
	writeJSON(w, http.StatusOK, notification)
}

// submitNotificationFeedback records one report about one Reminder. The enum is
// checked here so a bad slug is a 400, and a missing, foreign or called-off reminder
// is a 404 with no form behind it. Every submission is a new row: a repeat is
// reported each time it fires, so the same ReminderId is expected many times.
func (s *Server) submitNotificationFeedback(w http.ResponseWriter, r *http.Request, user store.User) {
	if unavailable(w, s.Notifications == nil, "Notifications are not configured.") {
		return
	}
	reminderID, ok := pathID(r, "reminderId")
	if !ok {
		notificationNotFound(w)
		return
	}
	var body struct {
		FailureMode string `json:"failureMode"`
		Comment     string `json:"comment"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	mode := strings.TrimSpace(body.FailureMode)
	if !store.ValidFailureMode(mode) {
		writeError(w, http.StatusBadRequest, "invalid_failure_mode", "Choose what went wrong.")
		return
	}
	comment := strings.TrimSpace(body.Comment)
	if len(comment) > maxFeedbackComment {
		writeError(w, http.StatusBadRequest, "comment_too_long", "That comment is too long.")
		return
	}

	id, err := s.Notifications.AddFeedback(r.Context(), user.UserID, reminderID, mode, comment)
	if errors.Is(err, store.ErrNotFound) {
		notificationNotFound(w)
		return
	}
	if err != nil {
		s.fail(w, "record feedback", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"notificationFeedbackId": id})
}

// listNotificationsForMonth is the calendar's read: the caller's Reminders whose
// instant falls in the month they are viewing, in the zone they are viewing it from.
// Each entry opens its Reminder, because the notification is the Reminder.
func (s *Server) listNotificationsForMonth(w http.ResponseWriter, r *http.Request, user store.User) {
	if unavailable(w, s.Notifications == nil, "Notifications are not configured.") {
		return
	}
	zone := strings.TrimSpace(r.URL.Query().Get("tz"))
	if zone == "" {
		zone = "UTC"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_timezone", "Timezone is not a known IANA zone.")
		return
	}
	month := strings.TrimSpace(r.URL.Query().Get("month"))
	if month == "" {
		month = time.Now().In(loc).Format("2006-01")
	}
	from, err := time.ParseInLocation("2006-01", month, loc)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_month", "Month must be YYYY-MM.")
		return
	}
	entries, err := s.Notifications.Month(r.Context(), user.UserID, from, from.AddDate(0, 1, 0))
	if err != nil {
		s.fail(w, "list notifications", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"month": month, "timezone": zone, "entries": entries,
	})
}

// notificationNotFound is the one answer for a missing, foreign or called-off
// reminder: the page has nothing to show and no form to collect.
func notificationNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "notification_not_found", "Notification not found.")
}

// pathID reads one positive integer path value. A malformed id is not a resource,
// so callers answer it as not found rather than as a validation error.
func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id, err == nil && id > 0
}
