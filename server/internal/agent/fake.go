package agent

import (
	"context"
	"regexp"
	"strings"
)

// Fake is the dev model (BLURT_FAKE_AGENT): it keeps the AgentLoop exercisable
// end to end without a gateway, the way BLURT_FAKE_STT keeps the capture path
// exercisable without phonon. e2e runs the real stack on it.
type Fake struct{}

const (
	// FakeTimePhrase is what the fake proposes when a capture carries no time of
	// its own, so a spec can assert a resolved instant without modelling a model.
	FakeTimePhrase = "tomorrow at 6pm"
	// FakeMemoryWord picks the Memory branch: a capture that says it is a fact.
	FakeMemoryWord = "remember"
	// FakeAskWord picks the branch that must ask: a capture with no time words at
	// all, which the time lint refuses.
	FakeAskWord = "when"
	// FakeSplitQuestion is the scripted split ask: two clocks in one capture are
	// more independent items than the one-event shape can carry.
	FakeSplitQuestion = "I can only take one reminder at a time. Which one should I set first?"
)

// Propose is deliberately blunt and keyword-driven, so a spec can reach each branch
// on purpose. The real model reads the same three outcomes out of one schema.
func (Fake) Propose(_ context.Context, in Input) (Turn, error) {
	capture := lastText(in)
	lower := strings.ToLower(capture)
	switch {
	case clocksIn(lower) > 1:
		return Turn{Say: FakeSplitQuestion}, nil
	case strings.Contains(lower, FakeMemoryWord):
		return Turn{Say: "Noted.", Writes: []Write{{Kind: KindMemory, Text: capture}}}, nil
	case strings.Contains(lower, FakeAskWord):
		return Turn{Say: "When should I remind you?",
			Writes: []Write{{Kind: KindReminder, Text: capture, Timezone: in.CurrentTZ}}}, nil
	default:
		event := eventOf(capture)
		if event == "" {
			// A clarification answer is just a time; the event is in the capture
			// that started the Job.
			event = eventOf(firstUserText(in))
		}
		return Turn{Say: "Reminder set for tomorrow at 6pm.",
			Writes: []Write{{Kind: KindReminder, Text: event, Timezone: in.CurrentTZ,
				TimePhrase: FakeTimePhrase}}}, nil
	}
}

func lastText(in Input) string {
	if last := len(in.Conversation) - 1; last >= 0 {
		return in.Conversation[last].Text
	}
	return ""
}

func firstUserText(in Input) string {
	for _, turn := range in.Conversation {
		if turn.Role == "user" && turn.Text != "" {
			return turn.Text
		}
	}
	return ""
}

// Ask returns the fixed question rather than writing one, so a spec asserts one exact sentence and the
// deterministic stack stays deterministic.
func (Fake) Ask(_ context.Context, _ Input) (string, error) {
	return defaultQuestion, nil
}

// clocksIn counts the clock forms a capture names. Two of them is the split-ask
// branch, which is the scripted model's way into the guidance the real prompt
// carries.
func clocksIn(text string) int {
	return len(clockRe.FindAllString(text, -1)) + len(clock24Re.FindAllString(text, -1))
}

// timeWordRe is the date vocabulary the fake strips from an event so the row it
// commits matches the prompt's no-time-words contract.
var timeWordRe = regexp.MustCompile(`(?i)\b(day after tomorrow|today|tonight|tomorrow|next|this|` +
	`monday|tuesday|wednesday|thursday|friday|saturday|sunday|` +
	`january|february|march|april|may|june|july|august|september|october|november|december)\b`)

// eventOf is the capture with its clock and date words taken out, and any
// dangling filler trimmed. The prompt asks for the event with no time words, and
// the lints hold the scripted model to the same contract as the real one, so a
// capture "buy glue at 6pm tomorrow" commits as "buy glue" rather than
// "buy glue at tomorrow".
func eventOf(capture string) string {
	stripped := clock24Re.ReplaceAllString(clockRe.ReplaceAllString(capture, ""), "")
	stripped = timeWordRe.ReplaceAllString(stripped, "")
	words := strings.Fields(stripped)
	for len(words) > 0 && filler[strings.ToLower(strings.Trim(words[len(words)-1], ",."))] {
		words = words[:len(words)-1]
	}
	return strings.Join(words, " ")
}

var _ Model = Fake{}

// FakeTagger is the dev tagger (BLURT_FAKE_AGENT): the fact's own content words,
// which is enough for e2e to assert a tag is searchable and keeps the
// deterministic stack deterministic.
type FakeTagger struct{}

func (FakeTagger) Tags(_ context.Context, text string) ([]string, error) {
	return parseTags(strings.Join(contentWords(text), ",")), nil
}

var _ Tagger = FakeTagger{}
