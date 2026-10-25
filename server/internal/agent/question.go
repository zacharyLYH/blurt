package agent

import (
	"context"
	"log/slog"
	"strings"
)

// questionText is what the user reads. One decision-maker: the model writes it when it can, naming the
// fields MissingFields could not accept, and the fixed sentence stands in when it cannot.
func (r *run) questionText(ctx context.Context) string {
	if r.clarityRounds() >= maxClarityRounds {
		// The model has had its rounds; another call would only repeat itself.
		return defaultQuestion
	}
	written, err := r.loop.questionModel(ctx, r.askInput(), r.missing)
	if err != nil {
		slog.Warn("agent question", "job_id", r.job.JobID, "err", err)
		return defaultQuestion
	}
	if !usableQuestion(written) {
		return defaultQuestion
	}
	return written
}

// clarityRounds is how many questions this Job has already asked: the rounds an
// answer has not yet closed.
func (r *run) clarityRounds() int {
	rounds := 0
	for _, turn := range r.job.Conversation {
		if turn.Role == "assistant" {
			rounds++
		}
	}
	return rounds
}

// usableQuestion reports whether a written question can be put to the user: one short sentence, ending
// in a question, naming no clock, and carrying no placeholder Blurt leaked into the prompt. A model
// handed the capture's own words gets the first of those wrong in exactly that way.
func usableQuestion(question string) bool {
	return question != "" && len(question) <= 200 &&
		strings.HasSuffix(question, "?") && !namesClock(question) &&
		!strings.Contains(question, "<unknown>") && !strings.Contains(question, "<nothing>")
}
