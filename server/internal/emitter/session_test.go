package emitter

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"blurt/internal/config"
	"blurt/internal/push"
	"blurt/internal/store"
)

// These are the compose file's dev VAPID identity: a real P-256 pair, so the sender
// signs a real VAPID token rather than being stubbed out.
const (
	testVAPIDPublic  = "BPCRZS3Lfu-TYCCcyqO99WCNWKGt_JCnjO1U67-6qVHrPWnRjs1IGxle6v7Vv2RK89T3Dwj_itMGWtCv-up64XQ"
	testVAPIDPrivate = "m6UconuCpzkrbd1fui1DQb_GXOEGCjue8mhRAzVvak0"
)

// TestPushOutlivesSessionExpiryAndStopsOnLogout is the session rule end to end
// with real SQLite: a subscription belongs to a device, not to a login cookie, so
// an expired session still receives reminders. Only explicit logout deletes the
// row that owns the subscription. The Reminder's lifecycle never depends on it.
func TestPushOutlivesSessionExpiryAndStopsOnLogout(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	sessions := store.NewSessionStore(s.handle.Write, s.handle.Read)

	var delivered atomic.Int64
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer relay.Close()

	// One device: a real P-256 point and a 16-byte auth secret, exactly what a browser
	// stores, pointed at the relay the sender will POST to.
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate device key: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("generate auth: %v", err)
	}
	subscription, err := json.Marshal(push.Subscription{
		Endpoint: relay.URL + "/push",
		Keys: push.Keys{
			P256DH: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
			Auth:   base64.RawURLEncoding.EncodeToString(auth),
		},
	})
	if err != nil {
		t.Fatalf("marshal subscription: %v", err)
	}

	token := []byte("device-token")
	signIn := func() {
		t.Helper()
		if err := sessions.Create(ctx, s.userID, token, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("create session: %v", err)
		}
		if _, err := sessions.SetPushSubscription(ctx, token, subscription); err != nil {
			t.Fatalf("store subscription: %v", err)
		}
	}
	signIn()

	at := anchor
	emit := &Emitter{
		Reminders: s.reminders,
		Pusher: &push.Fanout{
			Sessions: sessions,
			Sender:   push.NewSender(config.VAPID{PublicKey: testVAPIDPublic, PrivateKey: testVAPIDPrivate, Subject: "mailto:e2e@example.com"}),
		},
		Now: func() time.Time { return at },
	}
	due := func(text string) {
		t.Helper()
		s.commit(t, store.Reminder{
			Text: text, Timezone: "Asia/Singapore",
			LocalDateTime: "2026-10-25T18:00", EmitAtUtc: "2026-10-25T10:00:00Z",
		})
		emit.sweep(ctx)
	}

	// Signed in: the reminder reaches the device.
	due("Buy glue")
	if got := delivered.Load(); got != 1 {
		t.Fatalf("delivered = %d, want the live session's push", got)
	}

	// The login cookie expires, but the device subscription does not. The next
	// firing still reaches the phone.
	if _, err := s.handle.Write.Exec(
		`UPDATE Session SET ExpiresAt = ? WHERE TokenHash = ?`,
		"2026-10-24T10:00:00Z", token); err != nil {
		t.Fatalf("expire session: %v", err)
	}
	due("Post letter")
	if got := delivered.Load(); got != 2 {
		t.Fatalf("delivered = %d, want push after session expiry", got)
	}

	// Logout deletes the session row, subscription and all. The emitter still fires
	// what came due, but there is nobody to push to.
	if err := sessions.Delete(ctx, token); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	due("Water plants")
	if got := delivered.Load(); got != 2 {
		t.Fatalf("delivered = %d, want nothing after logout", got)
	}

	// Signing back in re-registers a device, and the next firing reaches it.
	signIn()
	due("Take out trash")
	if got := delivered.Load(); got != 3 {
		t.Fatalf("delivered = %d, want the new session's push", got)
	}

	// Every firing is still one Reminder, fired whether or not a device was there.
	var firedReminders int
	if err := s.handle.Read.QueryRow(
		`SELECT count(*) FROM Reminder WHERE SourceJobId IS NOT NULL`).Scan(&firedReminders); err != nil {
		t.Fatalf("count reminders: %v", err)
	}
	if firedReminders != 4 {
		t.Fatalf("reminders = %d, want one row per firing", firedReminders)
	}
	var skipped int
	if err := s.handle.Read.QueryRow(`
		SELECT count(*) FROM Trace
		WHERE Stage = ? AND json_extract(Detail, '$.outcome') = 'skipped_no_device'`,
		store.NotificationStage).Scan(&skipped); err != nil && err != sql.ErrNoRows {
		t.Fatalf("count skipped deliveries: %v", err)
	}
	if skipped != 1 {
		t.Fatalf("skipped delivery traces = %d, want the logged-out firing recorded", skipped)
	}
}
