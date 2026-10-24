package push

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"blurt/internal/store"
)

// Notifier fans one message out to every device a user is signed in on. Best
// effort by design: a device that fails never stops the rest, and a dead
// endpoint only costs its own subscription.
type Notifier struct {
	Sessions *store.SessionStore
	Sender   *Sender
}

// NotifyUser sends to every live session of this user and reports what
// happened. A VAPID rejection is returned after the loop because it is a
// deployment problem worth surfacing; single-device failures are counted and
// logged instead.
func (n *Notifier) NotifyUser(ctx context.Context, userID int64, msg Message) (Result, error) {
	devices, err := n.Sessions.ListPushSubscriptions(ctx, userID, time.Now())
	if err != nil {
		return Result{}, err
	}

	var res Result
	var authErr error
	for _, device := range devices {
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
