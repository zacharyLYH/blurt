package agent

import (
	"fmt"
	"strings"
)

// systemPrompt is a measured artifact, not prose. Three things in it were chosen
// from measurements and should survive an edit:
//
//   - The examples decide the answer. The model copies whatever example comes
//     last: with an "ask the user" example last it answered "When should I remind
//     you?" for a capture that named 6pm tomorrow. The order below ends on a
//     capture that yields both kinds of record, which is the common case.
//   - There is no tool list. Asked to pick one of six tool names, the model chose
//     wrong on a clear capture and on an ambiguous one both; asked to fill the
//     fields it understood, it was right three times out of three. Which record
//     to commit is the server's decision, not the model's.
//   - timePhrase is the user's words, never a resolved time. A constrained
//     decoder will not leave a declared field empty for long, so a local date and
//     time came back as an invented 2026-10-26T17:00 for "when I'm home from
//     school" — and read "6pm tomorrow" as 18:30, the clock out of now_local.
//     Copying words is what this model is good at; ResolveTime does the rest.
const systemPrompt = `You turn one spoken capture into records for Blurt.
text: the thing to be reminded about, short, with no time words. Empty if there is nothing to remind about.
timePhrase: the user's own words for WHEN, copied exactly. It is a time, never a place. Empty if the capture has no time words.
memoryText: a fact worth keeping for later, in the user's words. Empty if there is no such fact.
timezone: IANA zone. Copy current_tz unless the capture names another.
say: one short sentence for the user. When the capture holds more independent items than one event and one fact, leave every field empty and ask in say for the split: one reminder per blurt.

Example
current_tz: Asia/Singapore
capture: remind me to call mum tomorrow at 7pm
->
text: Call mum
timePhrase: tomorrow at 7pm
memoryText:
timezone: Asia/Singapore
say: Reminder set for tomorrow at 7pm.

Example
capture: remember that milk is cheaper at store A than store B
->
text:
timePhrase:
memoryText: Milk is cheaper at store A than store B
timezone: Asia/Singapore
say: Noted.

Example
capture: lunch at Ikea this Friday at 1pm, and I have an Ikea voucher to use by December
->
text: Lunch at Ikea
timePhrase: this Friday at 1pm
memoryText: I have an Ikea voucher to use by December
timezone: Asia/Singapore
say: Reminder set for Friday at 1pm.`

// recordsSchema is the shape the answer is forced into, as the gateway's own
// JSON-schema response format rather than OpenAI's tool calling. Every field is a
// string and every field is required: the decoder fills whatever is declared, and
// a number or an object only gives a small model room to wander. A ten-field
// schema made the model collapse into repeated tokens, so this stays small.
//
// The shape is also the per-turn cardinality guardrail: one text and one
// memoryText mean a turn can propose at most 1 Reminder and 1 Memory.
const recordsSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "say":        {"type": "string"},
    "text":       {"type": "string"},
    "timezone":   {"type": "string"},
    "timePhrase": {"type": "string"},
    "memoryText": {"type": "string"}
  },
  "required": ["say", "text", "timezone", "timePhrase", "memoryText"]
}`

// records is the schema's shape on the Go side.
type records struct {
	Say        string `json:"say"`
	Text       string `json:"text"`
	Timezone   string `json:"timezone"`
	TimePhrase string `json:"timePhrase"`
	MemoryText string `json:"memoryText"`
}

// question is the question schema's shape on the Go side.
type question struct {
	Question string `json:"question"`
}

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

// turnOf reads the records into a Turn. The schema has one shape for both kinds
// of record and the server decides which came back — a model that filled `text`
// proposed a Reminder, one that filled `memoryText` proposed a Memory, and one
// that filled neither proposed nothing, which is the Job asking the user.
func turnOf(parsed records) Turn {
	turn := Turn{Say: strings.TrimSpace(parsed.Say)}
	if text := strings.TrimSpace(parsed.Text); text != "" {
		turn.Writes = append(turn.Writes, Write{
			Kind:       KindReminder,
			Text:       text,
			Timezone:   strings.TrimSpace(parsed.Timezone),
			TimePhrase: strings.TrimSpace(parsed.TimePhrase),
		})
	}
	if text := strings.TrimSpace(parsed.MemoryText); text != "" {
		turn.Writes = append(turn.Writes, Write{Kind: KindMemory, Text: text})
	}
	return turn
}

// messages builds the one prompt the gateway sees, over whichever system prompt is asking.
// Everything the user said before this turn is history, and the newest turn is rendered as the
// capture line so the model reads it in the shape its examples use.
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

// contextBlock is what the model reads alongside the capture: the anchor zone,
// whatever retrieval found, and any refusal the server returned on the previous
// turn. The capture goes last so it is the freshest thing in the prompt.
func contextBlock(in Input, capture string) string {
	var block strings.Builder
	fmt.Fprintf(&block, "current_tz: %s\n", in.CurrentTZ)
	if len(in.Nearby) > 0 {
		block.WriteString("nearby reminders:\n")
		for _, reminder := range in.Nearby {
			fmt.Fprintf(&block, "- %s (%s %s)\n", reminder.Text, reminder.LocalDateTime, reminder.Timezone)
		}
	}
	if len(in.Remembered) > 0 {
		block.WriteString("remembered:\n")
		for _, memory := range in.Remembered {
			fmt.Fprintf(&block, "- %s\n", memory.Text)
		}
	}
	if len(in.Checks) > 0 {
		block.WriteString("these were refused, fix them or ask:\n")
		for _, check := range in.Checks {
			fmt.Fprintf(&block, "- %s (%s)", check.Code, check.Field)
			if label := labelOf(check.Write); label != "" {
				fmt.Fprintf(&block, ": %s", label)
			}
			block.WriteString("\n")
		}
	}
	if len(in.Accepted) > 0 {
		block.WriteString("these passed check:\n")
		for _, write := range in.Accepted {
			fmt.Fprintf(&block, "- %s\n", labelOf(write))
		}
	}
	fmt.Fprintf(&block, "capture: %s", capture)
	return block.String()
}

// labelOf is a write's own words: its text, plus the user's phrase where a
// Reminder has one. A refusal carries both so the model can attribute it, and a
// passing sibling carries them so the model knows what survived.
func labelOf(write Write) string {
	label := write.Text
	if write.TimePhrase != "" {
		if label != "" {
			label += " / "
		}
		label += write.TimePhrase
	}
	return label
}

// questionPrompt is the second artifact Blurt asks the same model for: the one question the user has
// to answer, written for this capture rather than picked from a list of three. The last line is the
// one that matters — a model just handed the capture's own clock words will happily name one, and a
// question that asserts a time the user never said is the failure this whole path exists to avoid.
const questionPrompt = `You ask the user one question about a capture Blurt could not finish.
The refused lines say what is missing. Ask for that, in the user's own words for the event.
One question, under twenty words, no greeting, no preamble, no apology.
Never name a time, a date, a place or a zone the user did not say, and never say anything was saved.

Example
current_tz: Asia/Singapore
refused: time_unresolved (timePhrase)
capture: buy glue when I'm home
->
question: When should I remind you about “buy glue”?`

// questionSchema is one field, for the same reason the records schema is small: the decoder fills
// whatever is declared, and every extra field is room for a small model to wander.
const questionSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "question": {"type": "string"}
  },
  "required": ["question"]
}`
