package push

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"blurt/internal/config"
)

// testVAPID mints a real identity: the sender signs a JWT with it, so a
// placeholder would only prove that the code compiles.
func testVAPID(t *testing.T) config.VAPID {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate VAPID key: %v", err)
	}
	return config.VAPID{
		PublicKey:  base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()),
		Subject:    "mailto:e2e@example.com",
	}
}

// deviceKeys mints the client half exactly as a browser would.
func deviceKeys(t *testing.T) (*ecdh.PrivateKey, Keys) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate device key: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("generate auth secret: %v", err)
	}
	return key, Keys{
		P256DH: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		Auth:   base64.RawURLEncoding.EncodeToString(auth),
	}
}

func TestValidateSubscription(t *testing.T) {
	_, keys := deviceKeys(t)
	valid := Subscription{Endpoint: "https://fcm.googleapis.com/fcm/send/x", Keys: keys}
	if err := ValidateSubscription(valid); err != nil {
		t.Fatalf("valid subscription rejected: %v", err)
	}
	// A local relay is the one place plain http is allowed.
	if err := ValidateSubscription(Subscription{Endpoint: "http://host.docker.internal:9/push", Keys: keys}); err != nil {
		t.Fatalf("local relay rejected: %v", err)
	}

	for name, sub := range map[string]Subscription{
		"plain http":      {Endpoint: "http://fcm.googleapis.com/x", Keys: keys},
		"empty host":      {Endpoint: "https://", Keys: keys},
		"unknown host":    {Endpoint: "https://evil.example/x", Keys: keys},
		"truncated point": {Endpoint: valid.Endpoint, Keys: Keys{P256DH: "AAAAAA", Auth: keys.Auth}},
		"short auth":      {Endpoint: valid.Endpoint, Keys: Keys{P256DH: keys.P256DH, Auth: "AAAAAA"}},
		"missing auth":    {Endpoint: valid.Endpoint, Keys: Keys{P256DH: keys.P256DH}},
		"missing point":   {Endpoint: valid.Endpoint, Keys: Keys{Auth: keys.Auth}},
	} {
		if err := ValidateSubscription(sub); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// The relay is real HTTP, so this covers the encryption, the VAPID signature,
// and the payload the service worker actually reads.
func TestSendEncryptsAndSigns(t *testing.T) {
	device, keys := deviceKeys(t)
	sender := NewSender(testVAPID(t))

	var captured http.Header
	var body []byte
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer relay.Close()

	raw, err := json.Marshal(Subscription{Endpoint: relay.URL + "/push", Keys: keys})
	if err != nil {
		t.Fatalf("marshal subscription: %v", err)
	}
	want := Message{Title: "Buy glue", Body: "6:00 PM SGT", Tag: "reminder-42", Navigate: "http://blurt.test/home"}
	if err := sender.Send(context.Background(), raw, Message{
		Title: want.Title, Body: want.Body, Tag: want.Tag, Navigate: want.Navigate, TraceID: 42,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := captured.Get("Content-Encoding"); got != "aes128gcm" {
		t.Fatalf("Content-Encoding = %q, want aes128gcm", got)
	}
	// The mailto: trap: webpush-go adds the scheme itself, so passing the
	// configured "mailto:" through would sign "mailto:mailto:..." and Apple
	// answers 403 BadJwtToken.
	if got := vapidSubject(t, captured.Get("Authorization")); got != "mailto:e2e@example.com" {
		t.Fatalf("VAPID sub = %q, want one mailto:", got)
	}
	if len(body) <= 100 {
		t.Fatalf("body is %d bytes, want a real encrypted record", len(body))
	}

	var got payload
	if err := json.Unmarshal(decrypt(t, device, keys, body), &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got.WebPush != webPushVersion {
		t.Fatalf("web_push = %d, want %d", got.WebPush, webPushVersion)
	}
	if got.Notification != want {
		t.Fatalf("notification = %+v, want %+v", got.Notification, want)
	}
	if got.TraceID != 42 {
		t.Fatalf("notificationTraceId = %d, want 42", got.TraceID)
	}
}

// A trace id of zero is left out, matching the worker's null default.
func TestSendOmitsEmptyTraceID(t *testing.T) {
	device, keys := deviceKeys(t)
	sender := NewSender(testVAPID(t))

	var body []byte
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer relay.Close()

	raw, err := json.Marshal(Subscription{Endpoint: relay.URL + "/push", Keys: keys})
	if err != nil {
		t.Fatalf("marshal subscription: %v", err)
	}
	if err := sender.Send(context.Background(), raw, Message{Title: "t", Body: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(decrypt(t, device, keys, body), &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if _, present := decoded["notificationTraceId"]; present {
		t.Fatalf("payload carried a zero notificationTraceId: %s", decoded)
	}
}

// Every relay answer the sender has an opinion about. The request count is
// asserted too, because the only retry in the path is the rate-limit one.
func TestSendMapsRelayStatus(t *testing.T) {
	sender := NewSender(testVAPID(t))
	_, keys := deviceKeys(t)

	for name, tc := range map[string]struct {
		statuses []int
		wantIs   error
		wantErr  bool
	}{
		"endpoint gone":              {[]int{http.StatusGone}, ErrGone, true},
		"endpoint not found":         {[]int{http.StatusNotFound}, ErrGone, true},
		"relay refuses our VAPID":    {[]int{http.StatusForbidden}, ErrVAPID, true},
		"relay rejects the payload":  {[]int{http.StatusRequestEntityTooLarge}, nil, true},
		"accepted":                   {[]int{http.StatusCreated}, nil, false},
		"rate limited then accepted": {[]int{http.StatusTooManyRequests, http.StatusCreated}, nil, false},
		"rate limited twice":         {[]int{http.StatusTooManyRequests, http.StatusTooManyRequests}, nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				status := tc.statuses[min(calls, len(tc.statuses)-1)]
				calls++
				w.WriteHeader(status)
			}))
			defer relay.Close()

			raw, err := json.Marshal(Subscription{Endpoint: relay.URL + "/push", Keys: keys})
			if err != nil {
				t.Fatalf("marshal subscription: %v", err)
			}
			err = sender.Send(context.Background(), raw, Message{Title: "t", Body: "b"})
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("err = %v, want %v", err, tc.wantIs)
			}
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if calls != len(tc.statuses) {
				t.Fatalf("relay saw %d requests, want %d", calls, len(tc.statuses))
			}
		})
	}
}

// A corrupt row must fail loudly instead of being sent as a blank endpoint.
func TestSendRejectsCorruptStoredSubscription(t *testing.T) {
	sender := NewSender(testVAPID(t))
	if err := sender.Send(context.Background(), []byte("not json"), Message{Title: "t", Body: "b"}); err == nil {
		t.Fatal("Send accepted a corrupt stored subscription")
	}
}

// vapidSubject reads the sub claim out of the VAPID Authorization header:
// "vapid t=<jwt>, k=<key>". It is the only place the mailto: prefix is
// observable, and the only claim a relay rejects us for.
func vapidSubject(t *testing.T, authorization string) string {
	t.Helper()
	_, rest, ok := strings.Cut(authorization, "t=")
	if !ok {
		t.Fatalf("no VAPID token in %q", authorization)
	}
	token, _, _ := strings.Cut(rest, ",")
	// The token is a JWS: only the payload segment carries the claims.
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		t.Fatalf("VAPID token has %d segments, want 3", len(segments))
	}
	raw, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		t.Fatalf("decode VAPID token: %v", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("decode VAPID claims: %v", err)
	}
	return claims.Sub
}

// decrypt receives one aes128gcm record (RFC 8188) using the RFC 8291 key
// schedule, so the assertion is what a browser would actually decode.
func decrypt(t *testing.T, private *ecdh.PrivateKey, keys Keys, body []byte) []byte {
	t.Helper()
	if len(body) < 21 {
		t.Fatalf("record is %d bytes, too short", len(body))
	}
	salt, recordSize := body[:16], binary.BigEndian.Uint32(body[16:20])
	idLength := int(body[20])
	if len(body) < 21+idLength {
		t.Fatalf("record is too short for its %d byte key id", idLength)
	}
	senderKey, ciphertext := body[21:21+idLength], body[21+idLength:]
	public, err := ecdh.P256().NewPublicKey(senderKey)
	if err != nil {
		t.Fatalf("decode sender key: %v", err)
	}
	shared, err := private.ECDH(public)
	if err != nil {
		t.Fatalf("derive shared secret: %v", err)
	}
	auth, err := base64.RawURLEncoding.DecodeString(keys.Auth)
	if err != nil {
		t.Fatalf("decode auth secret: %v", err)
	}

	prk, err := hkdf.Extract(sha256.New, shared, auth)
	if err != nil {
		t.Fatalf("extract ikm: %v", err)
	}
	info := append([]byte("WebPush: info\x00"), private.PublicKey().Bytes()...)
	ikm, err := hkdf.Expand(sha256.New, prk, string(append(info, senderKey...)), 32)
	if err != nil {
		t.Fatalf("expand ikm: %v", err)
	}
	keyPRK, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		t.Fatalf("extract keys: %v", err)
	}
	contentKey, err := hkdf.Expand(sha256.New, keyPRK, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		t.Fatalf("expand content key: %v", err)
	}
	nonce, err := hkdf.Expand(sha256.New, keyPRK, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		t.Fatalf("expand nonce: %v", err)
	}

	block, err := aes.NewCipher(contentKey)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("new gcm: %v", err)
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("open record: %v", err)
	}
	// The last plaintext byte is the delimiter 0x02, then padding to the
	// record size.
	end := len(plain) - 1
	for end >= 0 && plain[end] == 0 {
		end--
	}
	if end < 0 || plain[end] != 0x02 {
		t.Fatalf("no padding delimiter in a %d byte record", recordSize)
	}
	return plain[:end]
}
