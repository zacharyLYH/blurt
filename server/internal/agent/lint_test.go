package agent

import (
	"strings"
	"testing"
)

// TestLintCollectsEveryFault pins the linter's own contract: every check runs, every fault
// is collected in one block, and a question passes without a field being touched.
func TestLintCollectsEveryFault(t *testing.T) {
	cases := []struct {
		name    string
		answer  Answer
		want    []string
		nothing bool
	}{
		{"a resolved record", answerWith(oneOff("Call mum", "2026-10-25", "19:00")), nil, true},
		{"a fact", Answer{Kind: AnswerRecords, Records: []Record{{Kind: KindMemory, Text: "Milk"}}}, nil, true},
		{"a question", asked("When should I remind you?"), nil, true},
		// Which kinds exist is the linter's to state, and an answer that says nothing is not
		// a plan: the loop is told rather than committing nothing.
		{"no kind", Answer{}, []string{`kind "" is neither records nor question`}, false},
		{"a records answer with no record", Answer{Kind: AnswerRecords}, []string{"no record proposed"}, false},
		// A zero record is refused by every field that has no default: the event, the
		// zone, and the pattern. The date and clock wait on a pattern that names them.
		{"every field at once", answerWith(Record{Kind: KindReminder}),
			[]string{"event is empty", "zone is empty", "recurrence is empty"}, false},
		{"a second record's faults too",
			Answer{Kind: AnswerRecords, Records: []Record{
				oneOff("Call mum", "2026-10-25", "19:00"),
				{Kind: KindMemory, Text: "  "},
			}},
			[]string{"event is empty"}, false},
		// A clock the server cannot write and a clock that is simply too late are two
		// different faults: the first names the shape, the second names the past.
		{"a clock that is not one", recordWith(func(w *Record) { w.Time = "19:60" }),
			[]string{`time "19:60" is not HH:MM`}, false},
		{"a one-off already gone", recordWith(func(w *Record) { w.Date = "2026-10-24"; w.Time = "09:00" }),
			[]string{`time "09:00" is in the past`}, false},
		{"a day token outside the vocabulary",
			recordWith(func(w *Record) { w.Day = "when I'm home" }),
			[]string{`day "when I'm home" is not`}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			faults := lint(test.answer, pinned)
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
