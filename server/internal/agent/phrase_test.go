package agent

import (
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

// TestResolveTime pins the PRD's time rules against the words the model actually
// returns. Anything this cannot pin has to come back as a question, because a wrong
// fire time is the PRD's second worst failure mode.
func TestResolveTime(t *testing.T) {
	sg := mustLoad(t, "Asia/Singapore")
	saturday := time.Date(2026, 10, 24, 18, 30, 0, 0, sg)
	if saturday.Weekday() != time.Saturday {
		t.Fatalf("fixture is wrong: %s is a %s, want Saturday", saturday, saturday.Weekday())
	}
	// A weekday rolled on until it is ahead of now, which is the only case
	// allowed to move a week.
	fridayMorning := time.Date(2026, 10, 23, 9, 0, 0, 0, sg)
	if fridayMorning.Weekday() != time.Friday {
		t.Fatalf("fixture is wrong: %s is a %s, want Friday", fridayMorning, fridayMorning.Weekday())
	}
	december := time.Date(2026, 12, 2, 10, 0, 0, 0, sg)

	cases := []struct {
		name   string
		now    time.Time
		phrase string
		want   string // empty means "no time Blurt can pin"
	}{
		{"tomorrow with a clock", saturday, "at 6pm tomorrow", "2026-10-25T18:00"},
		{"the words in the other order", saturday, "6pm tomorrow", "2026-10-25T18:00"},
		{"minutes", saturday, "at 6:30am tomorrow", "2026-10-25T06:30"},
		{"24 hour clock", saturday, "at 18:00 tomorrow", "2026-10-25T18:00"},
		{"noon", saturday, "at noon tomorrow", "2026-10-25T12:00"},
		{"midnight", saturday, "at midnight tomorrow", "2026-10-25T00:00"},
		{"midnight spelled as 12am", saturday, "at 12am tomorrow", "2026-10-25T00:00"},
		{"noon spelled as 12pm", saturday, "at 12pm tomorrow", "2026-10-25T12:00"},
		{"day after tomorrow", saturday, "the day after tomorrow at 9am", "2026-10-26T09:00"},
		{"a weekday", saturday, "friday at 1pm", "2026-10-30T13:00"},
		{"the weekday next", saturday, "next friday at 2pm", "2026-10-30T14:00"},
		{"a month and a day", saturday, "november 1 at 8am", "2026-11-01T08:00"},
		{"a day and a month", saturday, "1 november at 8am", "2026-11-01T08:00"},
		{"an abbreviation", saturday, "1 nov at 8am", "2026-11-01T08:00"},
		{"an iso date", saturday, "2026-12-25 at 9am", "2026-12-25T09:00"},

		{"a weekday that is today and still ahead", fridayMorning, "friday at 1pm", "2026-10-23T13:00"},
		{"a weekday that is today and gone", fridayMorning, "thursday at 1pm", "2026-10-29T13:00"},

		// Everything below is a question, not a guess.
		{"a bare hour", saturday, "at 6 tomorrow", ""},
		{"a colon with no am or pm", saturday, "at 6:30 tomorrow", ""},
		{"no clock at all", saturday, "tomorrow", ""},
		{"already past", saturday, "at 5pm today", ""},
		{"a clock with no day that has passed", saturday, "at 6pm", ""},
		{"the measured fabrication", saturday, "when I am home from school", ""},
		{"the PRD's own relative case", saturday, "on the way home from work", ""},
		{"a date the month does not have", saturday, "31 november at 9am", ""},
		{"a date that has passed", december, "november 1 at 8am", ""},
		{"nothing", saturday, "", ""},
		{"a place, not a time", saturday, "at Ikea", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			at, ok := ResolveTime(test.phrase, test.now, sg)
			if test.want == "" {
				if ok {
					t.Fatalf("ResolveTime(%q) = %s, want a question",
						test.phrase, at.Format(time.RFC3339))
				}
				return
			}
			if !ok {
				t.Fatalf("ResolveTime(%q) asked, want %s", test.phrase, test.want)
			}
			if got := at.Format(localLayout); got != test.want {
				t.Fatalf("ResolveTime(%q) = %s, want %s", test.phrase, got, test.want)
			}
			// The instant and the stored human time have to agree: local time is
			// the truth and UTC is derived from it.
			if at.Location() != sg {
				t.Fatalf("ResolveTime(%q) answered in %s, want %s", test.phrase, at.Location(), sg)
			}
		})
	}
}

// TestResolveTimeDST pins the two cases the PRD names. Go answers the ambiguous one
// the way the PRD wants and the gap one the opposite way, so both are held here
// rather than trusted.
func TestResolveTimeDST(t *testing.T) {
	ny := mustLoad(t, "America/New_York")

	// 2026-03-08 02:00 EST jumps to 03:00 EDT, so 02:30 never happens.
	beforeGap := time.Date(2026, 3, 7, 12, 0, 0, 0, ny)
	at, ok := ResolveTime("tomorrow at 2:30am", beforeGap, ny)
	if !ok {
		t.Fatal("the gap case asked, want a resolved time")
	}
	// Shifted forward through the gap to 03:30 EDT. Go's own answer would have
	// been 06:30Z, the instant an hour before the jump, which is the hour the
	// user did not name.
	if got := at.UTC().Format(time.RFC3339); got != "2026-03-08T07:30:00Z" {
		t.Fatalf("gap = %s, want the first local time past the gap 2026-03-08T07:30:00Z", got)
	}
	if got := at.Format(localLayout); got != "2026-03-08T03:30" {
		t.Fatalf("gap local time = %s, want 2026-03-08T03:30", got)
	}

	// 2026-11-01 01:30 happens twice, at 05:30Z and 06:30Z. The PRD takes the
	// first, and Go does too.
	beforeFallback := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	at, ok = ResolveTime("tomorrow at 1:30am", beforeFallback, ny)
	if !ok {
		t.Fatal("the ambiguous case asked, want a resolved time")
	}
	if got := at.UTC().Format(time.RFC3339); got != "2026-11-01T05:30:00Z" {
		t.Fatalf("ambiguous = %s, want the first occurrence 2026-11-01T05:30:00Z", got)
	}
}
