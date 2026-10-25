// Package failure classifies provider errors and carries the copy both processing
// workers use when a capture or plan cannot be processed at all.
package failure

import "errors"

// FallbackMessage is what the user reads when the work could not be done.
const FallbackMessage = "I couldn't process that. Please say it again or type it."

// retryable is the shape the gateway errors share without sharing a type.
type retryable interface {
	Retryable() bool
}

// Permanent reports whether a retry cannot fix the error: a bad key, an unknown
// model, or another refusal the next attempt would repeat.
func Permanent(err error) bool {
	var refusal retryable
	return errors.As(err, &refusal) && !refusal.Retryable()
}

// Outcome names a failure in the Trace.
func Outcome(err error) string {
	if Permanent(err) {
		return "rejected"
	}
	return "error"
}
