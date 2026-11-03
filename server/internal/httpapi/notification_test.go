package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"blurt/internal/db"
)

// seedReminder writes one Reminder as the emitter would have left it, so a handler
// test is about the handler rather than about the emitter.
func seedReminder(t *testing.T, handle *db.DB, userID int64, text, instant, status string) int64 {
	t.Helper()
	var reminderID int64
	if err := handle.Write.QueryRow(`
		INSERT INTO Reminder (UserId, Text, Timezone, LocalDateTime, EmitAtUtc, Status, CreatedAt)
		VALUES (?, ?, 'Asia/Singapore', '2026-11-20T18:00', ?, ?, ?)
		RETURNING ReminderId`,
		userID, text, instant, status, instant).Scan(&reminderID); err != nil {
		t.Fatalf("seed reminder: %v", err)
	}
	return reminderID
}

func userIDByEmail(t *testing.T, handle *db.DB, email string) int64 {
	t.Helper()
	var id int64
	if err := handle.Read.QueryRow(`SELECT UserId FROM User WHERE Email = ?`, email).Scan(&id); err != nil {
		t.Fatalf("read user id: %v", err)
	}
	return id
}

func notificationURL(base string, reminderID int64) string {
	return base + "/api/notifications/" + strconv.FormatInt(reminderID, 10)
}

func TestNotificationDetailReadsTheRemindersWords(t *testing.T) {
	client, base, handle := newClient(t)
	login(t, client, base, "lam@example.com")
	userID := userIDByEmail(t, handle, "lam@example.com")
	reminderID := seedReminder(t, handle, userID, "Buy glue", "2026-11-20T10:00:00Z", "fired")

	status, body := call(t, client, http.MethodGet, notificationURL(base, reminderID), nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var reply struct {
		ReminderID int64  `json:"reminderId"`
		Text       string `json:"text"`
		At         string `json:"at"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reply.ReminderID != reminderID || reply.Text != "Buy glue" || reply.At != "2026-11-20T10:00:00Z" {
		t.Fatalf("detail = %+v, want the reminder's own words and instant", reply)
	}

	// A guessed id, including somebody else's real one, is a 404 with no body to read.
	newUserClient(t, base, "other@example.com")
	otherID := userIDByEmail(t, handle, "other@example.com")
	foreignID := seedReminder(t, handle, otherID, "Their glue", "2026-11-20T11:00:00Z", "fired")
	for name, id := range map[string]int64{
		"missing": 9_999_999,
		"foreign": foreignID,
	} {
		status, body := call(t, client, http.MethodGet, notificationURL(base, id), nil)
		if status != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404: %s", name, status, body)
		}
	}

	// Calling the reminder off closes its page too: there is nothing to report once
	// the interruption has been withdrawn.
	if _, err := handle.Write.Exec(`UPDATE Reminder SET Status = 'cancelled' WHERE UserId = ?`, userID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if status, body := call(t, client, http.MethodGet, notificationURL(base, reminderID), nil); status != http.StatusNotFound {
		t.Fatalf("cancelled status = %d, want 404: %s", status, body)
	}
}

func TestNotificationFeedbackIsANewRowPerSubmission(t *testing.T) {
	client, base, handle := newClient(t)
	login(t, client, base, "lam@example.com")
	userID := userIDByEmail(t, handle, "lam@example.com")
	reminderID := seedReminder(t, handle, userID, "Take pills", "2026-11-20T10:00:00Z", "resolved")
	path := notificationURL(base, reminderID) + "/feedback"

	// Only the closed set the form offers is accepted, and it is required.
	for name, payload := range map[string]any{
		"missing mode": map[string]any{"comment": "hm"},
		"unknown mode": map[string]any{"failureMode": "it was bad"},
	} {
		if status, body := call(t, client, http.MethodPost, path, payload); status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400: %s", name, status, body)
		}
	}

	// A recurring Reminder reports each firing: two submissions are two rows with the
	// same ReminderId and their own ids, because the feedback row is the instance.
	ids := map[int64]bool{}
	for _, mode := range []string{"wrong_date_time_zone", "duplicate_too_many"} {
		status, body := call(t, client, http.MethodPost, path, map[string]any{
			"failureMode": mode, "comment": "same reminder, next firing",
		})
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", status, body)
		}
		var reply struct {
			ID int64 `json:"notificationFeedbackId"`
		}
		if err := json.Unmarshal(body, &reply); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if reply.ID == 0 || ids[reply.ID] {
			t.Fatalf("feedback id = %d, want a fresh id each submission", reply.ID)
		}
		ids[reply.ID] = true
	}

	var rows int
	var mode, comment, createdAt string
	if err := handle.Read.QueryRow(`
		SELECT count(*), MIN(FailureMode), MIN(COALESCE(Comment, '')), MIN(CreatedAt)
		FROM NotificationFeedback WHERE ReminderId = ?`, reminderID).
		Scan(&rows, &mode, &comment, &createdAt); err != nil {
		t.Fatalf("read feedback: %v", err)
	}
	if rows != 2 || mode == "" || comment == "" || createdAt == "" {
		t.Fatalf("feedback = %d rows %q %q %q, want one row per submission", rows, mode, comment, createdAt)
	}

	// Missing and foreign reminders have no form.
	if status, _ := call(t, client, http.MethodPost, notificationURL(base, 9999999)+"/feedback",
		map[string]any{"failureMode": "other"}); status != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", status)
	}
}

func TestNotificationsForMonthAreScopedToUserAndMonth(t *testing.T) {
	client, base, handle := newClient(t)
	login(t, client, base, "lam@example.com")
	userID := userIDByEmail(t, handle, "lam@example.com")

	// An 18:00 Singapore reminder is 10:00Z: inside November in the user's zone.
	inMonth := seedReminder(t, handle, userID, "Buy glue", "2026-11-20T10:00:00Z", "fired")
	seedReminder(t, handle, userID, "December thing", "2026-12-02T10:00:00Z", "resolved")
	newUserClient(t, base, "other@example.com")
	otherID := userIDByEmail(t, handle, "other@example.com")
	seedReminder(t, handle, otherID, "Their glue", "2026-11-20T11:00:00Z", "fired")

	status, body := call(t, client, http.MethodGet,
		base+"/api/notifications?month=2026-11&tz=Asia/Singapore", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var reply struct {
		Month   string `json:"month"`
		Entries []struct {
			ReminderID int64  `json:"reminderId"`
			Text       string `json:"text"`
			At         string `json:"at"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reply.Month != "2026-11" || len(reply.Entries) != 1 {
		t.Fatalf("month = %q entries = %+v, want only the caller's November reminder", reply.Month, reply.Entries)
	}
	if entry := reply.Entries[0]; entry.Text != "Buy glue" || entry.ReminderID != inMonth || entry.At == "" {
		t.Fatalf("entry = %+v, want the caller's reminder and its instant", entry)
	}

	for name, query := range map[string]string{
		"bad month":    "?month=November&tz=UTC",
		"bad timezone": "?month=2026-11&tz=Mars/Olympus",
	} {
		if status, body := call(t, client, http.MethodGet, base+"/api/notifications"+query, nil); status != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400: %s", name, status, body)
		}
	}
}

// Trace is diagnostics: the product's own paths must keep working when the table is
// gone, and the detail's words must come from the Reminder, never a Trace Detail.
func TestNotificationPathsWorkWithTraceDropped(t *testing.T) {
	client, base, handle := newClient(t)
	login(t, client, base, "lam@example.com")
	userID := userIDByEmail(t, handle, "lam@example.com")
	reminderID := seedReminder(t, handle, userID, "Buy glue", "2026-11-20T10:00:00Z", "fired")

	if _, err := handle.Write.Exec(`DROP TABLE Trace`); err != nil {
		t.Fatalf("drop trace: %v", err)
	}

	if status, body := call(t, client, http.MethodGet, notificationURL(base, reminderID), nil); status != http.StatusOK {
		t.Fatalf("detail without Trace: status = %d: %s", status, body)
	}
	if status, body := call(t, client, http.MethodPost, notificationURL(base, reminderID)+"/feedback",
		map[string]any{"failureMode": "wrong_reminder"}); status != http.StatusCreated {
		t.Fatalf("feedback without Trace: status = %d: %s", status, body)
	}
	if status, body := call(t, client, http.MethodGet,
		base+"/api/notifications?month=2026-11&tz=Asia/Singapore", nil); status != http.StatusOK {
		t.Fatalf("calendar without Trace: status = %d: %s", status, body)
	}
}
