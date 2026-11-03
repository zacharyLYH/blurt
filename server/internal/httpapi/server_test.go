package httpapi_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"blurt/internal/auth"
	"blurt/internal/capture"
	"blurt/internal/db"
	"blurt/internal/httpapi"
	"blurt/internal/push"
	"blurt/internal/queue"
	"blurt/internal/store"
)

type sentPin struct {
	to  string
	pin string
}

// fakeMailer stands in for SMTP so the test can read what was sent while the
// rest of the path (PIN store, session cookie, handlers) stays real.
type fakeMailer struct {
	mu       sync.Mutex
	sent     []sentPin
	approved []string
	rejected []string
}

func (m *fakeMailer) SendPIN(to, pin string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentPin{to: to, pin: pin})
	return nil
}

func (m *fakeMailer) SendApproved(to, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.approved = append(m.approved, to)
	return nil
}

func (m *fakeMailer) SendRejected(to, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rejected = append(m.rejected, to)
	return nil
}

func (m *fakeMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func (m *fakeMailer) last(t *testing.T) sentPin {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		t.Fatal("no PIN email was sent")
	}
	return m.sent[len(m.sent)-1]
}

// fakePusher stands in for the relay, so the send handler's own decisions
// (which user, what payload, how a rejection maps to a status code) are what
// gets tested. The rest of the path stays real.
type fakePusher struct {
	mu      sync.Mutex
	calls   int
	userID  int64
	message push.Message
	result  push.Result
	err     error
}

func (p *fakePusher) NotifyUser(_ context.Context, userID int64, msg push.Message) (push.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.userID, p.message = userID, msg
	return p.result, p.err
}

// testPushKeys mints the device half of a subscription, because the subscribe
// endpoint now proves the key is a point the encryption can use.
func testPushKeys(t *testing.T) map[string]string {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate push key: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("generate auth secret: %v", err)
	}
	return map[string]string{
		"p256dh": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		"auth":   base64.RawURLEncoding.EncodeToString(auth),
	}
}

type envelope struct {
	User store.User `json:"user"`
}

// testOrigin must be in Deps.AllowedOrigins; the CSRF guard rejects unsafe
// requests that arrive without an allowed Origin.
const testOrigin = "http://blurt.test"

// testOwner is the address the test stack names as owner: the one account that
// may approve or reject everyone else.
const testOwner = "owner@example.com"

// testHandle is the database the current stack booted on. Signing in never
// creates a row, so the login helper approves an applicant the way the owner
// would; the approval API itself is exercised in admin_test.go.
var testHandle *db.DB

// approve gives the address the row an approved applicant would have.
func approve(t *testing.T, email string) {
	t.Helper()
	if _, err := store.NewUserStore(testHandle.Write, testHandle.Read).
		EnsureActive(context.Background(), "Lam", email); err != nil {
		t.Fatalf("approve %s: %v", email, err)
	}
}

// A real P-256 public point (65 bytes, base64url) so the key endpoint has a
// realistic value to hand out.
const testVAPIDPublicKey = "BPCRZS3Lfu-TYCCcyqO99WCNWKGt_JCnjO1U67-6qVHrPWnRjs1IGxle6v7Vv2RK89T3Dwj_itMGWtCv-up64XQ"

// newClient boots the dev stack: no mailer, so the PIN is fixed and login is
// the same path the browser takes locally. One way in, for every test.
func newClient(t *testing.T) (*http.Client, string, *db.DB) {
	t.Helper()
	client, base, handle, _ := newTestStack(t, auth.NewPinStore(time.Minute, auth.DevPIN), nil, nil)
	return client, base, handle
}

// newPushClient is newClient plus a recording pusher, so the send endpoint can
// be driven without a relay.
func newPushClient(t *testing.T) (*http.Client, string, *fakePusher, *db.DB) {
	t.Helper()
	pusher := &fakePusher{}
	client, base, handle, _ := newTestStack(t, auth.NewPinStore(time.Minute, auth.DevPIN), nil, pusher)
	return client, base, pusher, handle
}

// newQueueClient is newClient plus the queues the handlers publish to, so a test
// can see which stream a capture was routed to.
func newQueueClient(t *testing.T) (*http.Client, string, *db.DB, *queue.Queues) {
	t.Helper()
	client, base, handle, queues := newTestStack(t, auth.NewPinStore(time.Minute, auth.DevPIN), nil, nil)
	return client, base, handle, queues
}

// consume takes the next message off a stage stream.
func consume(t *testing.T, stream queue.Stream) (int64, int) {
	t.Helper()
	jobID, attempt, _, err := stream.Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	return jobID, attempt
}

// newTestStack boots the API on a throwaway database. A nil mailer is the dev
// path (BLURT_FAKE_PIN): fixed PIN, no mail. A mailer is production: only an
// approved row gets a PIN, and it is mailed. Either way signing in never
// creates an account.
func newTestStack(t *testing.T, pins *auth.PinStore, mailer httpapi.Mailer, pusher push.Notifier) (*http.Client, string, *db.DB, *queue.Queues) {
	t.Helper()
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	testHandle = handle
	audio, err := capture.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("open audio store: %v", err)
	}
	queues := &queue.Queues{TranscriptionQueue: queue.NewMock(), AgentQueue: queue.NewMock()}

	server := httptest.NewServer(httpapi.New(httpapi.Deps{
		DB:             handle.Write,
		Users:          store.NewUserStore(handle.Write, handle.Read),
		Sessions:       store.NewSessionStore(handle.Write, handle.Read),
		Pins:           pins,
		Mailer:         mailer,
		Owner:          testOwner,
		Pusher:         pusher,
		Jobs:           store.NewJobStore(handle.Write, handle.Read),
		Audio:          audio,
		Queues:         queues,
		Reminders:      store.NewReminderStore(handle.Write, handle.Read),
		Notifications:  store.NewNotificationStore(handle.Write, handle.Read),
		VAPIDPublicKey: testVAPIDPublicKey,
		AllowedOrigins: []string{testOrigin},
	}))
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &http.Client{Jar: jar}, server.URL, handle, queues
}

func do(t *testing.T, client *http.Client, method, url string, body any, origin string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func call(t *testing.T, client *http.Client, method, url string, body any) (int, []byte) {
	t.Helper()
	res := do(t, client, method, url, body, testOrigin)
	reply, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return res.StatusCode, reply
}

// requestPin asks for a PIN on the dev stack, where the code is fixed and
// nothing is mailed.
func requestPin(t *testing.T, client *http.Client, base, email string) string {
	t.Helper()
	status, body := call(t, client, http.MethodPost, base+"/api/auth/request-pin", map[string]string{"email": email})
	if status != http.StatusOK {
		t.Fatalf("request-pin status = %d: %s", status, body)
	}
	return auth.DevPIN
}

func login(t *testing.T, client *http.Client, base, email string) store.User {
	t.Helper()
	approve(t, email)
	pin := requestPin(t, client, base, email)
	status, body := call(t, client, http.MethodPost, base+"/api/auth/verify-pin", map[string]string{
		"email": email, "pin": pin,
	})
	if status != http.StatusOK {
		t.Fatalf("verify-pin status = %d: %s", status, body)
	}
	var reply envelope
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode verify-pin: %v", err)
	}
	return reply.User
}

// newUserClient opens a second browser session, signed in as someone else.
func newUserClient(t *testing.T, base, email string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar}
	login(t, client, base, email)
	return client
}

// sessionExpiry reads the single session's stored expiry, the source of truth
// for whether the user is still signed in.
func sessionExpiry(t *testing.T, handle *db.DB) time.Time {
	t.Helper()
	var raw string
	if err := handle.Read.QueryRow(`SELECT ExpiresAt FROM Session LIMIT 1`).Scan(&raw); err != nil {
		t.Fatalf("read session expiry: %v", err)
	}
	expiry, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("parse session expiry %q: %v", raw, err)
	}
	return expiry
}

// expireSessions moves every session into the past without touching the
// browser cookie, so the next request exercises the expired-session path.
func expireSessions(t *testing.T, handle *db.DB) {
	t.Helper()
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if _, err := handle.Write.Exec(`UPDATE Session SET ExpiresAt = ?`, past); err != nil {
		t.Fatalf("expire sessions: %v", err)
	}
}

func TestUserRequiresSession(t *testing.T) {
	client, base, _ := newClient(t)
	status, body := call(t, client, http.MethodGet, base+"/api/user", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", status, body)
	}
}

func TestPinFlowIssuesSession(t *testing.T) {
	client, base, _ := newClient(t)
	approve(t, "lam@example.com")
	pin := requestPin(t, client, base, "lam@example.com")

	status, body := call(t, client, http.MethodPost, base+"/api/auth/verify-pin", map[string]string{
		"email": "lam@example.com", "pin": "000000",
	})
	if pin == "000000" {
		t.Fatalf("fake PIN collided with the wrong-PIN probe")
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong PIN status = %d, want 401: %s", status, body)
	}

	status, body = call(t, client, http.MethodPost, base+"/api/auth/verify-pin", map[string]string{
		"email": "lam@example.com", "pin": pin,
	})
	if status != http.StatusOK {
		t.Fatalf("verify-pin status = %d: %s", status, body)
	}

	status, body = call(t, client, http.MethodGet, base+"/api/user", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/user status = %d: %s", status, body)
	}
	var reply envelope
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode user: %v", err)
	}
	if reply.User.Email != "lam@example.com" || reply.User.UserID == 0 {
		t.Fatalf("unexpected user: %+v", reply.User)
	}
	// Single use: replaying the same PIN fails.
	if status, _ := call(t, client, http.MethodPost, base+"/api/auth/verify-pin", map[string]string{
		"email": "lam@example.com", "pin": pin,
	}); status != http.StatusUnauthorized {
		t.Fatalf("replayed PIN status = %d, want 401", status)
	}
}

func TestSignedInUserCannotRequestPin(t *testing.T) {
	client, base, _ := newClient(t)
	login(t, client, base, "lam@example.com")

	status, body := call(t, client, http.MethodPost, base+"/api/auth/request-pin", map[string]string{"email": "lam@example.com"})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", status, body)
	}
}

// Dev mode: the PIN is fixed and nothing is mailed, which is what lets a stack
// with no mailbox sign in.
func TestFakePINModeSkipsMail(t *testing.T) {
	client, base, _, _ := newTestStack(t, auth.NewPinStore(time.Minute, auth.DevPIN), nil, nil)
	approve(t, "dev@example.com")

	status, body := call(t, client, http.MethodPost, base+"/api/auth/request-pin", map[string]string{"email": "dev@example.com"})
	if status != http.StatusOK {
		t.Fatalf("request-pin status = %d: %s", status, body)
	}
	status, body = call(t, client, http.MethodPost, base+"/api/auth/verify-pin", map[string]string{
		"email": "dev@example.com", "pin": auth.DevPIN,
	})
	if status != http.StatusOK {
		t.Fatalf("verify-pin status = %d: %s", status, body)
	}
	if status, body := call(t, client, http.MethodGet, base+"/api/user", nil); status != http.StatusOK {
		t.Fatalf("GET /api/user status = %d: %s", status, body)
	}
}

// Production remembers an email before it can sign in: an admin creates the
// User row. Unknown addresses get the same 200 as a known one, but no PIN and
// no row. A seeded row signs in with the name the admin chose.
func TestSignupClosedRequiresExistingUser(t *testing.T) {
	mailer := &fakeMailer{}
	client, base, handle, _ := newTestStack(t, auth.NewPinStore(time.Minute, ""), mailer, nil)

	status, body := call(t, client, http.MethodPost, base+"/api/auth/request-pin", map[string]string{
		"email": "stranger@example.com",
	})
	if status != http.StatusOK {
		t.Fatalf("unknown request-pin status = %d, want 200: %s", status, body)
	}
	if mailer.count() != 0 {
		t.Fatalf("PIN emails = %d, want 0", mailer.count())
	}
	var users int
	if err := handle.Read.QueryRow(`SELECT count(*) FROM User`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 0 {
		t.Fatalf("users = %d, want 0: an unknown email must not create one", users)
	}

	// A guessed code cannot create the missing row either.
	status, body = call(t, client, http.MethodPost, base+"/api/auth/verify-pin", map[string]string{
		"name": "Stranger", "email": "stranger@example.com", "pin": "000000",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("unknown verify-pin status = %d, want 401: %s", status, body)
	}

	// The admin's row is the guest list. Its name wins over the login form.
	if _, err := handle.Write.Exec(
		`INSERT INTO User (Name, Email, Status, CreatedAt) VALUES (?, ?, 'active', ?)`,
		"Lam", "lam@example.com", time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	status, body = call(t, client, http.MethodPost, base+"/api/auth/request-pin", map[string]string{
		"email": "lam@example.com",
	})
	if status != http.StatusOK {
		t.Fatalf("seeded request-pin status = %d: %s", status, body)
	}
	sent := mailer.last(t)
	if sent.to != "lam@example.com" {
		t.Fatalf("PIN sent to %q, want lam@example.com", sent.to)
	}
	status, body = call(t, client, http.MethodPost, base+"/api/auth/verify-pin", map[string]string{
		"name": "Imposter", "email": "lam@example.com", "pin": sent.pin,
	})
	if status != http.StatusOK {
		t.Fatalf("seeded verify-pin status = %d: %s", status, body)
	}
	var reply envelope
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode verify-pin: %v", err)
	}
	if reply.User.Name != "Lam" {
		t.Fatalf("name = %q, want the seeded Lam", reply.User.Name)
	}
	if err := handle.Read.QueryRow(`SELECT count(*) FROM User`).Scan(&users); err != nil {
		t.Fatalf("count users after login: %v", err)
	}
	if users != 1 {
		t.Fatalf("users = %d, want 1", users)
	}
}

// An address nobody seeded cannot get in, however the login is attempted: the
// request answers 200 but mints nothing, so every code — the fixed dev one
// included — is refused, and no session or User row is left behind.
func TestLoginWithUnseededEmailFails(t *testing.T) {
	mailer := &fakeMailer{}
	client, base, handle, _ := newTestStack(t, auth.NewPinStore(time.Minute, ""), mailer, nil)

	if status, body := call(t, client, http.MethodPost, base+"/api/auth/request-pin", map[string]string{
		"email": "stranger@example.com",
	}); status != http.StatusOK {
		t.Fatalf("request-pin status = %d, want 200: %s", status, body)
	}
	if mailer.count() != 0 {
		t.Fatalf("PIN emails = %d, want 0", mailer.count())
	}

	// Nothing was minted, so nothing verifies: not a guess, not the dev PIN.
	for _, pin := range []string{"000000", auth.DevPIN} {
		status, body := call(t, client, http.MethodPost, base+"/api/auth/verify-pin", map[string]string{
			"name": "Stranger", "email": "stranger@example.com", "pin": pin,
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("verify-pin %q status = %d, want 401: %s", pin, status, body)
		}
	}

	// A failed login leaves nothing behind: no session, no row.
	if status, body := call(t, client, http.MethodGet, base+"/api/user", nil); status != http.StatusUnauthorized {
		t.Fatalf("GET /api/user status = %d, want 401: %s", status, body)
	}
	var users int
	if err := handle.Read.QueryRow(`SELECT count(*) FROM User`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 0 {
		t.Fatalf("users = %d, want 0: a failed login must not create one", users)
	}
}

func TestQuestionnaireStoresMemoryAndVersion(t *testing.T) {
	client, base, handle := newClient(t)
	login(t, client, base, "lam@example.com")

	status, body := call(t, client, http.MethodPost, base+"/api/questionnaire", map[string]any{
		"version": 1,
		"answers": []map[string]string{
			{"question": "What time zone are you in?", "answer": "Asia/Singapore"},
			{"question": "When do you wake up?", "answer": "7am"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("submit status = %d: %s", status, body)
	}
	var reply envelope
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode submit: %v", err)
	}
	if reply.User.LastQuestionnaireVersion != 1 {
		t.Fatalf("version = %d, want 1", reply.User.LastQuestionnaireVersion)
	}
	if got := countQuestionnaireMemory(t, handle); got != 2 {
		t.Fatalf("memory rows = %d, want 2", got)
	}

	// Resubmitting replaces stored answers instead of duplicating them.
	status, body = call(t, client, http.MethodPost, base+"/api/questionnaire", map[string]any{
		"version": 2,
		"answers": []map[string]string{
			{"question": "What time zone are you in?", "answer": "Asia/Tokyo"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("resubmit status = %d: %s", status, body)
	}
	if got := countQuestionnaireMemory(t, handle); got != 1 {
		t.Fatalf("memory rows after resubmit = %d, want 1", got)
	}

	status, body = call(t, client, http.MethodGet, base+"/api/user", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/user status = %d: %s", status, body)
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode user: %v", err)
	}
	if reply.User.LastQuestionnaireVersion != 2 {
		t.Fatalf("version = %d, want 2", reply.User.LastQuestionnaireVersion)
	}
}

func countQuestionnaireMemory(t *testing.T, handle *db.DB) int {
	t.Helper()
	var count int
	if err := handle.Read.QueryRow(`SELECT count(*) FROM Memory WHERE Tags = 'questionnaire'`).Scan(&count); err != nil {
		t.Fatalf("count memory: %v", err)
	}
	return count
}

func TestQuestionnaireRejectsBadInput(t *testing.T) {
	client, base, _ := newClient(t)
	login(t, client, base, "lam@example.com")

	for name, payload := range map[string]any{
		"zero version": map[string]any{"version": 0, "answers": []map[string]string{{"question": "q", "answer": "a"}}},
		"no answers":   map[string]any{"version": 1, "answers": []map[string]string{}},
		"blank answer": map[string]any{"version": 1, "answers": []map[string]string{{"question": "q", "answer": " "}}},
	} {
		status, body := call(t, client, http.MethodPost, base+"/api/questionnaire", payload)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400: %s", name, status, body)
		}
	}
}

func TestUnsafeRequestsNeedAllowedOrigin(t *testing.T) {
	client, base, _ := newClient(t)
	payload := map[string]string{"email": "lam@example.com"}

	if res := do(t, client, http.MethodPost, base+"/api/auth/request-pin", payload, ""); res.StatusCode != http.StatusForbidden {
		t.Fatalf("missing Origin status = %d, want 403", res.StatusCode)
	}
	if res := do(t, client, http.MethodPost, base+"/api/auth/request-pin", payload, "https://evil.example"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", res.StatusCode)
	}
	if res := do(t, client, http.MethodGet, base+"/api/user", nil, "https://evil.example"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("safe method status = %d, want 401", res.StatusCode)
	}
}

func TestSecureCookieBehindHTTPSProxy(t *testing.T) {
	client, base, _ := newClient(t)
	approve(t, "lam@example.com")
	pin := requestPin(t, client, base, "lam@example.com")

	payload, err := json.Marshal(map[string]string{"email": "lam@example.com", "pin": pin})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/api/auth/verify-pin", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("verify-pin: %v", err)
	}
	defer res.Body.Close()

	if cookie := res.Header.Get("Set-Cookie"); !strings.Contains(cookie, "Secure") {
		t.Fatalf("https session cookie missing Secure: %s", cookie)
	}
}

func TestRejectsOversizedBody(t *testing.T) {
	client, base, _ := newClient(t)
	status, _ := call(t, client, http.MethodPost, base+"/api/auth/request-pin", map[string]string{
		"email": "lam@example.com",
		"pad":   strings.Repeat("x", 2<<20),
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

func TestSessionCookieIsHardened(t *testing.T) {
	client, base, _ := newClient(t)
	approve(t, "lam@example.com")
	pin := requestPin(t, client, base, "lam@example.com")

	res := do(t, client, http.MethodPost, base+"/api/auth/verify-pin", map[string]string{
		"email": "lam@example.com", "pin": pin,
	}, testOrigin)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("verify-pin status = %d", res.StatusCode)
	}

	cookie := res.Header.Get("Set-Cookie")
	for _, flag := range []string{"HttpOnly", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(cookie, flag) {
			t.Fatalf("session cookie missing %q: %s", flag, cookie)
		}
	}
	// Local test server is plain http: Secure would make the browser drop it.
	if strings.Contains(cookie, "Secure") {
		t.Fatalf("session cookie marked Secure over http: %s", cookie)
	}

	// Sessions slide on use: an authenticated response refreshes the cookie.
	refresh := do(t, client, http.MethodGet, base+"/api/user", nil, "")
	if refreshed := refresh.Header.Get("Set-Cookie"); !strings.Contains(refreshed, "Max-Age=") || !strings.Contains(refreshed, "HttpOnly") {
		t.Fatalf("session cookie not refreshed on use: %q", refreshed)
	}

	// A forged token never resolves to a user.
	forged, err := http.NewRequest(http.MethodGet, base+"/api/user", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	forged.AddCookie(&http.Cookie{Name: "blurt_session", Value: "Zm9yZ2Vk"})
	// Fresh client: the logged-in jar would add the valid cookie too.
	response, err := (&http.Client{}).Do(forged)
	if err != nil {
		t.Fatalf("forged request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged cookie status = %d, want 401", response.StatusCode)
	}
}

// A probe that cannot refresh the browser cookie must not slide the store row.
func TestRequestPinProbeDoesNotSlideSession(t *testing.T) {
	client, base, handle := newClient(t)
	login(t, client, base, "lam@example.com")

	before := sessionExpiry(t, handle)
	res := do(t, client, http.MethodPost, base+"/api/auth/request-pin",
		map[string]string{"email": "lam@example.com"}, testOrigin)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.StatusCode)
	}
	if cookie := res.Header.Get("Set-Cookie"); cookie != "" {
		t.Fatalf("signed-in probe refreshed the cookie: %q", cookie)
	}
	if after := sessionExpiry(t, handle); !after.Equal(before) {
		t.Fatalf("probe slid expiry from %s to %s", before, after)
	}
}

// An expired row is dead: the request is rejected, nothing is refreshed, and
// the user can start a fresh login instead of being told they are signed in.
func TestExpiredSessionIsRejectedNotRevived(t *testing.T) {
	client, base, handle := newClient(t)
	login(t, client, base, "lam@example.com")
	expireSessions(t, handle)
	expired := sessionExpiry(t, handle)

	res := do(t, client, http.MethodGet, base+"/api/user", nil, testOrigin)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	if cookie := res.Header.Get("Set-Cookie"); cookie != "" {
		t.Fatalf("expired session was refreshed: %q", cookie)
	}
	if after := sessionExpiry(t, handle); !after.Equal(expired) {
		t.Fatalf("expired session moved from %s to %s", expired, after)
	}

	status, body := call(t, client, http.MethodPost, base+"/api/auth/request-pin",
		map[string]string{"email": "lam@example.com"})
	if status != http.StatusOK {
		t.Fatalf("request-pin status = %d, want 200: %s", status, body)
	}
}

// Renewal must move the store row and the browser cookie to the same instant,
// otherwise they drift apart and one expires before the other.
func TestSessionRenewalAlignsCookieAndStore(t *testing.T) {
	client, base, handle := newClient(t)
	login(t, client, base, "lam@example.com")

	// Shrink the stored session so a successful renewal is unambiguous.
	near := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	if _, err := handle.Write.Exec(`UPDATE Session SET ExpiresAt = ?`, near); err != nil {
		t.Fatalf("shrink session: %v", err)
	}

	res := do(t, client, http.MethodGet, base+"/api/user", nil, testOrigin)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var session *http.Cookie
	for _, cookie := range res.Cookies() {
		if cookie.Name == "blurt_session" {
			session = cookie
		}
	}
	if session == nil || session.Expires.IsZero() {
		t.Fatalf("no refreshed session cookie with an expiry: %+v", res.Cookies())
	}

	stored := sessionExpiry(t, handle)
	if !session.Expires.Equal(stored) {
		t.Fatalf("cookie expiry %s != store expiry %s", session.Expires, stored)
	}
	if remaining := time.Until(stored); remaining < 29*24*time.Hour {
		t.Fatalf("session renewed only %s ahead", remaining)
	}
}

// A cookie that is not even valid base64 must behave like no cookie at all.
func TestMalformedSessionCookieIsUnauthorized(t *testing.T) {
	_, base, _ := newClient(t)
	req, err := http.NewRequest(http.MethodGet, base+"/api/user", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "blurt_session", Value: "not base64!!"})
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

func TestPushKeyReturnsConfiguredKey(t *testing.T) {
	client, base, _ := newClient(t)

	status, body := call(t, client, http.MethodGet, base+"/api/push/key", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var reply struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode key: %v", err)
	}
	if reply.PublicKey != testVAPIDPublicKey {
		t.Fatalf("publicKey = %q, want %q", reply.PublicKey, testVAPIDPublicKey)
	}
}

func TestPushSubscribeStoresAndReplacesSubscription(t *testing.T) {
	client, base, handle := newClient(t)
	login(t, client, base, "lam@example.com")

	subscription := func(endpoint string) map[string]any {
		return map[string]any{"endpoint": endpoint, "keys": testPushKeys(t)}
	}

	status, body := call(t, client, http.MethodPost, base+"/api/push/subscribe",
		subscription("https://fcm.googleapis.com/first"))
	if status != http.StatusNoContent {
		t.Fatalf("subscribe status = %d: %s", status, body)
	}
	var stored string
	if err := handle.Read.QueryRow(`SELECT PushSubscription FROM Session LIMIT 1`).Scan(&stored); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if !strings.Contains(stored, "https://fcm.googleapis.com/first") {
		t.Fatalf("stored subscription = %q", stored)
	}

	status, body = call(t, client, http.MethodPost, base+"/api/push/subscribe",
		subscription("https://fcm.googleapis.com/second"))
	if status != http.StatusNoContent {
		t.Fatalf("re-subscribe status = %d: %s", status, body)
	}
	if err := handle.Read.QueryRow(`SELECT PushSubscription FROM Session LIMIT 1`).Scan(&stored); err != nil {
		t.Fatalf("read replaced subscription: %v", err)
	}
	if !strings.Contains(stored, "/second") || strings.Contains(stored, "/first") {
		t.Fatalf("subscription not replaced: %q", stored)
	}
}

// A device that just subscribed gets one notification back: it is the only proof
// the user has that the whole chain — permission, subscription, relay, worker —
// reaches this phone. The handoff repeats on every page load, so the same
// subscription must not turn into a notification every page load.
func TestPushSubscribeConfirmsOnce(t *testing.T) {
	client, base, pusher, _ := newPushClient(t)
	login(t, client, base, "lam@example.com")
	pusher.result = push.Result{Sent: 1}

	subscription := map[string]any{"endpoint": "https://fcm.googleapis.com/first", "keys": testPushKeys(t)}
	if status, body := call(t, client, http.MethodPost, base+"/api/push/subscribe", subscription); status != http.StatusNoContent {
		t.Fatalf("subscribe status = %d: %s", status, body)
	}
	if pusher.calls != 1 {
		t.Fatalf("confirmation pushes = %d, want 1", pusher.calls)
	}
	if pusher.message.Tag != "push-ready" || pusher.message.Title == "" || pusher.message.Body == "" {
		t.Fatalf("confirmation = %+v, want a titled, tagged message", pusher.message)
	}

	// The very same subscription again: nothing new to confirm.
	if status, body := call(t, client, http.MethodPost, base+"/api/push/subscribe", subscription); status != http.StatusNoContent {
		t.Fatalf("re-subscribe status = %d: %s", status, body)
	}
	if pusher.calls != 1 {
		t.Fatalf("confirmation pushes after a repeat = %d, want 1", pusher.calls)
	}

	// A rotated endpoint is a new device as far as the user is concerned.
	if status, body := call(t, client, http.MethodPost, base+"/api/push/subscribe",
		map[string]any{"endpoint": "https://fcm.googleapis.com/second", "keys": testPushKeys(t)}); status != http.StatusNoContent {
		t.Fatalf("rotated subscribe status = %d: %s", status, body)
	}
	if pusher.calls != 2 {
		t.Fatalf("confirmation pushes after a rotation = %d, want 2", pusher.calls)
	}
}

func TestPushSubscribeValidatesPayload(t *testing.T) {
	client, base, _ := newClient(t)
	login(t, client, base, "lam@example.com")

	validKeys := testPushKeys(t)
	for name, payload := range map[string]any{
		"plain http":    map[string]any{"endpoint": "http://fcm.googleapis.com/x", "keys": validKeys},
		"empty host":    map[string]any{"endpoint": "https://", "keys": validKeys},
		"unknown host":  map[string]any{"endpoint": "https://evil.example/x", "keys": validKeys},
		"truncated key": map[string]any{"endpoint": "https://fcm.googleapis.com/x", "keys": map[string]string{"p256dh": "device-key", "auth": validKeys["auth"]}},
		"short auth":    map[string]any{"endpoint": "https://fcm.googleapis.com/x", "keys": map[string]string{"p256dh": validKeys["p256dh"], "auth": "device-auth"}},
		"missing key":   map[string]any{"endpoint": "https://fcm.googleapis.com/x", "keys": map[string]string{"p256dh": validKeys["p256dh"]}},
		"blank p256dh":  map[string]any{"endpoint": "https://fcm.googleapis.com/x", "keys": map[string]string{"p256dh": " ", "auth": validKeys["auth"]}},
	} {
		status, body := call(t, client, http.MethodPost, base+"/api/push/subscribe", payload)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400: %s", name, status, body)
		}
	}
}

func TestPushSubscribeRequiresSession(t *testing.T) {
	client, base, _ := newClient(t)

	status, body := call(t, client, http.MethodPost, base+"/api/push/subscribe", map[string]any{
		"endpoint": "https://fcm.googleapis.com/x",
		"keys":     testPushKeys(t),
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", status, body)
	}
}

// The send endpoint is scoped to the caller's own devices: blurt serves many
// users, so an open send would be a way to notify somebody else's phone.
func TestPushSendFansOutToCallerDevices(t *testing.T) {
	client, base, pusher, handle := newPushClient(t)
	login(t, client, base, "lam@example.com")
	pusher.result = push.Result{Sent: 1}

	status, body := call(t, client, http.MethodPost, base+"/api/push", map[string]any{
		"title": "Buy glue", "body": "On the way home. 6:00 PM SGT",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var reply push.Result
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if reply.Sent != 1 {
		t.Fatalf("result = %+v, want one sent", reply)
	}
	if pusher.calls != 1 {
		t.Fatalf("pusher called %d times, want 1", pusher.calls)
	}

	var wantUserID int64
	if err := handle.Read.QueryRow(`SELECT UserId FROM User WHERE Email = ?`, "lam@example.com").Scan(&wantUserID); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if pusher.userID != wantUserID {
		t.Fatalf("pusher got user %d, want the caller %d", pusher.userID, wantUserID)
	}
	if pusher.message.Title != "Buy glue" || !strings.Contains(pusher.message.Body, "6:00 PM") {
		t.Fatalf("message = %+v", pusher.message)
	}
	// No navigate sent means the app home, absolute so the service worker can
	// hand it straight to the browser.
	target, err := url.Parse(pusher.message.Navigate)
	if err != nil || target.Path != "/home" || target.Host == "" {
		t.Fatalf("navigate = %q, want a same-origin /home", pusher.message.Navigate)
	}
	// The reminder the push is about is backend-owned, so a client cannot point at one.
	if pusher.message.ReminderID != 0 {
		t.Fatalf("reminderId = %d, want the backend to own it", pusher.message.ReminderID)
	}

	// An explicit same-origin target is passed through unchanged.
	deepLink := base + "/jobs/7"
	if status, body := call(t, client, http.MethodPost, base+"/api/push", map[string]any{
		"title": "t", "body": "b", "navigate": deepLink,
	}); status != http.StatusOK {
		t.Fatalf("navigate status = %d: %s", status, body)
	}
	if pusher.message.Navigate != deepLink {
		t.Fatalf("navigate = %q, want %q", pusher.message.Navigate, deepLink)
	}

	// A path resolves against the worker's own origin, so it is passed through
	// as-is and cannot point anywhere else.
	if status, body := call(t, client, http.MethodPost, base+"/api/push", map[string]any{
		"title": "t", "body": "b", "navigate": "/jobs/7",
	}); status != http.StatusOK {
		t.Fatalf("relative navigate status = %d: %s", status, body)
	}
	if pusher.message.Navigate != "/jobs/7" {
		t.Fatalf("navigate = %q, want the path unchanged", pusher.message.Navigate)
	}
}

func TestPushSendValidatesPayload(t *testing.T) {
	client, base, pusher, _ := newPushClient(t)
	login(t, client, base, "lam@example.com")

	for name, payload := range map[string]any{
		"no title":          map[string]any{"body": "b"},
		"no body":           map[string]any{"title": "t"},
		"blank title":       map[string]any{"title": " ", "body": "b"},
		"long title":        map[string]any{"title": strings.Repeat("x", 101), "body": "b"},
		"long body":         map[string]any{"title": "t", "body": strings.Repeat("x", 501)},
		"long tag":          map[string]any{"title": "t", "body": "b", "tag": strings.Repeat("x", 65)},
		"foreign target":    map[string]any{"title": "t", "body": "b", "navigate": "https://evil.example/home"},
		"protocol relative": map[string]any{"title": "t", "body": "b", "navigate": "//evil.example/home"},
	} {
		status, body := call(t, client, http.MethodPost, base+"/api/push", payload)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400: %s", name, status, body)
		}
	}
	if pusher.calls != 0 {
		t.Fatalf("pusher ran %d times on invalid payloads", pusher.calls)
	}
}

func TestPushSendRequiresSession(t *testing.T) {
	client, base, _ := newClient(t)

	status, body := call(t, client, http.MethodPost, base+"/api/push", map[string]any{"title": "t", "body": "b"})
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", status, body)
	}
}

// A relay that refuses our identity is a deploy problem, so it is reported
// apart from a dead device.
func TestPushSendReportsVAPIDRejection(t *testing.T) {
	client, base, pusher, _ := newPushClient(t)
	login(t, client, base, "lam@example.com")
	pusher.result, pusher.err = push.Result{Failed: 1}, push.ErrVAPID

	status, body := call(t, client, http.MethodPost, base+"/api/push", map[string]any{"title": "t", "body": "b"})
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", status, body)
	}
	if strings.Contains(string(body), `"sent":0`) == false || !strings.Contains(string(body), "vapid_rejected") {
		t.Fatalf("body = %s", body)
	}
}

// Without a sender configured the endpoint says so instead of pretending.
func TestPushSendWithoutPusher(t *testing.T) {
	client, base, _ := newClient(t)
	login(t, client, base, "lam@example.com")

	status, body := call(t, client, http.MethodPost, base+"/api/push", map[string]any{"title": "t", "body": "b"})
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", status, body)
	}
}

func TestKnownPathRejectsWrongMethod(t *testing.T) {
	client, base, _ := newClient(t)

	res := do(t, client, http.MethodPost, base+"/api/user", nil, testOrigin)
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.StatusCode)
	}
	if allow := res.Header.Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", allow)
	}

	res = do(t, client, http.MethodGet, base+"/api/push", nil, testOrigin)
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/push status = %d, want 405", res.StatusCode)
	}

	status, body := call(t, client, http.MethodGet, base+"/api/not-built", nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("unknown endpoint status = %d, want 501: %s", status, body)
	}
}

func TestDecodeJSONRejectsTrailingData(t *testing.T) {
	client, base, _ := newClient(t)

	req, err := http.NewRequest(http.MethodPost, base+"/api/auth/request-pin",
		strings.NewReader(`{"email":"a@example.com"}{"email":"b@example.com"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestDecodeJSONRejectsNonJSONContentType(t *testing.T) {
	client, base, _ := newClient(t)

	req, err := http.NewRequest(http.MethodPost, base+"/api/auth/request-pin",
		strings.NewReader(`{"email":"a@example.com"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", testOrigin)
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", res.StatusCode)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	client, base, _ := newClient(t)
	login(t, client, base, "lam@example.com")

	if status, body := call(t, client, http.MethodPost, base+"/api/auth/logout", nil); status != http.StatusNoContent {
		t.Fatalf("logout status = %d: %s", status, body)
	}
	if status, body := call(t, client, http.MethodGet, base+"/api/user", nil); status != http.StatusUnauthorized {
		t.Fatalf("after logout status = %d, want 401: %s", status, body)
	}
}
