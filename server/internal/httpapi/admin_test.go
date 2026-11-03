package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"blurt/internal/auth"
	"blurt/internal/store"
)

// applicantList is what the owner's portal reads.
type applicantList struct {
	Users []store.User `json:"users"`
}

// apply is the public half: anyone can ask for an account.
func apply(t *testing.T, client *http.Client, base, name, email string) {
	t.Helper()
	if status, body := call(t, client, http.MethodPost, base+"/api/auth/apply", map[string]string{
		"name": name, "email": email,
	}); status != http.StatusOK {
		t.Fatalf("apply status = %d: %s", status, body)
	}
}

// ownerClient signs in the one account the stack names as owner.
func ownerClient(t *testing.T, base string) *http.Client {
	t.Helper()
	return newUserClient(t, base, testOwner)
}

// idOf finds an applicant in the owner's list.
func idOf(t *testing.T, users []store.User, email string) int64 {
	t.Helper()
	for _, user := range users {
		if user.Email == email {
			return user.UserID
		}
	}
	t.Fatalf("%s is not in the owner's list: %+v", email, users)
	return 0
}

// applicants reads the owner's queue.
func applicants(t *testing.T, owner *http.Client, base string) []store.User {
	t.Helper()
	status, body := call(t, owner, http.MethodGet, base+"/api/admin/users", nil)
	if status != http.StatusOK {
		t.Fatalf("owner list status = %d: %s", status, body)
	}
	var list applicantList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode applicants: %v", err)
	}
	return list.Users
}

func decide(t *testing.T, owner *http.Client, base, action string, userID int64) (int, []byte) {
	t.Helper()
	return call(t, owner, http.MethodPost, base+"/api/admin/users/"+strconv.FormatInt(userID, 10)+"/"+action, nil)
}

// The whole door, end to end: someone applies, the owner sees them waiting,
// approves, and only then can they sign in.
func TestOwnerApprovesAnApplication(t *testing.T) {
	mailer := &fakeMailer{}
	client, base, _, _ := newTestStack(t, auth.NewPinStore(time.Minute, auth.DevPIN), mailer, nil)

	// Applying is public and creates a row that cannot sign in yet.
	apply(t, client, base, "Sam", "sam@example.com")
	if status, body := call(t, client, http.MethodPost, base+"/api/auth/request-pin", map[string]string{
		"email": "sam@example.com",
	}); status != http.StatusOK {
		t.Fatalf("request-pin status = %d, want the silent 200: %s", status, body)
	}
	if mailer.count() != 0 {
		t.Fatalf("PIN emails = %d, want 0 while the application waits", mailer.count())
	}
	if status, body := call(t, client, http.MethodPost, base+"/api/auth/verify-pin", map[string]string{
		"email": "sam@example.com", "pin": auth.DevPIN,
	}); status != http.StatusUnauthorized {
		t.Fatalf("pending verify-pin status = %d, want 401: %s", status, body)
	}

	// A signed-in non-owner is refused: the portal is the owner's alone.
	other := newUserClient(t, base, "other@example.com")
	if status, body := call(t, other, http.MethodGet, base+"/api/admin/users", nil); status != http.StatusForbidden {
		t.Fatalf("non-owner list status = %d, want 403: %s", status, body)
	}

	// The owner sees the application waiting, and approves it.
	owner := ownerClient(t, base)
	samID := idOf(t, applicants(t, owner, base), "sam@example.com")
	if status, body := decide(t, owner, base, "approve", samID); status != http.StatusOK {
		t.Fatalf("approve status = %d: %s", status, body)
	}
	if len(mailer.approved) != 1 || mailer.approved[0] != "sam@example.com" {
		t.Fatalf("approval emails = %v, want one to sam@example.com", mailer.approved)
	}
	// Deciding twice is refused, not repeated.
	if status, body := decide(t, owner, base, "approve", samID); status != http.StatusConflict {
		t.Fatalf("second approve status = %d, want 409: %s", status, body)
	}

	// Now — and only now — the applicant can sign in.
	approved := newUserClient(t, base, "sam@example.com")
	if status, body := call(t, approved, http.MethodGet, base+"/api/user", nil); status != http.StatusOK {
		t.Fatalf("GET /api/user status = %d: %s", status, body)
	}
}

// A rejection leaves nothing behind: the row is deleted, the address is mailed,
// and the application can be made again later.
func TestOwnerRejectsAnApplication(t *testing.T) {
	mailer := &fakeMailer{}
	client, base, handle, _ := newTestStack(t, auth.NewPinStore(time.Minute, auth.DevPIN), mailer, nil)

	apply(t, client, base, "Sam", "sam@example.com")
	owner := ownerClient(t, base)
	samID := idOf(t, applicants(t, owner, base), "sam@example.com")

	if status, body := decide(t, owner, base, "reject", samID); status != http.StatusNoContent {
		t.Fatalf("reject status = %d: %s", status, body)
	}
	if len(mailer.rejected) != 1 || mailer.rejected[0] != "sam@example.com" {
		t.Fatalf("rejection emails = %v, want one to sam@example.com", mailer.rejected)
	}

	var rows int
	if err := handle.Read.QueryRow(`SELECT count(*) FROM User WHERE Email = ?`, "sam@example.com").Scan(&rows); err != nil {
		t.Fatalf("count sam: %v", err)
	}
	if rows != 0 {
		t.Fatalf("sam rows = %d, want the application deleted", rows)
	}

	// Applying again is allowed and starts a fresh review.
	apply(t, client, base, "Sam", "sam@example.com")
	idOf(t, applicants(t, owner, base), "sam@example.com")
}

// A stack with no mailer is dev mode: both verdicts still work, and neither
// touches a mailer that is not there.
func TestVerdictsWithoutAMailer(t *testing.T) {
	client, base, _, _ := newTestStack(t, auth.NewPinStore(time.Minute, auth.DevPIN), nil, nil)
	owner := ownerClient(t, base)

	apply(t, client, base, "Sam", "sam@example.com")
	apply(t, client, base, "Ada", "ada@example.com")
	users := applicants(t, owner, base)

	if status, body := decide(t, owner, base, "approve", idOf(t, users, "sam@example.com")); status != http.StatusOK {
		t.Fatalf("approve status = %d, want 200 without a mailer: %s", status, body)
	}
	if status, body := decide(t, owner, base, "reject", idOf(t, users, "ada@example.com")); status != http.StatusNoContent {
		t.Fatalf("reject status = %d, want 204 without a mailer: %s", status, body)
	}
}

// Applying twice for the same address does not queue a second review.
func TestApplyIsIdempotent(t *testing.T) {
	client, base, handle, _ := newTestStack(t, auth.NewPinStore(time.Minute, auth.DevPIN), nil, nil)

	apply(t, client, base, "Sam", "sam@example.com")
	apply(t, client, base, "Sammy", "sam@example.com")

	var rows int
	if err := handle.Read.QueryRow(`SELECT count(*) FROM User WHERE Email = ?`, "sam@example.com").Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want one", rows)
	}
	var status string
	if err := handle.Read.QueryRow(`SELECT Status FROM User WHERE Email = ?`, "sam@example.com").Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != store.StatusPending {
		t.Fatalf("status = %q, want pending", status)
	}
}

// The portal is the owner's alone, and every door into it says so: a stranger
// is refused before the account is looked at, and a signed-in non-owner is
// refused by name. Neither verdict may touch the row it was aimed at.
func TestAdminIsOwnerOnly(t *testing.T) {
	client, base, handle, _ := newTestStack(t, auth.NewPinStore(time.Minute, auth.DevPIN), nil, nil)
	apply(t, client, base, "Sam", "sam@example.com")
	owner := ownerClient(t, base)
	samID := idOf(t, applicants(t, owner, base), "sam@example.com")
	path := "/api/admin/users/" + strconv.FormatInt(samID, 10)

	// Not signed in at all: the session gate answers first.
	stranger := &http.Client{}
	for _, probe := range []struct{ method, url string }{
		{http.MethodGet, base + "/api/admin/users"},
		{http.MethodPost, base + path + "/approve"},
		{http.MethodPost, base + path + "/reject"},
	} {
		if status, body := call(t, stranger, probe.method, probe.url, nil); status != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s status = %d, want 401: %s", probe.method, probe.url, status, body)
		}
	}

	// Signed in, but not the owner.
	other := newUserClient(t, base, "other@example.com")
	for _, probe := range []struct{ method, url string }{
		{http.MethodGet, base + "/api/admin/users"},
		{http.MethodPost, base + path + "/approve"},
		{http.MethodPost, base + path + "/reject"},
	} {
		if status, body := call(t, other, probe.method, probe.url, nil); status != http.StatusForbidden {
			t.Fatalf("non-owner %s %s status = %d, want 403: %s", probe.method, probe.url, status, body)
		}
	}

	// The refusals left the application exactly as it was.
	var status string
	if err := handle.Read.QueryRow(`SELECT Status FROM User WHERE UserID = ?`, samID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != store.StatusPending {
		t.Fatalf("sam status = %q, want pending after the refused decisions", status)
	}
}
