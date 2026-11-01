package agent

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// check is one deterministic test of one proposed record. It answers the faults it found,
// or nil when the record passes; every fault is a sentence the model reads.
type check func(Record, time.Time) []string

// checks is the whole linter, in the order its faults read in: the day token leads, because an
// unreadable one is what leaves the date empty, and naming the token is the actionable half of
// that pair. checkAfterNow comes last, so it speaks only for a record whose fields are whole. A
// new check is a new function on this list and nothing else. The target whitelist is not one of
// them: it is about the answer rather than a record, so lint runs it first.
var checks = []check{checkDay, checkFields, checkAfterNow}

// lint is the server's verdict on the answer the loop is about to act on: every op's verb, the
// row it names, and every check over the record it proposes — all faults collected, or nil when
// the answer stands. It reads the answer and the ids this run's own lookups printed, and nothing
// else; it is the server's own authority, the same checks that would have been put to the user as
// a question, offered to the model first instead.
//
// The strict shape (one kind, and the fields the other kind does not use) is answerFrom's, where
// the JSON is read, and a question carries no op, so asking passes lint for free. A call-off is
// whole with no record at all: the app owns that write, so its op only has to say where it is.
func lint(answer Answer, now time.Time, found []int64) []string {
	switch answer.Kind {
	case AnswerQuestion:
		return nil
	case AnswerRecords:
	default:
		// The server never infers the kind from an empty field, and a Model may be a double:
		// the answer has to say which of the two it is.
		return []string{fmt.Sprintf("kind %q is neither records nor question", answer.Kind)}
	}
	if len(answer.Ops) == 0 && len(answer.Memories) == 0 {
		return []string{"nothing proposed: answer with an op, a fact, or a question"}
	}
	faults := []string{}
	for _, op := range answer.Ops {
		faults = append(faults, lintOp(op, now, found)...)
	}
	return faults
}

// lintOp is the verdict on one op: the verb it names, the row it names, and the record it
// proposes. It speaks for that op alone, so a two-op answer reads as two verdicts and the model
// is told about both at once.
func lintOp(op Op, now time.Time, found []int64) []string {
	switch op.Intent {
	case IntentCreate:
		if op.ReminderID != 0 {
			return []string{fmt.Sprintf("a create names no row, but reminder_id is %d", op.ReminderID)}
		}
	case IntentUpdate:
		if op.ReminderID == 0 {
			return []string{"an update names the row it changes: reminder_id is 0"}
		}
	case IntentCancel:
		// A call-off names no row and proposes no record: it only says where the write is done.
		if op.ReminderID != 0 {
			return []string{fmt.Sprintf("a call-off names no row, but reminder_id is %d", op.ReminderID)}
		}
		return nil
	default:
		// A verb the server does not know is named rather than guessed at, the way kind is.
		return []string{fmt.Sprintf("intent %q is neither create, update nor cancel", op.Intent)}
	}
	// The whitelist: a row may be named only if this run's own lookups printed it. The ids are
	// small integers over one user's own rows, so a guessed one is somebody's real Reminder.
	if op.ReminderID != 0 && !slices.Contains(found, op.ReminderID) {
		return []string{fmt.Sprintf(
			"reminder_id %d was not one of this run's own find_reminders results", op.ReminderID)}
	}
	return lintRecord(op.Record, now)
}

// lintRecord is every check over one record, all faults collected: the linter's body, so a
// pre-check on a change runs the same list rather than a second rulebook.
func lintRecord(record Record, now time.Time) []string {
	faults := []string{}
	for _, check := range checks {
		faults = append(faults, check(record, now)...)
	}
	return faults
}

// note renders the faults as the one block the model is handed, and as what the Trace
// keeps.
func note(faults []string) string {
	lines := make([]string, 0, len(faults))
	for _, fault := range faults {
		lines = append(lines, "lint: "+fault)
	}
	return strings.Join(lines, "\n")
}

// checkFields is MissingFields with one sentence per refused field: the value the model
// wrote and what the server would have taken. The shape of every field is in there; whether
// a one-off's instant is still ahead is checkAfterNow's, which is why this check ignores now.
func checkFields(record Record, _ time.Time) []string {
	faults := []string{}
	for _, field := range MissingFields(record) {
		spec := fieldFaults[field]
		if value := strings.TrimSpace(spec.value(record)); value != "" {
			faults = append(faults, fmt.Sprintf("%s %q is not %s", field, value, spec.expected))
			continue
		}
		faults = append(faults, string(field)+" is empty")
	}
	return faults
}

// checkAfterNow is the one check about the clock rather than the fields: a one-off whose
// instant has already gone gets its own sentence, so the model can tell a clock it cannot
// write from a clock that is simply too late. A record it cannot read the instant out of is
// left to checkFields, which names the field that is wrong.
func checkAfterNow(record Record, now time.Time) []string {
	if record.Recurrence != RecurrenceNone {
		// A repeat is walked forward from now, so nothing about it is too late.
		return nil
	}
	date, dateOK := parseDate(record.Date)
	hour, minute, timeOK := parseTime(record.Time)
	zone, err := time.LoadLocation(strings.TrimSpace(record.Zone))
	if !dateOK || !timeOK || err != nil {
		return nil
	}
	if localTime(date.Year(), date.Month(), date.Day(), hour, minute, zone).After(now) {
		return nil
	}
	return []string{fmt.Sprintf("time %q is in the past", strings.TrimSpace(record.Time))}
}

// fieldFaults is one entry per field MissingFields can name: where the model's value sits, and
// what the server would have taken instead, so a fault cannot quote a field against the wrong
// expectation.
var fieldFaults = map[Field]struct {
	value    func(Record) string
	expected string
}{
	FieldEvent:      {func(r Record) string { return r.Text }, "an event or a fact"},
	FieldZone:       {func(r Record) string { return r.Zone }, "an IANA zone, e.g. current_tz"},
	FieldDate:       {func(r Record) string { return r.Date }, "YYYY-MM-DD, or a day token the server can place"},
	FieldTime:       {func(r Record) string { return r.Time }, "HH:MM"},
	FieldRecurrence: {func(r Record) string { return r.Recurrence }, "none, daily, weekly, monthly or yearly"},
}

// checkDay reads the day token the model copied. A token outside the vocabulary leaves the
// date empty, which would read as a missing date; naming the token is what tells the model
// to copy the user's words instead of a date it worked out itself.
func checkDay(record Record, now time.Time) []string {
	if record.Day == "" {
		return nil
	}
	if _, ok := resolveDay(record.Day, inZone(now, record.Zone)); !ok {
		return []string{fmt.Sprintf(
			"day %q is not today, tonight, tomorrow, in:<N>d, this:<weekday>, next:<weekday>, next:<N>:<weekday>, or a date like 2026-12-02",
			record.Day)}
	}
	return nil
}
