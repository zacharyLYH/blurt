package agent

import (
	"context"
	"log/slog"
	"strings"
)

// question is what the user reads. One decision-maker: the model's own sentence first, because a capture's
// edge case is exactly what one fixed sentence cannot cover, and a zero-writes split turn puts its question
// in say; then the writer when a refusal gives it something to ask about; then the fixed sentence.
func (r *run) question(ctx context.Context) string {
	if usableQuestion(r.turn.Say) {
		return r.turn.Say
	}
	if len(r.refused) == 0 {
		// Nothing was refused, so there is nothing for the writer to ask about: this is the capture that named nothing Blurt can act on.
		return defaultQuestion
	}
	written, err := r.loop.askModel(ctx, r.askInput())
	if err != nil {
		slog.Warn("agent question", "job_id", r.job.JobID, "err", err)
		return defaultQuestion
	}
	if !usableQuestion(written) {
		return defaultQuestion
	}
	return written
}

// usableQuestion reports whether a written question can be put to the user: one short sentence, ending
// in a question, naming no clock. The prompt forbids a time the user did not say, and a model handed
// the capture's own words gets that wrong in exactly that way.
func usableQuestion(question string) bool {
	return question != "" && len(question) <= 200 &&
		strings.HasSuffix(question, "?") && !namesClock(question)
}
