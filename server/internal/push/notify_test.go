package push

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"blurt/internal/db"
	"blurt/internal/store"
)

// relay answers every push with one status and counts the requests it served,
// so a test asserts both the fan-out's bookkeeping and the traffic it actually
// caused.
func relay(t *testing.T, status int) (endpoint string, calls func() int32) {
	t.Helper()
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/push", count.Load
}

// NotifyUser fans a message out to every session of one user. The test runs a
// real database and real relays, because which rows and which endpoints the
// fan-out touches is the whole point.
func TestNotifyUserFansOutToLiveDevices(t *testing.T) {
	handle, sessions, userID := fanOutStack(t)
	live, liveCalls := relay(t, http.StatusCreated)
	gone, goneCalls := relay(t, http.StatusGone)

	dead := subscribedDevice(t, sessions, userID, gone, time.Hour)
	subscribedDevice(t, sessions, userID, live, time.Hour)
	expired := subscribedDevice(t, sessions, userID, live, -time.Hour)
	otherUser := ensureUser(t, handle, "Someone Else", "other@example.com")
	foreign := subscribedDevice(t, sessions, otherUser, live, time.Hour)

	notifier := &Notifier{Sessions: sessions, Sender: NewSender(testVAPID(t))}
	res, err := notifier.NotifyUser(context.Background(), userID, Message{Title: "Buy glue", Body: "6:00 PM"})
	if err != nil {
		t.Fatalf("NotifyUser: %v", err)
	}
	if res.Sent != 1 || res.Pruned != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v, want one sent and one pruned", res)
	}
	// The traffic is the assertion: the expired session and the other user's
	// device were never contacted.
	if liveCalls() != 1 || goneCalls() != 1 {
		t.Fatalf("relays saw %d live and %d gone requests, want one each", liveCalls(), goneCalls())
	}
	// A dead endpoint loses its subscription, never its session.
	if got := storedSubscription(t, handle, dead); got != "" {
		t.Fatalf("dead endpoint still stored %q", got)
	}
	if count := sessionCount(t, handle, dead); count != 1 {
		t.Fatalf("pruning push removed %d session rows, want 1 kept", count)
	}
	for name, token := range map[string][]byte{"expired session": expired, "other user's device": foreign} {
		if storedSubscription(t, handle, token) == "" {
			t.Fatalf("%s lost its subscription", name)
		}
	}
}

// A relay that refuses our VAPID identity is a deploy problem, so the
// subscription stays and the error reaches the caller.
func TestNotifyUserReportsVAPIDRejection(t *testing.T) {
	handle, sessions, userID := fanOutStack(t)
	refusing, _ := relay(t, http.StatusForbidden)
	device := subscribedDevice(t, sessions, userID, refusing, time.Hour)

	notifier := &Notifier{Sessions: sessions, Sender: NewSender(testVAPID(t))}
	res, err := notifier.NotifyUser(context.Background(), userID, Message{Title: "t", Body: "b"})
	if !errors.Is(err, ErrVAPID) {
		t.Fatalf("err = %v, want ErrVAPID", err)
	}
	if res.Failed != 1 || res.Pruned != 0 || res.Sent != 0 {
		t.Fatalf("result = %+v, want one failure", res)
	}
	if storedSubscription(t, handle, device) == "" {
		t.Fatal("a VAPID rejection pruned the subscription")
	}
}

// A user with no device set up is not an error: push is best effort.
func TestNotifyUserWithoutDevices(t *testing.T) {
	_, sessions, userID := fanOutStack(t)
	notifier := &Notifier{Sessions: sessions, Sender: NewSender(testVAPID(t))}
	res, err := notifier.NotifyUser(context.Background(), userID, Message{Title: "t", Body: "b"})
	if err != nil || res != (Result{}) {
		t.Fatalf("NotifyUser = %+v, %v, want an empty result", res, err)
	}
}

// fanOutStack boots a real database with one user: the fan-out's job is
// choosing among that user's sessions, so the rows have to be real.
func fanOutStack(t *testing.T) (*db.DB, *store.SessionStore, int64) {
	t.Helper()
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	return handle, store.NewSessionStore(handle.Write, handle.Read), ensureUser(t, handle, "Lam", "lam@example.com")
}

func ensureUser(t *testing.T, handle *db.DB, name, email string) int64 {
	t.Helper()
	user, err := store.NewUserStore(handle.Write, handle.Read).EnsureUser(context.Background(), name, email)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	return user.UserID
}

// subscribedDevice signs one device in and stores a subscription pointing at
// endpoint, with real client keys so the sender genuinely encrypts for it.
func subscribedDevice(t *testing.T, sessions *store.SessionStore, userID int64, endpoint string, ttl time.Duration) []byte {
	t.Helper()
	token := make([]byte, 8)
	if _, err := rand.Read(token); err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if err := sessions.Create(context.Background(), userID, token, time.Now().Add(ttl)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, keys := deviceKeys(t)
	raw, err := json.Marshal(Subscription{Endpoint: endpoint, Keys: keys})
	if err != nil {
		t.Fatalf("marshal subscription: %v", err)
	}
	if err := sessions.SetPushSubscription(context.Background(), token, raw); err != nil {
		t.Fatalf("store subscription: %v", err)
	}
	return token
}

func storedSubscription(t *testing.T, handle *db.DB, token []byte) string {
	t.Helper()
	var stored sql.NullString
	err := handle.Read.QueryRow(`SELECT PushSubscription FROM Session WHERE TokenHash = ?`, token).Scan(&stored)
	if err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	return stored.String
}

func sessionCount(t *testing.T, handle *db.DB, token []byte) int {
	t.Helper()
	var count int
	err := handle.Read.QueryRow(`SELECT COUNT(*) FROM Session WHERE TokenHash = ?`, token).Scan(&count)
	if err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return count
}
