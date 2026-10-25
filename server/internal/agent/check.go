package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"blurt/internal/store"
)

// The check codes. They are stable because they land in the Trace and because they
// are what the model is handed back to revise against.
const (
	CheckTextMissing      = "text_missing"
	CheckEventUnsupported = "event_unsupported"
	CheckEventHasTime     = "event_has_time"
	CheckContradicted     = "contradicted"
	CheckTimezoneMissing  = "timezone_missing"
	CheckTimezoneInvalid  = "timezone_invalid"
	CheckTimeUnresolved   = "time_unresolved"
	CheckConflict         = "conflict"
	CheckKindUnsupported  = "kind_unsupported"
	// CheckJobReminderExists is the per-Job cardinality guardrail: a Job holds at
	// most one Reminder, ever.
	CheckJobReminderExists = "job_reminder_exists"
	// CheckTimeUnsupported means the time phrase's words are not the user's,
	// which is different from a phrase that names no pinnable time.
	CheckTimeUnsupported = "time_unsupported"
)

// accept checks one proposed write and keeps it when it passes, reporting the check
// code and the field when it does not. This is PostAgentCheck's deterministic half:
// the server proves what it can prove without a model, and it never edits a
// proposal — a write either passes or comes back to the model as a code it can revise
// against.
func (r *run) accept(ctx context.Context, write Write) (string, string) {
	if strings.TrimSpace(write.Text) == "" {
		return CheckTextMissing, "text"
	}
	key := write.Kind + "\x00" + write.Text
	seen, repeated := r.accepted[key]
	if repeated && !sameWrite(seen, write) {
		// One event at two different times is not a duplicate to drop. It is a
		// conflict, and which one the user meant is the user's to say.
		return CheckConflict, "text"
	}
	if write.Kind == KindReminder && r.reminderHeld && !repeated {
		// A Job holds at most one Reminder: the schema can propose one per turn,
		// and this is what keeps a run from adding a second to an earlier one. A
		// repeat of the same proposal is not a second Reminder; the insert's own
		// NOT EXISTS drops its duplicate row.
		return CheckJobReminderExists, "kind"
	}
	if code, field := r.provenance(ctx, write); code != "" {
		return code, field
	}
	switch write.Kind {
	case KindReminder:
		reminder, code, field := checkReminder(write, r.started)
		if code != "" {
			return code, field
		}
		r.accepted[key], r.reminderHeld = write, true
		r.passed = append(r.passed, write)
		r.reminders = append(r.reminders, reminder)
	case KindMemory:
		r.accepted[key] = write
		r.passed = append(r.passed, write)
		r.memories = append(r.memories, store.Memory{Text: write.Text})
	default:
		// The schema has no kind field, so a kind this model cannot produce came
		// from something else and is not ours to commit.
		return CheckKindUnsupported, "kind"
	}
	return "", ""
}

// provenance is the event lint for both fields the model copies from the capture: a
// Reminder's text and its time phrase. The server proves what it can; a paraphrase is
// the one case it cannot settle, so that is where the fallback is asked. One call
// covers both fields, because the reader is handed the whole write.
func (r *run) provenance(ctx context.Context, write Write) (string, string) {
	said := captureText(r.job)
	textBad := unsupported(write.Text, said)
	phraseBad := write.Kind == KindReminder && strings.TrimSpace(write.TimePhrase) != "" &&
		unsupported(write.TimePhrase, said)
	if !textBad && !phraseBad {
		return "", ""
	}
	switch r.resolve(ctx, write) {
	case VerdictSupported:
		// Invited synonymy: the reader says this is what the user asked for.
		return "", ""
	case VerdictContradicted:
		if textBad {
			return CheckContradicted, "text"
		}
		return CheckContradicted, "timePhrase"
	default:
		if textBad {
			return CheckEventUnsupported, "text"
		}
		return CheckTimeUnsupported, "timePhrase"
	}
}

// resolve asks the fallback about one write, and reports unknown when there is no fallback or the call
// fails — which leaves the lint's own refusal standing. It is advisory, exactly like the transcript gate,
// so a gateway that is down never gets a record committed on its nod; what it said rides in the Trace
// either way, so a fallback that keeps abstaining is visible rather than anecdotal.
func (r *run) resolve(ctx context.Context, write Write) Verdict {
	if r.loop.Fallback == nil {
		return VerdictUnknown
	}
	reading, err := r.loop.Fallback.Resolve(ctx, captureText(r.job), write)
	if err != nil {
		slog.Warn("agent fallback", "job_id", r.job.JobID, "err", err)
	}
	r.readings = append(r.readings, fmt.Sprintf("%s:%.2f", reading.Verdict, reading.Confidence))
	return reading.Verdict
}

// sameWrite reports whether two proposals for one event name the same time and zone. Both are
// empty for a Memory, so a Memory that repeats is a plain duplicate.
func sameWrite(seen, write Write) bool {
	return seen.Timezone == write.Timezone && seen.TimePhrase == write.TimePhrase
}

// unsupported reports whether a record's words are ones the user never said. The prompt asks
// for both fields in the user's own words, so every content word of the record has to appear in
// the capture — which is what stops a model from committing an event the user never mentioned.
// Paraphrase is the case this misses on purpose: judging that is the fallback model's to do.
func unsupported(text, said string) bool {
	known := make(map[string]bool)
	for _, word := range contentWords(said) {
		known[word] = true
	}
	for _, word := range contentWords(text) {
		if !known[word] {
			return true
		}
	}
	return false
}

// contentWords is the words of a text that carry meaning: lowercased, stripped of punctuation,
// and with the framing a capture and a record do not have to share left out. A Reminder titled
// "Call mum" for "remind me to call mum tomorrow at 7pm" shares call and mum, which is the
// point of the test.
func contentWords(text string) []string {
	stripped := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}, text)
	words := make([]string, 0, 8)
	for _, word := range strings.Fields(stripped) {
		if !filler[word] {
			words = append(words, word)
		}
	}
	return words
}

// filler is the vocabulary a record does not inherit from the capture: articles, prepositions,
// pronouns, and the framing a capture arrives in ("remind me to").
var filler = map[string]bool{
	"a": true, "an": true, "and": true, "at": true, "be": true, "for": true, "from": true,
	"i": true, "in": true, "is": true, "it": true, "me": true, "my": true, "of": true,
	"on": true, "or": true, "please": true, "remind": true, "remember": true, "that": true,
	"the": true, "this": true, "to": true, "was": true, "with": true,
}

// checkReminder is the time lint, and the reason the model returns words instead of
// a timestamp: the server resolves them, so a fire time is never a model's
// arithmetic.
func checkReminder(write Write, now time.Time) (store.Reminder, string, string) {
	if namesClock(write.Text) {
		// The header and the time are told apart by the header having no clock in it: an
		// event that names one would be titled with the time it is also timed by.
		return store.Reminder{}, CheckEventHasTime, "text"
	}
	if strings.TrimSpace(write.Timezone) == "" {
		return store.Reminder{}, CheckTimezoneMissing, "timezone"
	}
	zone, err := time.LoadLocation(write.Timezone)
	if err != nil {
		return store.Reminder{}, CheckTimezoneInvalid, "timezone"
	}
	at, ok := ResolveTime(write.TimePhrase, now.In(zone), zone)
	if !ok {
		return store.Reminder{}, CheckTimeUnresolved, "timePhrase"
	}
	return store.Reminder{
		Text:          write.Text,
		Timezone:      write.Timezone,
		LocalDateTime: at.Format(localLayout),
		EmitAtUtc:     at.UTC().Format(time.RFC3339),
	}, "", ""
}
