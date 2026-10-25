package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"blurt/internal/decision"
)

// Verdict is what the fallback reader decided about one write.
type Verdict string

const (
	// VerdictSupported means the reader says the record is what the user asked for. It is the only
	// verdict that lets a write past a lint that refused it.
	VerdictSupported Verdict = "supported"
	// VerdictContradicted means the reader says the record is not, or is more than, what the user asked
	// for: the one refusal no further model turn can settle.
	VerdictContradicted Verdict = "contradicted"
	// VerdictUnknown means the reader could not tell, which is also what it reports when it is not
	// configured or could not be reached.
	VerdictUnknown Verdict = "unknown"
)

// Reading is a verdict with the confidence it was reached at: the number is what tells a decisive answer
// from a flat one, and it rides in the Trace so a classifier that keeps abstaining is visible rather than
// anecdotal.
type Reading struct {
	Verdict    Verdict
	Confidence float64
}

// Fallback is the reader a lint hands a write to when it cannot resolve one: one model call about one
// write, asked only after a deterministic check has abstained. An interface so the loop's policy is
// testable without a classifier, and nil is valid — with no fallback, the lints are the whole of
// PostAgentCheck.
type Fallback interface {
	Resolve(ctx context.Context, said string, write Write) (Reading, error)
}

// fallbackQuestion names the one decision Blurt asks laya for in the loop.
const fallbackQuestion = "record"

// floor is the probability a verdict needs before it is acted on, and ahead is how many times the
// runner-up it has to beat. Both exist so a classifier that splits its probability between criteria
// abstains rather than deciding: it can catch a confident contradiction, and it can never confirm a
// record on its own.
const (
	floor = 0.7
	ahead = 2
)

// stateChars caps the state handed to laya from the front, keeping the newest turns. laya's window is
// 512 tokens and its overflow is silent, so an over-long state would push the user's own words out
// while the criteria survived — a verdict about a request the classifier never read.
const stateChars = 700

// Laya is the fallback: the gateway's laya classifier behind the Fallback port, asked one choice
// question with three criteria.
type Laya struct {
	Decider decision.Decider
}

func (l Laya) Resolve(ctx context.Context, said string, write Write) (Reading, error) {
	answers, err := l.Decider.Decide(ctx, fallbackState(said, write), decision.Questions{
		fallbackQuestion: {
			Kind:         decision.Choice,
			Instructions: "Does the record match what the user asked for?",
			Criteria: map[string]string{
				string(VerdictSupported):    "the record says what the user asked, in their words",
				string(VerdictContradicted): "the record says something else, or more than the user asked",
				string(VerdictUnknown):      "the request is too unclear to tell",
			},
		},
	})
	if err != nil {
		return Reading{Verdict: VerdictUnknown}, err
	}
	answer, ok := answers[fallbackQuestion]
	if !ok {
		return Reading{Verdict: VerdictUnknown}, errors.New("fallback: laya answered nothing")
	}
	confidence := top(answer.Probabilities)
	if !decisive(confidence, answer.Probabilities) {
		return Reading{Verdict: VerdictUnknown, Confidence: confidence}, nil
	}
	return Reading{Verdict: Verdict(answer.Choice), Confidence: confidence}, nil
}

// fallbackState is what the reader is handed: the user's own words and the record. The model's
// explanation is deliberately absent, because the question is whether the record matches the request,
// not whether the model's story about it is plausible. A Reminder's phrase rides in parentheses: a
// verdict about a time needs the time words.
func fallbackState(said string, write Write) string {
	if len(said) > stateChars {
		said = said[len(said)-stateChars:]
	}
	record := write.Text
	if write.TimePhrase != "" {
		record += " (" + write.TimePhrase + ")"
	}
	return fmt.Sprintf("the user said: %s\nthe record: %s", strings.TrimSpace(said), record)
}

// decisive reports whether the leading probability is far enough ahead of the next one to be acted on.
func decisive(leading float64, probabilities map[string]float64) bool {
	if leading < floor {
		return false
	}
	next := 0.0
	for _, probability := range probabilities {
		if probability < leading && probability > next {
			next = probability
		}
	}
	return leading >= ahead*next
}

// top is the largest probability in the answer.
func top(probabilities map[string]float64) float64 {
	best := 0.0
	for _, probability := range probabilities {
		if probability > best {
			best = probability
		}
	}
	return best
}
