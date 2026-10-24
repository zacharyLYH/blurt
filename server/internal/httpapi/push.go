package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"blurt/internal/push"
	"blurt/internal/store"
)

// Notification limits keep one request from queueing a payload the relay
// would reject as too large anyway.
const (
	maxPushTitle = 100
	maxPushBody  = 500
	maxPushTag   = 64
)

// pushKey hands the browser the public half of the VAPID identity. The key is
// public by design, so this endpoint needs no session.
func (s *Server) pushKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"publicKey": s.VAPIDPublicKey})
}

// subscribePush stores the device's subscription on its session row. A
// re-subscribe replaces the old endpoint, matching browser rotation.
func (s *Server) subscribePush(w http.ResponseWriter, r *http.Request, _ store.User) {
	var body push.Subscription
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Endpoint = strings.TrimSpace(body.Endpoint)
	body.Keys.P256DH = strings.TrimSpace(body.Keys.P256DH)
	body.Keys.Auth = strings.TrimSpace(body.Keys.Auth)
	// The endpoint becomes a POST target later, so it is judged now: an
	// unknown relay or a malformed key is refused instead of stored broken.
	if err := push.ValidateSubscription(body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_subscription", err.Error())
		return
	}

	raw, ok := sessionToken(r)
	if !ok {
		unauthorized(w)
		return
	}
	stored, err := json.Marshal(body)
	if err != nil {
		s.fail(w, "encode push subscription", err)
		return
	}
	if err := s.Sessions.SetPushSubscription(r.Context(), hashToken(raw), stored); err != nil {
		s.fail(w, "store push subscription", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sendPush fans a notification out to every device the caller is signed in
// on. Deliberately scoped to the caller: blurt serves many users, so an open
// send would be a way to notify somebody else's phone. The TraceId is not
// accepted here either, since it points at a row the backend must own.
func (s *Server) sendPush(w http.ResponseWriter, r *http.Request, user store.User) {
	if s.Pusher == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Push sending is not configured.")
		return
	}
	var msg push.Message
	if !decodeJSON(w, r, &msg) {
		return
	}
	// TrimSpace so a blank title is a blank title, not a space.
	msg.Title, msg.Body, msg.Tag = strings.TrimSpace(msg.Title), strings.TrimSpace(msg.Body), strings.TrimSpace(msg.Tag)
	if msg.Title == "" || msg.Body == "" {
		writeError(w, http.StatusBadRequest, "invalid_notification", "A notification needs a title and a body.")
		return
	}
	if len(msg.Title) > maxPushTitle || len(msg.Body) > maxPushBody || len(msg.Tag) > maxPushTag {
		writeError(w, http.StatusBadRequest, "invalid_notification", "Notification is too long.")
		return
	}
	navigate, err := notificationTarget(r, strings.TrimSpace(msg.Navigate))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_navigate", err.Error())
		return
	}
	msg.Navigate = navigate

	res, err := s.Pusher.NotifyUser(r.Context(), user.UserID, msg)
	if errors.Is(err, push.ErrVAPID) {
		// The relay refused our identity: a deploy problem, not a dead device.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"sent": res.Sent, "failed": res.Failed, "pruned": res.Pruned,
			"code": "vapid_rejected", "error": "Push relay rejected Blurt's VAPID credentials.",
		})
		return
	}
	if err != nil {
		s.fail(w, "send push", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// notificationTarget defaults to the app home and refuses a foreign origin, so
// a crafted notification cannot open anything but Blurt. Host is the public
// origin because nginx forwards it, port included.
func notificationTarget(r *http.Request, raw string) (string, error) {
	scheme := "http"
	if usesHTTPS(r) {
		scheme = "https"
	}
	if raw == "" {
		return scheme + "://" + r.Host + "/home", nil
	}
	// A path is resolved against the worker's own origin, so it cannot point
	// anywhere else. "//host" is protocol-relative, therefore not a path.
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		return raw, nil
	}
	target, err := url.Parse(raw)
	if err != nil || target.Host == "" {
		return "", errors.New("Notification target must be a URL.")
	}
	if !strings.EqualFold(target.Scheme, scheme) || !strings.EqualFold(target.Host, r.Host) {
		return "", errors.New("Notification target must be same-origin.")
	}
	return raw, nil
}
