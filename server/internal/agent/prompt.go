package agent

import (
	"fmt"
	"strings"
	"time"

	"blurt/internal/store"
)

// systemPrompt is the record shape Blurt asks one model for: the fields it is sure
// of, and empty strings for the rest. Which of those the server can commit, and
// which the user has to answer for, is the server's decision and not the model's.
//
//   - Nothing is invented. A model told to fill a declared field will fill it:
//     asked for a date for "when I'm home from school" it returned one. Every field
//     here may be empty, and the schema says so, because an empty field is a
//     question rather than a wrong fire time.
//   - recurrence is the pattern beside the clock, never the time of day, so a
//     repeat can never be committed as a one-off.
//   - memoryText is the second record kind and carries neither a zone nor a time:
//     a fact is context, not an interruption, so the fields that make a Reminder
//     are left empty beside it.
//   - evidence is the user's own words. It is the one prose field, and it is what a
//     reader checks the answer against.
const systemPrompt = `You turn one spoken capture into records for Blurt.
text: the event to remind the user about, short, with no time words. Empty if there is nothing to remind about.
memoryText: a fact worth keeping for later, in the user's own words. Empty if the capture carries no such fact.
zone: IANA zone. Copy current_tz unless the capture names another.
date: the day, as YYYY-MM-DD. Empty if the capture names no day you are sure of.
time: the clock time, 24-hour HH:MM. Empty if the capture names no time you are sure of.
recurrence: the repeat pattern: none, daily, weekly, monthly or yearly. none when the reminder happens once.
evidence: the user's exact words for the time and the zone, and the zone you copied.

Fill a field only when you are certain of it. Leave it empty rather than guess: "when I'm home" and "next week" name no day or time anyone can pin.
The time fields belong to the reminder alone. A memoryText carries no zone, no date and no time.
A recurring reminder has no fixed day: fill recurrence and time, and leave date empty unless the capture names the day the series starts on.
A weekly, monthly or yearly reminder still needs the day it lands on, as date.

Example
current_tz: Asia/Singapore
now_local: 2026-10-24T18:30
capture: remind me to call mum tomorrow at 7pm
->
text: Call mum
memoryText:
zone: Asia/Singapore
date: 2026-10-25
time: 19:00
recurrence: none
evidence: the user said "tomorrow at 7pm"; current_tz is Asia/Singapore

Example
capture: remember that milk is cheaper at store A than store B
->
text:
memoryText: Milk is cheaper at store A than store B
zone: Asia/Singapore
date:
time:
recurrence: none
evidence: the capture names no time; current_tz is Asia/Singapore

Example
capture: remind me to water the plants every morning at 8am
->
text: Water the plants
memoryText:
zone: Asia/Singapore
date:
time: 08:00
recurrence: daily
evidence: the user said "every morning at 8am"

Example
capture: remind me to take the trash out every Tuesday at 7pm
->
text: Take the trash out
memoryText:
zone: Asia/Singapore
date: 2026-10-27
time: 19:00
recurrence: weekly
evidence: the user said "every Tuesday at 7pm"

Example
capture: lunch at Ikea this Friday at 1pm, and I have an Ikea voucher to use by December
->
text: Lunch at Ikea
memoryText: I have an Ikea voucher to use by December
zone: Asia/Singapore
date: 2026-10-30
time: 13:00
recurrence: none
evidence: the user said "this Friday at 1pm"; current_tz is Asia/Singapore`

// recordsSchema is the shape the answer is forced into, as the gateway's own
// JSON-schema response format rather than OpenAI's tool calling. Every field is a
// string, the two the model resolves carry a pattern, recurrence is an enum, and
// an empty string is a legal answer everywhere it matters — which is what makes
// "I am not sure" expressible at all.
const recordsSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "text":       {"type": "string"},
    "memoryText": {"type": "string"},
    "zone":       {"type": "string"},
    "date":       {"type": "string", "pattern": "^\\d{4}-\\d{2}-\\d{2}$|^$"},
    "time":       {"type": "string", "pattern": "^([01]\\d|2[0-3]):[0-5]\\d$|^$"},
    "recurrence": {"type": "string", "enum": ["none", "daily", "weekly", "monthly", "yearly"]},
    "evidence":   {"type": "string"}
  },
  "required": ["text", "memoryText", "zone", "date", "time", "recurrence", "evidence"]
}`

// records is the schema's shape on the Go side.
type records struct {
	Text       string `json:"text"`
	MemoryText string `json:"memoryText"`
	Zone       string `json:"zone"`
	Date       string `json:"date"`
	Time       string `json:"time"`
	Recurrence string `json:"recurrence"`
	Evidence   string `json:"evidence"`
}

// question is the question schema's shape on the Go side.
type question struct {
	Question string `json:"question"`
}

// turnOf reads the records into a Turn. Which fields the model filled is what the
// server commits: a filled text proposed a Reminder, a filled memoryText proposed a
// Memory, both is a capture that carried an event and a fact, and neither is the Job
// asking the user.
func turnOf(parsed records) Turn {
	turn := Turn{}
	if text := strings.TrimSpace(parsed.Text); text != "" {
		recurrence := strings.TrimSpace(parsed.Recurrence)
		if recurrence == "" {
			recurrence = RecurrenceNone
		}
		turn.Writes = append(turn.Writes, Write{
			Kind:       KindReminder,
			Text:       text,
			Zone:       strings.TrimSpace(parsed.Zone),
			Date:       strings.TrimSpace(parsed.Date),
			Time:       strings.TrimSpace(parsed.Time),
			Recurrence: recurrence,
			Evidence:   strings.TrimSpace(parsed.Evidence),
		})
	}
	if text := strings.TrimSpace(parsed.MemoryText); text != "" {
		turn.Writes = append(turn.Writes, Write{Kind: KindMemory, Text: text})
	}
	return turn
}

// messages builds the one prompt the gateway sees, over whichever system prompt is asking.
// Everything the user said before this turn is history, and the newest turn is rendered as the
// capture with the context block above it.
func messages(system string, in Input) []wireMessage {
	wire := []wireMessage{{Role: "system", Content: system}}
	var capture string
	if last := len(in.Conversation) - 1; last >= 0 {
		for _, turn := range in.Conversation[:last] {
			if turn.Text == "" || (turn.Role != "user" && turn.Role != "assistant") {
				continue
			}
			wire = append(wire, wireMessage{Role: turn.Role, Content: turn.Text})
		}
		capture = in.Conversation[last].Text
	}
	return append(wire, wireMessage{Role: "user", Content: contextBlock(in, capture)})
}

// contextBlock is what the model reads alongside the capture: the anchor zone and
// its local clock, whatever retrieval found, and the fields the question is about.
// The capture goes last so it is the freshest thing in the prompt.
func contextBlock(in Input, capture string) string {
	var block strings.Builder
	zone := time.UTC
	if loc, err := time.LoadLocation(in.CurrentTZ); err == nil {
		zone = loc
	}
	fmt.Fprintf(&block, "current_tz: %s\nnow_local: %s\n",
		in.CurrentTZ, in.Now.In(zone).Format("2006-01-02T15:04"))
	if len(in.Nearby) > 0 {
		block.WriteString("nearby reminders:\n")
		for _, reminder := range in.Nearby {
			fmt.Fprintf(&block, "- %s (%s)\n", reminder.Text, whenOf(reminder))
		}
	}
	if len(in.Remembered) > 0 {
		block.WriteString("remembered:\n")
		for _, memory := range in.Remembered {
			fmt.Fprintf(&block, "- %s\n", memory.Text)
		}
	}
	if len(in.Missing) > 0 {
		block.WriteString("missing: ")
		for i, field := range in.Missing {
			if i > 0 {
				block.WriteString(", ")
			}
			block.WriteString(string(field))
		}
		block.WriteString("\n")
	}
	fmt.Fprintf(&block, "capture: %s", capture)
	return block.String()
}

// whenOf is the time one retrieved Reminder is about, as the model reads it: the local
// time the model resolved for a one-off, and for a repeat the next occurrence its rule
// derives, in the record's own zone, with the pattern named beside it. A repeat carries no
// local time of its own, so without this the context would show it as an empty slot — and
// read as a one-off to a model deciding whether to correct it.
func whenOf(reminder store.Reminder) string {
	when := reminder.LocalDateTime
	if when == "" {
		when = localAt(reminder.NextEmitAtUtc, reminder.Timezone)
		if reminder.Rule != nil {
			when += " " + reminder.Rule.Frequency
		}
	}
	return strings.TrimSpace(when + " " + reminder.Timezone)
}

// localAt renders one stored UTC instant as the wall clock of the zone it belongs to.
func localAt(utc, zone string) string {
	at, err := time.Parse(time.RFC3339, utc)
	if err != nil {
		return utc
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return at.UTC().Format("2006-01-02T15:04")
	}
	return at.In(loc).Format("2006-01-02T15:04")
}

// questionPrompt is the second artifact Blurt asks the same model for: the one question the user
// has to answer, written for this capture rather than picked from a list of three. The last line is the
// one that matters — a model just handed the capture's own clock words will happily name one, and a
// question that asserts a time the user never said is the failure this whole path exists to avoid.
const questionPrompt = `You ask the user one question about a capture Blurt could not finish.
The missing line names the fields still unknown. Ask for  those, in the user's own words for the event.
One question, under twenty words, no greeting, no preamble, no apology.
Never name a time, a date, a place or a zone the user did not say, and never say anything was saved.

Example
current_tz: Asia/Singapore
now_local: 2026-10-24T18:30
missing: time
capture: buy glue when I'm home
->
question: When should I remind you about "buy glue"?`

// questionSchema is one field, for the same reason the records schema stays small: the decoder fills
// whatever is declared, and every extra field is room for a small model to wander.
const questionSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "question": {"type": "string"}
  },
  "required": ["question"]
}`

// tagsPrompt is the third artifact Blurt asks a model for: the search features of
// one Memory. It is a separate call rather than a field on the records schema
// because that schema is measured — a ten-field one made the model collapse into
// repeated tokens — and tags are the one place where more output is better.
//
// "Think outside the box" is deliberate. A model asked only for the fact's own
// words returns the fact's own words, which is exactly what BM25 already has;
// the value is in the words a future, differently-worded capture would use.
const tagsPrompt = `You name the search features of one remembered fact, so Blurt can find it again from wording nothing like the fact itself.
Answer with 3 to 10 tags, comma-separated, in one string.
Each tag is one simple, common, singular word, lowercase: no quotes, no punctuation, no explanation.
Think outside the box: name the topic, the object, the place, the people, and the occasion the fact belongs to, not only the words already in the sentence.
More tags are better than fewer, but every tag must be one a future sentence about this fact could use.

Example
fact: I have AirAsia points to use by December
tags: travel, flight, airline, points, loyalty, voucher, december, trip, booking, expiry

Example
fact: Milk is cheaper at store A than store B
tags: milk, grocery, price, shopping, store, supermarket, dairy, savings, budget, errand`

// tagsSchema is one flat string for the same reason the records schema is small:
// a declared list gives a small model room to wander, and the server can split a
// comma-separated string itself.
const tagsSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "tags": {"type": "string"}
  },
  "required": ["tags"]
}`

// tags is the tags schema's shape on the Go side.
type tags struct {
	Tags string `json:"tags"`
}

// maxTags caps the features one Memory carries. Tags are searched with column
// weighting, so a handful is signal and a page is noise.
const maxTags = 10

// parseTags normalizes the tags answer into the list a Memory stores: lowercased,
// deduped, and capped. Empties are dropped, because a model told to always fill a
// field will sometimes fill it with nothing.
func parseTags(raw string) []string {
	seen := make(map[string]bool)
	normalized := []string{}
	for _, field := range strings.Split(raw, ",") {
		tag := strings.ToLower(strings.TrimSpace(field))
		tag = strings.Trim(tag, "\"'.#")
		tag = strings.Join(strings.Fields(tag), " ")
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		normalized = append(normalized, tag)
		if len(normalized) == maxTags {
			break
		}
	}
	return normalized
}
