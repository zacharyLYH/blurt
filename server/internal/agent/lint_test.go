package agent

import (
	"strings"
	"testing"
)

// TestLintCollectsEveryFault pins the linter's own contract: every check runs, every fault
// is collected in one block, and a question passes without a field being touched. The verb and
// the row an answer names are read first, because a change to a row this run never saw is the
// one fault no field can describe.
func TestLintCollectsEveryFault(t *testing.T) {
	cases := []struct {
		name    string
		answer  Answer
		found   []int64
		want    []string
		nothing bool
	}{
		{"a resolved record", answerWith(oneOff("Call mum", "2026-10-25", "19:00")), nil, nil, true},
		{"a fact", Answer{Kind: AnswerRecords, Memories: []string{"Milk"}}, nil, nil, true},
		{"a question", asked("When should I remind you?"), nil, nil, true},
		// A call-off is the one op that is whole with no record: the app owns that write.
		{"a call-off", Answer{Kind: AnswerRecords, Ops: []Op{{Intent: IntentCancel}}}, nil, nil, true},
		// Which kinds exist is the linter's to state, and an answer that says nothing is not
		// a plan: the loop is told rather than committing nothing.
		{"no kind", Answer{}, nil, []string{`kind "" is neither records nor question`}, false},
		{"a verb the server does not know", Answer{Kind: AnswerRecords, Ops: []Op{{Intent: "delete"}}},
			nil, []string{`intent "delete" is neither create, update nor cancel`}, false},
		// The whitelist runs before any field: a small integer is somebody's real row.
		{"a row this run never saw",
			Answer{Kind: AnswerRecords, Ops: []Op{{Intent: IntentUpdate, ReminderID: 42,
				Record: oneOff("Call mum", "2026-10-25", "19:00")}}},
			nil, []string{"reminder_id 42 was not one of this run's own find_reminders results"}, false},
		{"a change to a row this run saw",
			Answer{Kind: AnswerRecords, Ops: []Op{{Intent: IntentUpdate, ReminderID: 42,
				Record: oneOff("Call mum", "2026-10-25", "19:00")}}},
			[]int64{42}, nil, true},
		{"a records answer with nothing in it", Answer{Kind: AnswerRecords},
			nil, []string{"nothing proposed"}, false},
		// A zero record is refused by every field that has no default: the event, the
		// zone, and the pattern. The date and clock wait on a pattern that names them.
		{"every field at once", answerWith(Record{Kind: KindReminder}),
			nil, []string{"event is empty", "zone is empty", "recurrence is empty"}, false},
		{"a second op's faults too",
			Answer{Kind: AnswerRecords, Ops: []Op{
				{Intent: IntentCreate, Record: oneOff("Call mum", "2026-10-25", "19:00")},
				{Intent: IntentCreate, Record: Record{Kind: KindReminder}},
			}},
			nil, []string{"event is empty", "zone is empty", "recurrence is empty"}, false},
		// A clock the server cannot write and a clock that is simply too late are two
		// different faults: the first names the shape, the second names the past.
		{"a clock that is not one", recordWith(func(w *Record) { w.Time = "19:60" }),
			nil, []string{`time "19:60" is not HH:MM`}, false},
		{"a one-off already gone", recordWith(func(w *Record) { w.Date = "2026-10-24"; w.Time = "09:00" }),
			nil, []string{`time "09:00" is in the past`}, false},
		{"a day token outside the vocabulary",
			recordWith(func(w *Record) { w.Day = "when I'm home" }),
			nil, []string{`day "when I'm home" is not`}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			faults := lint(test.answer, pinned, test.found)
			if test.nothing {
				if len(faults) != 0 {
					t.Fatalf("lint = %v, want the answer to pass", faults)
				}
				return
			}
			block := note(faults)
			for _, want := range test.want {
				if !strings.Contains(block, "lint: "+want) {
					t.Fatalf("note = %q, want a lint line naming %q", block, want)
				}
			}
			if len(faults) != len(test.want) {
				t.Fatalf("lint = %v, want %d faults", faults, len(test.want))
			}
		})
	}
}

// recordWith is one committable record with one field changed, looked at through the answer
// shape the linter reads.
func recordWith(mutate func(*Record)) Answer { return answerWith(mutateRecord(mutate)) }
