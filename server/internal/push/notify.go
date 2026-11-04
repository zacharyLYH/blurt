package push

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"blurt/internal/store"
)

// Notifier is the port the rest of Blurt talks to: one user, one message, every
// signed-in device. It lives beside the fan-out that implements it so the five
// callers cannot drift into five slightly different copies of one method, and a
// test double that satisfies it satisfies all of them.
type Notifier interface {
	NotifyUser(ctx context.Context, userID int64, msg Message) (Result, error)
}

// Fanout fans one message out to every device a user has subscribed. Best
// effort by design: a device that fails never stops the rest, and a dead
// endpoint only costs its own subscription.
type Fanout struct {
	Sessions *store.SessionStore
	Sender   *Sender
}

// NotifyUser sends to every subscribed device of this user and reports what
// happened. A VAPID rejection is returned after the loop because it is a
// deployment problem worth surfacing; single-device failures are counted and
// logged instead.
func (n *Fanout) NotifyUser(ctx context.Context, userID int64, msg Message) (Result, error) {
	devices, err := n.Sessions.ListPushSubscriptions(ctx, userID)
	if err != nil {
		return Result{}, err
	}

	var res Result
	var authErr error
	seen := make(map[string]struct{}, len(devices))
	for _, device := range devices {
		// A browser that lost its cookie and signed in again can leave two rows
		// pointing at the same endpoint. Notify once; the newest row wins because
		// the store lists it first.
		key := subscriptionKey(device.Subscription)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}

		switch err := n.Sender.Send(ctx, device.Subscription, msg); {
		case err == nil:
			res.Sent++
		case errors.Is(err, ErrGone):
			// Forget the endpoint, never the session: the cookie is still how
			// this device is signed in.
			res.Pruned++
			if err := n.Sessions.ClearPushSubscription(ctx, device.SessionID); err != nil {
				slog.Error("clear dead push subscription", "session_id", device.SessionID, "err", err)
			}
		default:
			res.Failed++
			if errors.Is(err, ErrVAPID) {
				authErr = err
			}
			slog.Warn("push delivery failed", "session_id", device.SessionID, "err", err)
		}
	}
	return res, authErr
}

// subscriptionKey identifies one device by its endpoint. A corrupt row falls
// back to its raw JSON, so the sender still reports the failure once instead of
// hiding it as a duplicate.
func subscriptionKey(raw []byte) string {
	var sub Subscription
	if err := json.Unmarshal(raw, &sub); err == nil && sub.Endpoint != "" {
		return sub.Endpoint
	}
	return string(raw)
}
