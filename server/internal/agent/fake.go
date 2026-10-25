package agent

import (
	"context"
	"regexp"
	"strings"
	"time"

	"blurt/internal/store"
)

// Fake is the dev model (BLURT_FAKE_AGENT): it keeps the AgentLoop exercisable
// end to end without a gateway, the way BLURT_FAKE_STT keeps the capture path
// exercisable without phonon. e2e runs the real stack on it.
type Fake struct{}

const (
	// FakeTime is the clock the fake reads out of a capture that carries one, so a
	// spec can assert a resolved instant without modelling a model.
	FakeTime = "18:00"
	// FakeMemoryWord picks the Memory branch: a capture that says it is a fact.
	FakeMemoryWord = "remember"
	// FakeRepeatWord marks a capture that repeats, and the frequency is read off the
	// word beside it: "every day", "daily", "every week" and so on. A capture that says
	// it repeats without naming how often gets the ask rather than a one-off.
	FakeRepeatWord = "every"
	// FakeSoonWord marks a capture the fake resolves to the minute ahead: near enough
	// that a spec can watch the emitter fire it, and still a future instant, which is
	// what the check on a one-off requires.
	FakeSoonWord = "soon"
)

// fakeSoonRe is the word "soon" on its own, so a capture that merely contains the
// letters — "buy glue soonest" — is not read as one.
var fakeSoonRe = regexp.MustCompile(`(?i)\bsoon\b`)

// Propose is deliberately blunt and keyword-driven, so a spec can reach each branch
// on purpose. The real model reads the same outcomes out of one schema.
func (Fake) Propose(_ context.Context, in Input) (Turn, error) {
	capture := strings.ToLower(lastText(in))
	if strings.Contains(capture, FakeMemoryWord) {
		// A fact is context rather than an interruption, so it carries no clock.
		return Turn{Writes: []Write{{Kind: KindMemory, Text: lastText(in)}}}, nil
	}
	if fakeSoonRe.MatchString(capture) && !namesClock(capture) {
		// The minute ahead, in the zone the device named: a clock the fake can place
		// without a model, and an instant that is still ahead of the run that resolved
		// it. Nothing else in the thread identifies it as special, which is the point —
		// the spec watches the real emitter fire an ordinary one-off.
		loc := time.UTC
		if loaded, err := time.LoadLocation(in.CurrentTZ); err == nil {
			loc = loaded
		}
		text := eventOf(firstUserText(in))
		if text == "" {
			return Turn{}, nil
		}
		at := in.Now.In(loc).Add(time.Minute)
		return Turn{Writes: []Write{{
			Kind: KindReminder, Text: text, Zone: in.CurrentTZ,
			Date: at.Format("2006-01-02"), Time: at.Format("15:04"),
			Recurrence: RecurrenceNone, Evidence: "the user said " + capture,
		}}}, nil
	}
	if !namesClock(capture) || clocksIn(capture) > 1 {
		// No clock is a time the fake cannot pin, and two of them are more than one
		// reminder can carry. Either way it proposes nothing and the Job asks — the
		// same ask the real model's empty field earns.
		return Turn{}, nil
	}
	text := eventOf(firstUserText(in))
	if text == "" {
		return Turn{}, nil
	}
	recurrence, repeats := fakeRecurrence(capture)
	if strings.Contains(capture, FakeRepeatWord) && !repeats {
		// The capture says it repeats without naming how often, and the fake cannot
		// place a pattern it cannot read: it proposes nothing and the Job asks, the
		// same ask an unreadable clock earns.
		return Turn{}, nil
	}
	date := in.Now
	if loc, err := time.LoadLocation(in.CurrentTZ); err == nil {
		date = date.In(loc)
	}
	if strings.Contains(capture, "tomorrow") {
		date = date.AddDate(0, 0, 1)
	}
	day := date.Format("2006-01-02")
	if recurrence == store.FrequencyDaily {
		// A daily rule is its clock and nothing else, so the fake leaves the day empty
		// rather than pinning the series to today.
		day = ""
	}
	if recurrence == "" {
		recurrence = RecurrenceNone
	}
	return Turn{Writes: []Write{{
		Kind: KindReminder, Text: text, Zone: in.CurrentTZ,
		Date: day, Time: FakeTime,
		Recurrence: recurrence, Evidence: "the user said " + capture,
	}}}, nil
}

// fakeRecurrence reads how often a capture repeats off its own word, the way the rest of the
// fake reads a clock out of it. It is blunt on purpose — a weekly, monthly or yearly rule is
// anchored on the fake's own date rather than on a day the capture names — so a spec reaches
// each pattern by saying it and asserts the flow rather than the calendar.
func fakeRecurrence(capture string) (string, bool) {
	for _, pattern := range []struct{ word, frequency string }{
		{"daily", store.FrequencyDaily}, {"every day", store.FrequencyDaily},
		{"weekly", store.FrequencyWeekly}, {"every week", store.FrequencyWeekly},
		{"monthly", store.FrequencyMonthly}, {"every month", store.FrequencyMonthly},
		{"yearly", store.FrequencyYearly}, {"every year", store.FrequencyYearly},
	} {
		if strings.Contains(capture, pattern.word) {
			return pattern.frequency, true
		}
	}
	return "", false
}

// Question answers the fixed sentence rather than writing one, so a spec asserts one exact sentence and
// the deterministic stack stays deterministic.
func (Fake) Question(_ context.Context, _ Input, _ []Field) (string, error) {
	return defaultQuestion, nil
}

// nonWord is what a tag is not: one tag is one word, so everything else is a separator.
var nonWord = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// Tags is the dev tagger: the fact's own words, minus the filler a sentence trails with. It is
// enough for a spec to assert a tag is searchable, and it keeps the deterministic stack
// deterministic.
func (Fake) Tags(_ context.Context, text string) ([]string, error) {
	kept := []string{}
	for _, word := range strings.Fields(nonWord.ReplaceAllString(text, " ")) {
		if !eventFiller[strings.ToLower(word)] {
			kept = append(kept, word)
		}
	}
	return parseTags(strings.Join(kept, ",")), nil
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

// clocksIn counts the clock forms a capture names. Two of them is more than one
// write can carry.
func clocksIn(text string) int {
	return len(clockRe.FindAllString(text, -1)) + len(clock24Re.FindAllString(text, -1))
}

// timeWordRe is the date vocabulary the fake strips from an event so the row it
// commits matches the prompt's no-time-words contract.
var timeWordRe = regexp.MustCompile(`(?i)\b(soon|day after tomorrow|today|tonight|tomorrow|` +
	`every day|every week|every month|every year|every|daily|weekly|monthly|yearly|next|this|` +
	`monday|tuesday|wednesday|thursday|friday|saturday|sunday|` +
	`january|february|march|april|may|june|july|august|september|october|november|december)\b`)

// eventFiller is the vocabulary a dev capture trails with once its clock and date
// words are gone: "buy glue at 6pm tomorrow" has to come back as "buy glue".
var eventFiller = map[string]bool{
	"a": true, "an": true, "and": true, "at": true, "for": true, "i": true,
	"is": true, "it": true, "me": true, "my": true, "of": true, "on": true,
	"the": true, "to": true,
}

// eventOf is the capture with its clock and date words taken out, and any dangling
// filler trimmed. The prompt asks for the event with no time words, and the lints
// hold the scripted model to the same contract as the real one, so a capture "buy
// glue at 6pm tomorrow" commits as "buy glue" rather than "buy glue at tomorrow".
func eventOf(capture string) string {
	stripped := clock24Re.ReplaceAllString(clockRe.ReplaceAllString(capture, ""), "")
	stripped = timeWordRe.ReplaceAllString(stripped, "")
	words := strings.Fields(stripped)
	for len(words) > 0 && eventFiller[strings.ToLower(strings.Trim(words[len(words)-1], ",."))] {
		words = words[:len(words)-1]
	}
	return strings.Join(words, " ")
}

var _ Model = Fake{}
