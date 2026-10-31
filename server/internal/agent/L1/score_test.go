package l1

import (
	"fmt"
	"slices"
	"strings"

	"blurt/internal/agent"
	"blurt/internal/store"
)

// miss is one thing that did not match: which key, and what it was against what it should
// have been. The keys are the report's per-field failures.
type miss struct {
	Field  string
	Detail string
}

// compare is the scorer: what the run left behind against the case's static truth. The
// derived fields are exact, because the server computed them from the pinned clock; only a
// record's text is matched by substring, because prose belongs to the model and the event
// does not.
func compare(c Case, got outcome) []miss {
	wantStatus := store.JobComplete
	if c.Expect.Kind == agent.AnswerQuestion {
		wantStatus = store.JobClarifying
	}
	var misses []miss
	if got.status != wantStatus {
		misses = append(misses, miss{"kind", fmt.Sprintf("job is %s, want %s", got.status, wantStatus)})
	}
	if got.status == store.JobFailed {
		if reason := traceString(got.trace, "error"); reason != "" {
			misses = append(misses, miss{"error", reason})
		}
	}
	if c.Expect.Kind == agent.AnswerQuestion {
		return append(misses, compareQuestion(c, got)...)
	}
	return append(misses, compareRecords(c, got)...)
}

// compareQuestion is the ask half: a question commits nothing, and it has to be about the
// thing the capture left open. The wording is the model's, so the ask is matched by
// substring and any one of the accepted words counts.
func compareQuestion(c Case, got outcome) []miss {
	var misses []miss
	if len(got.reminders) > 0 || len(got.memories) > 0 {
		misses = append(misses, miss{"count", fmt.Sprintf(
			"a question commits nothing, but %d reminders and %d memories landed",
			len(got.reminders), len(got.memories))})
	}
	if len(c.Expect.AsksFor) > 0 && !containsAny(got.reply, c.Expect.AsksFor) {
		misses = append(misses, miss{"asks_for", fmt.Sprintf(
			"the question %q asks about none of %v", got.reply, c.Expect.AsksFor)})
	}
	return misses
}

// compareRecords is the commit half: the counts, then every row, then the lookup if the
// case is about one.
func compareRecords(c Case, got outcome) []miss {
	var misses []miss
	if len(got.reminders) != len(c.Expect.Reminders) {
		misses = append(misses, miss{"count", fmt.Sprintf("%d reminders, want %d: %s",
			len(got.reminders), len(c.Expect.Reminders), reminderSummary(got.reminders))})
	}
	for i, want := range c.Expect.Reminders {
		if i >= len(got.reminders) {
			break
		}
		misses = append(misses, compareReminder(i, want, got.reminders[i])...)
	}
	if len(got.memories) != len(c.Expect.Memories) {
		misses = append(misses, miss{"count", fmt.Sprintf("%d memories, want %d: %v",
			len(got.memories), len(c.Expect.Memories), got.memories)})
	}
	for i, want := range c.Expect.Memories {
		if i >= len(got.memories) {
			break
		}
		for _, part := range want.Contains {
			if !containsFold(got.memories[i], part) {
				misses = append(misses, miss{fmt.Sprintf("memories[%d]", i),
					fmt.Sprintf("%q is not in %q", part, got.memories[i])})
			}
		}
	}
	for _, tool := range c.Expect.Tools {
		if !slices.Contains(got.tools, tool) {
			misses = append(misses, miss{"tools",
				fmt.Sprintf("%s was not called; the run called %v", tool, got.tools)})
		}
	}
	return misses
}

// compareReminder is one row: the event by substring, then every derived field exactly. An
// empty key is not asserted, which is how a case about a repeat leaves the local date time
// alone.
func compareReminder(i int, want ExpectReminder, got store.Reminder) []miss {
	var misses []miss
	key := func(field string) string { return fmt.Sprintf("reminders[%d].%s", i, field) }
	for _, part := range want.TextContains {
		if !containsFold(got.Text, part) {
			misses = append(misses, miss{key("text"), fmt.Sprintf("%q is not in %q", part, got.Text)})
		}
	}
	for _, field := range []struct {
		name string
		want string
		got  string
	}{
		{"datetime", want.DateTime, got.LocalDateTime},
		{"utc", want.UTC, instantOf(got)},
		{"zone", want.Zone, got.Timezone},
		{"recurrence", want.Recurrence, recurrenceOf(got)},
		{"status", want.Status, got.Status},
	} {
		if field.want != "" && field.want != field.got {
			misses = append(misses, miss{key(field.name),
				fmt.Sprintf("%q, want %q", field.got, field.want)})
		}
	}
	return append(misses, compareRule(key("rule"), want.Rule, got.Rule)...)
}

// compareRule is a repeat's pattern, field by field. A zero field is not asserted.
func compareRule(key string, want *ExpectRule, got *store.Rule) []miss {
	if want == nil {
		return nil
	}
	if got == nil {
		return []miss{{key, "no rule was stored"}}
	}
	var misses []miss
	for _, field := range []struct {
		name string
		want any
		got  any
	}{
		{"frequency", want.Frequency, got.Frequency},
		{"time", want.Time, got.Time},
		{"weekday", want.Weekday, got.Weekday},
		{"day", want.Day, got.Day},
		{"month", want.Month, got.Month},
	} {
		if unset(field.want) || fmt.Sprint(field.want) == fmt.Sprint(field.got) {
			continue
		}
		misses = append(misses, miss{key + "." + field.name,
			fmt.Sprintf("%v, want %v", field.got, field.want)})
	}
	return misses
}

// instantOf is the row's instant: a one-off's local time derived to UTC, a repeat's next
// occurrence. Both are the server's arithmetic, so both are asserted exactly.
func instantOf(reminder store.Reminder) string {
	if reminder.EmitAtUtc != "" {
		return reminder.EmitAtUtc
	}
	return reminder.NextEmitAtUtc
}

// recurrenceOf is the frequency the row repeats on, or none for a one-off.
func recurrenceOf(reminder store.Reminder) string {
	if reminder.Rule == nil {
		return agent.RecurrenceNone
	}
	return reminder.Rule.Frequency
}

// reminderSummary is what the rows actually said, so a count that is off reads in one line
// rather than needing a re-run with -audit.
func reminderSummary(reminders []store.Reminder) string {
	summary := make([]string, 0, len(reminders))
	for _, reminder := range reminders {
		summary = append(summary, fmt.Sprintf("%s @ %s %s %s", reminder.Text,
			instantOf(reminder), reminder.Timezone, recurrenceOf(reminder)))
	}
	return strings.Join(summary, " | ")
}

// traceString reads one diagnostic off a Trace detail.
func traceString(detail map[string]any, key string) string {
	value, _ := detail[key].(string)
	return value
}

// unset reports whether a rule field was left out, which is how a case asserts only the
// parts of a pattern it is about.
func unset(value any) bool {
	return value == nil || fmt.Sprint(value) == "" || fmt.Sprint(value) == "0"
}

// containsFold is a case-insensitive substring test: prose is the model's, so a case
// asserts the words that carry the meaning and not the capitalisation.
func containsFold(text, part string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(part))
}

// containsAny reports whether any accepted word appears.
func containsAny(text string, parts []string) bool {
	for _, part := range parts {
		if containsFold(text, part) {
			return true
		}
	}
	return false
}

// traceStrings reads a list of diagnostics off a Trace detail: the lint faults a run had to
// clear, in the model's own terms.
func traceStrings(detail map[string]any, key string) []string {
	values, _ := detail[key].([]any)
	found := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			found = append(found, text)
		}
	}
	return found
}

// traceCount reads one of a Trace detail's numbers, which JSON decoding leaves a float64.
func traceCount(detail map[string]any, key string) int {
	count, _ := detail[key].(float64)
	return int(count)
}
