// Package push delivers Web Push notifications. One subscription belongs to
// one session (device), so the store owns persistence and this package owns
// the transport plus the per-user fan-out.
package push

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"blurt/internal/config"
)

const (
	// webPushVersion marks the payload shape the service worker understands.
	webPushVersion = 8030
	// ttlSeconds keeps an undelivered message for a minute: Blurt sends
	// reminders, not news, so a stale delivery is worse than a miss.
	ttlSeconds = 60
	// sendTimeout bounds one device so a slow relay cannot stall the fan-out.
	sendTimeout = 10 * time.Second
	// retryAfterCap bounds the pause before the single rate-limit retry.
	retryAfterCap = 2 * time.Second
	// authSecretLen is the 16-byte secret the browser mints per subscription.
	authSecretLen = 16
)

// ErrGone means the relay reports this endpoint dead. The caller drops the
// stored subscription and keeps the session: signing a user out over a stale
// push endpoint would be a bug.
var ErrGone = errors.New("push endpoint is gone")

// ErrVAPID means the relay rejected Blurt's VAPID identity. That is a
// deployment mistake, so nothing is pruned.
var ErrVAPID = errors.New("push relay rejected VAPID credentials")

// Keys is the browser's PushSubscription.toJSON().keys verbatim.
type Keys struct {
	P256DH string `json:"p256dh"`
	Auth   string `json:"auth"`
}

// Subscription is one device's subscription, stored on its session row.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     Keys   `json:"keys"`
}

// Message is one notification's content.
type Message struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Tag      string `json:"tag,omitempty"`
	Navigate string `json:"navigate,omitempty"`
	// TraceID is the firing attempt's Trace row, which the service worker
	// keeps for notification feedback. Never read from a request: it points
	// at a row the backend must own.
	TraceID int64 `json:"-"`
}

// payload is what actually crosses the wire. A zero TraceID is omitted,
// matching the worker's null default.
type payload struct {
	WebPush      int     `json:"web_push"`
	Notification Message `json:"notification"`
	TraceID      int64   `json:"notificationTraceId,omitempty"`
}

// Result counts one fan-out.
type Result struct {
	Sent   int `json:"sent"`
	Failed int `json:"failed"`
	Pruned int `json:"pruned"`
}

// Sender delivers one message to one device: it encrypts for the device's keys
// and signs with Blurt's VAPID identity.
type Sender struct {
	subscriber string
	publicKey  string
	privateKey string
}

// NewSender builds the transport. It takes config.VAPID because the private
// half is used nowhere else.
func NewSender(vapid config.VAPID) *Sender {
	return &Sender{
		subscriber: subscriberURI(vapid.Subject),
		publicKey:  vapid.PublicKey,
		privateKey: vapid.PrivateKey,
	}
}

// subscriberURI drops the "mailto:" prefix that webpush-go adds itself: it
// would otherwise sign "mailto:mailto:...", which Apple rejects as 403
// BadJwtToken. An https: contact URI is passed through untouched.
func subscriberURI(subject string) string {
	return strings.TrimPrefix(subject, "mailto:")
}

// Send encrypts the message for one device. subscription is the stored JSON,
// so a corrupt row fails here instead of being read as a blank endpoint.
func (s *Sender) Send(ctx context.Context, subscription []byte, msg Message) error {
	var sub Subscription
	if err := json.Unmarshal(subscription, &sub); err != nil {
		return fmt.Errorf("decode stored push subscription: %w", err)
	}
	body, err := json.Marshal(payload{WebPush: webPushVersion, Notification: msg, TraceID: msg.TraceID})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	status, backoff, err := s.post(ctx, body, sub)
	if err == nil && status == http.StatusTooManyRequests {
		if err := sleep(ctx, backoff); err != nil {
			return err
		}
		status, _, err = s.post(ctx, body, sub)
	}
	switch {
	case err != nil:
		return err
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusNotFound || status == http.StatusGone:
		return ErrGone
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: relay returned %d", ErrVAPID, status)
	default:
		return fmt.Errorf("push relay returned %d", status)
	}
}

// post sends one encrypted request. The body is never read, so it is closed
// here: a status code is all this needs, and an unread body would hold the
// connection open.
func (s *Sender) post(ctx context.Context, body []byte, sub Subscription) (int, time.Duration, error) {
	resp, err := webpush.SendNotificationWithContext(ctx, body, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{P256dh: sub.Keys.P256DH, Auth: sub.Keys.Auth},
	}, &webpush.Options{
		Subscriber:      s.subscriber,
		VAPIDPublicKey:  s.publicKey,
		VAPIDPrivateKey: s.privateKey,
		TTL:             ttlSeconds,
	})
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	var backoff time.Duration
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
		backoff = time.Duration(secs) * time.Second
	}
	return resp.StatusCode, backoff, nil
}

// sleep waits out a relay backoff, capped so one rate-limited device cannot
// stall the devices queued behind it.
func sleep(ctx context.Context, backoff time.Duration) error {
	if backoff <= 0 {
		return nil
	}
	timer := time.NewTimer(min(backoff, retryAfterCap))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ValidateSubscription rejects anything Blurt must not POST to. The endpoint
// arrives from an authenticated session, but it is still an SSRF surface, so
// the host must be a known push service. Loopback and the Docker host may use
// http so a local relay works in tests.
func ValidateSubscription(sub Subscription) error {
	endpoint, err := url.Parse(strings.TrimSpace(sub.Endpoint))
	if err != nil || endpoint.Hostname() == "" {
		return errors.New("Push endpoint must be an https URL.")
	}
	host := strings.ToLower(endpoint.Hostname())
	local := isLocalHost(host)
	if endpoint.Scheme != "https" && !(local && endpoint.Scheme == "http") {
		return errors.New("Push endpoint must be an https URL.")
	}
	if !local && !knownRelay(host) {
		return errors.New("Push endpoint is not a known push service.")
	}
	// Browsers mint both keys as unpadded base64url. ecdh is what proves the
	// point is real, on the curve, and the right shape.
	point, err := base64.RawURLEncoding.DecodeString(sub.Keys.P256DH)
	if err == nil {
		_, err = ecdh.P256().NewPublicKey(point)
	}
	if err != nil {
		return errors.New("Push subscription p256dh is not a valid P-256 point.")
	}
	if auth, err := base64.RawURLEncoding.DecodeString(sub.Keys.Auth); err != nil || len(auth) != authSecretLen {
		return errors.New("Push subscription auth secret must be 16 bytes.")
	}
	return nil
}

func isLocalHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "host.docker.internal":
		return true
	}
	return false
}

// knownRelay is the allowlist of services browsers actually subscribe with.
func knownRelay(host string) bool {
	for _, suffix := range []string{
		"googleapis.com", "push.apple.com", "mozilla.com", "mozaws.net", "notify.windows.com",
	} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}
