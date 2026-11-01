package agent

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"blurt/internal/store"
)

// Fake is the dev model (BLURT_FAKE_AGENT): it keeps the AgentLoop exercisable
// end to end without a gateway, the way BLURT_FAKE_STT keeps the capture path
// exercisable without phonon. e2e runs the real stack on it.
type Fake struct{}

const (
	// FakeMemoryWord picks the Memory branch: a capture that says it is a fact.
	FakeMemoryWord = "remember"
	// FakeRepeatWord marks a capture that repeats, and the frequency is read off the
	// word beside it: "every day", "daily", "every week" and so on. A capture that says
	// it repeats without naming how often gets the ask rather than a one-off.
	FakeRepeatWord = "every"
	// FakeSoonWord marks a capture the fake resolves to the minute ahead: near enough
	// that a spec can watch the emitter fire it, and still a future instant, which is
	// what lint's after-now check on a one-off requires.
	FakeSoonWord = "soon"
)

// fakeSoonRe is the word "soon" on its own, so a capture that merely contains the
// letters — "buy glue soonest" — is not read as one.
var fakeSoonRe = regexp.MustCompile(`(?i)\bsoon\b`)

// fakeDateRe is a pinned calendar date: a capture can name "2030-03-05" and the
// fake resolves that day rather than a relative one. It is what lets an e2e spec
// place a Reminder on a fixed day without waiting for the calendar to reach it.
var fakeDateRe = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})\b`)

// fakeNotifyRe is the clause that names when to be notified: "remind me at 6pm"
// or "remind me soon". It is what tells a capture that names two clocks which one is
// the notification time; the other clock belongs to the event and rides in text.
var fakeNotifyRe = regexp.MustCompile(`(?i)\bremind me (?:at )?(soon|[0-9]{1,2}(?::[0-9]{2})?\s*(?:am|pm)|[0-9]{1,2}:[0-9]{2})\b`)

// fakeRememberRe is the word that named a capture as a fact. The schema asks the real model
// for the user's words without it, so the fake strips it too.
var fakeRememberRe = regexp.MustCompile(`(?i)^\s*remember(?:\s+that)?\s+`)

// fakeToDoRe marks "remember to <do something>", which is a reminder the user wants set
// rather than a fact to keep.
var fakeToDoRe = regexp.MustCompile(`(?i)^\s*remember\s+to\b`)

// fakeIntentRe is the phrase that says what the user wants done rather than what the event
// is: "remind me to call mum" is the event "call mum", the shape the prompt asks for.
var fakeIntentRe = regexp.MustCompile(`(?i)^\s*(?:remind me|remember)\b\s*(?:(?:to|about|that)\b\s*)?`)

// Prescreen is the dev screen: it lets every capture through. A spec that needs a
// refusal scripts its own double, the way the flow tests already do.
func (Fake) Prescreen(_ context.Context, _ string) (Verdict, error) {
	return Verdict{Outcome: verdictOK}, nil
}

// Chat is the dev agent turn. The fake never looks anything up — it reads the capture
// itself — so it asks to submit at once; the loop, the linter and the strict call still run
// for real around it.
func (Fake) Chat(_ context.Context, _ PlannerInput, _ []Message, _ []Tool) (Message, error) {
	return submitCall(), nil
}

// Finalize is deliberately blunt and keyword-driven, so a spec can reach each branch on
// purpose. The real model reads the same outcomes out of one schema.
func (Fake) Finalize(_ context.Context, in PlannerInput, _ []Message) (Answer, error) {
	capture := strings.ToLower(lastText(in))
	if fact, ok := fakeFact(lastText(in)); ok {
		// A fact is context rather than an interruption, so it carries no clock.
		return placed(Answer{Memories: []string{fact}}), nil
	}
	if record, ok := fakeNotifyAndEvent(in, capture); ok && fakeAhead(in, record) {
		// The capture names both clocks: the notification time and the event's own.
		return placed(Answer{Ops: []Op{{Intent: IntentCreate, Record: record}}}), nil
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
			return asked(defaultQuestion), nil
		}
		at := in.Now.In(loc).Add(time.Minute)
		return placed(Answer{Ops: []Op{{Intent: IntentCreate, Record: Record{
			Kind: KindReminder, Text: text, Zone: in.CurrentTZ,
			Date: at.Format("2006-01-02"), Time: at.Format("15:04"),
			Recurrence: RecurrenceNone,
		}}}}), nil
	}
	if !namesClock(capture) || clocksIn(capture) > 1 {
		// No clock is a time the fake cannot pin, and two of them are more than one
		// reminder can carry. Either way it proposes nothing and the Job asks — the
		// same ask the real model's empty field earns.
		return asked(defaultQuestion), nil
	}
	clock, ok := fakeSingleClock(capture)
	if !ok {
		// A clock the fake cannot read is a time it cannot pin, so it asks rather than
		// substituting a clock of its own.
		return asked(defaultQuestion), nil
	}
	text := eventOf(firstUserText(in))
	if text == "" {
		return asked(defaultQuestion), nil
	}
	recurrence, repeats := fakeRecurrence(capture)
	if strings.Contains(capture, FakeRepeatWord) && !repeats {
		// The capture says it repeats without naming how often, and the fake cannot
		// place a pattern it cannot read: it proposes nothing and the Job asks, the
		// same ask an unreadable clock earns.
		return asked(defaultQuestion), nil
	}
	loc := time.UTC
	if loaded, err := time.LoadLocation(in.CurrentTZ); err == nil {
		loc = loaded
	}
	day := in.Now.In(loc)
	if pinned, ok := fakeDate(capture, loc); ok {
		day = pinned
	} else if strings.Contains(capture, "tomorrow") {
		day = day.AddDate(0, 0, 1)
	}
	record := fakeRecord(in, text, day.Format("2006-01-02"), clock, recurrence, capture)
	if !fakeAhead(in, record) {
		// A one-off the clock has already passed is not a time the fake can place.
		return asked(defaultQuestion), nil
	}
	return placed(Answer{Ops: []Op{{Intent: IntentCreate, Record: record}}}), nil
}

// fakeNotifyAndEvent splits a capture that names both a notification time and an
// event time, the way the prompt does: time is when Blurt rings, and the event's own
// clock stays in text ("Dinner at 8pm"). A capture with two clocks but no notification
// clause is two events rather than this shape, so it is left to the ordinary ask.
func fakeNotifyAndEvent(in PlannerInput, capture string) (Record, bool) {
	match := fakeNotifyRe.FindStringSubmatch(capture)
	if match == nil {
		return Record{}, false
	}
	event := fakeNotifyRe.ReplaceAllString(lastText(in), "")
	if !namesClock(event) {
		return Record{}, false
	}

	loc := time.UTC
	if loaded, err := time.LoadLocation(in.CurrentTZ); err == nil {
		loc = loaded
	}
	now := in.Now.In(loc)
	date, clock := now, match[1]
	if clock == "soon" {
		at := now.Add(time.Minute)
		date, clock = at, at.Format("15:04")
	} else {
		hour, minute, ok := fakeClock(clock)
		if !ok {
			return Record{}, false
		}
		clock = fmt.Sprintf("%02d:%02d", hour, minute)
		if strings.Contains(capture, "tomorrow") {
			date = date.AddDate(0, 0, 1)
		}
	}

	recurrence, _ := fakeRecurrence(capture)
	text := eventOfKeepingClock(event)
	if text == "" {
		return Record{}, false
	}
	return fakeRecord(in, text, date.Format("2006-01-02"), clock, recurrence, capture), true
}

// fakeRecord is the one reminder shape every branch proposes: the device zone's
// fields, a defaulting recurrence, and — because a daily rule is its clock and
// nothing else — no calendar day.
// fakeFact is the fact a capture states, without the word that named it as one, and whether
// it states a fact at all: "remember <something>" is a fact, while "remember to <do
// something>" is a reminder the user wants set.
func fakeFact(capture string) (string, bool) {
	lowered := strings.ToLower(capture)
	if !strings.Contains(lowered, FakeMemoryWord) || fakeToDoRe.MatchString(lowered) {
		return "", false
	}
	fact := strings.TrimSpace(fakeRememberRe.ReplaceAllString(capture, ""))
	return fact, fact != ""
}

// fakeSingleClock is the one clock a capture names, as HH:MM: the notification time the user
// said rather than a clock of the fake's own. The forms are the ones the notify clause reads,
// so a one-clock capture and a two-clock one agree about how a clock is written.
func fakeSingleClock(capture string) (string, bool) {
	for _, re := range []*regexp.Regexp{clock24Re, clockRe} {
		raw := re.FindString(capture)
		if raw == "" {
			continue
		}
		hour, minute, ok := fakeClock(raw)
		if !ok {
			return "", false
		}
		return fmt.Sprintf("%02d:%02d", hour, minute), true
	}
	return "", false
}

// fakeAhead reports whether a record the fake proposed is one the server would still commit:
// it asks the server's own after-now check rather than keeping a second copy of it.
func fakeAhead(in PlannerInput, record Record) bool {
	return len(checkAfterNow(record, in.Now)) == 0
}

func fakeRecord(in PlannerInput, text, day, clock, recurrence, capture string) Record {
	if recurrence == "" {
		recurrence = RecurrenceNone
	}
	if recurrence == store.FrequencyDaily {
		day = ""
	}
	return Record{
		Kind: KindReminder, Text: text, Zone: in.CurrentTZ,
		Date: day, Time: clock, Recurrence: recurrence,
	}
}

// fakeClock reads the notification clock the fake's own regex found as HH:MM.
// fakeDate is the one pinned date a capture names, read in the zone the device gave
// so the day is the user's own.
func fakeDate(capture string, loc *time.Location) (time.Time, bool) {
	match := fakeDateRe.FindStringSubmatch(capture)
	if match == nil {
		return time.Time{}, false
	}
	at, err := time.ParseInLocation("2006-01-02", match[1], loc)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

func fakeClock(raw string) (int, int, bool) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if m := clock24Re.FindStringSubmatch(raw); m != nil {
		hour, _ := strconv.Atoi(m[1])
		minute, _ := strconv.Atoi(m[2])
		if hour > 23 || minute > 59 {
			return 0, 0, false
		}
		return hour, minute, true
	}
	m := clockRe.FindStringSubmatch(raw)
	if m == nil {
		return 0, 0, false
	}
	hour, _ := strconv.Atoi(m[1])
	minute := 0
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2])
	}
	if hour < 1 || hour > 12 || minute > 59 {
		return 0, 0, false
	}
	if m[3] == "pm" && hour != 12 {
		hour += 12
	}
	if m[3] == "am" && hour == 12 {
		hour = 0
	}
	return hour, minute, true
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

// placed marks a turn the fake could place, and asked one it could not. The
// deterministic stack asks the fixed sentence rather than writing one, so a spec
// asserts one exact sentence.
func placed(turn Answer) Answer {
	turn.Kind = AnswerRecords
	return turn
}

// asked is the answer that asks rather than commits: the one kind the linter leaves
// alone, because asking is the point.
func asked(question string) Answer {
	return Answer{Kind: AnswerQuestion, Question: question}
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

// submitCall is the tool call that ends the loop, as a dev model or a test double asks for it.
func submitCall() Message {
	return Message{Role: "assistant", ToolCalls: []ToolCall{{
		ID: "submit", Type: functionType,
		Function: CallFunction{Name: toolSubmitPlan, Arguments: "{}"},
	}}}
}

func lastText(in PlannerInput) string {
	if last := len(in.Conversation) - 1; last >= 0 {
		return in.Conversation[last].Text
	}
	return ""
}

func firstUserText(in PlannerInput) string {
	for _, turn := range in.Conversation {
		if turn.Role == "user" && turn.Text != "" {
			return turn.Text
		}
	}
	return ""
}

// clocksIn counts the clock forms a capture names. Two of them is more than one
// record can carry.
func clocksIn(text string) int {
	return len(clockRe.FindAllString(text, -1)) + len(clock24Re.FindAllString(text, -1))
}

// timeWordRe is the date vocabulary the fake strips from an event so the row it
// commits matches the prompt's no-time-words contract.
var timeWordRe = regexp.MustCompile(`(?i)\b(soon|day after tomorrow|today|tonight|tomorrow|` +
	`\d{4}-\d{2}-\d{2}|every day|every week|every month|every year|every|daily|weekly|monthly|yearly|next|this|` +
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
// filler trimmed. The prompt asks for the event with no time words when the notify
// time is the event time, and the lints hold the scripted model to the same contract
// as the real one, so a capture "buy glue at 6pm tomorrow" commits as "buy glue"
// rather than "buy glue at tomorrow".
func eventOf(capture string) string {
	stripped := clock24Re.ReplaceAllString(clockRe.ReplaceAllString(capture, ""), "")
	return eventOfKeepingClock(fakeIntentRe.ReplaceAllString(stripped, ""))
}

// eventOfKeepingClock is the capture with its date words taken out and any dangling
// filler trimmed, but its clock left in: when the notification time differs, the
// event's own clock is what tells the user what the reminder is for.
func eventOfKeepingClock(capture string) string {
	stripped := timeWordRe.ReplaceAllString(capture, "")
	words := strings.Fields(stripped)
	for len(words) > 0 && !eventWord(words[len(words)-1]) {
		words = words[:len(words)-1]
	}
	for len(words) > 0 && !eventWord(words[0]) {
		words = words[1:]
	}
	return strings.Join(words, " ")
}

// eventWord reports whether a word belongs to the event rather than being filler or
// punctuation left behind by the words stripped around it.
func eventWord(word string) bool {
	word = strings.ToLower(strings.Trim(word, ",."))
	return word != "" && !eventFiller[word]
}

var _ Model = Fake{}
