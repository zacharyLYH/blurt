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

// TestParseDate is the schema's pattern enforced a second time in Go: the shape is
// the gateway's job, and whether the calendar has the day is ours. A date Go would
// silently roll — 30 February — has to come back as a question.
func TestParseDate(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"a date", "2026-11-01", "2026-11-01"},
		{"a leap day", "2028-02-29", "2028-02-29"},
		{"the last day of a month", "2026-01-31", "2026-01-31"},
		{"nothing", "", ""},
		{"a day the month does not have", "2026-02-30", ""},
		{"a month that is not one", "2026-13-01", ""},
		{"a two digit year", "26-11-01", ""},
		{"prose", "tomorrow", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			date, ok := parseDate(test.in)
			if test.want == "" {
				if ok {
					t.Fatalf("parseDate(%q) = %s, want a question", test.in, date)
				}
				return
			}
			if !ok || date.Format("2006-01-02") != test.want {
				t.Fatalf("parseDate(%q) = %s %v, want %s", test.in, date, ok, test.want)
			}
		})
	}
}

// TestParseTime refuses the clocks a 24-hour reading cannot have, which is what
// keeps a wrong fire time out of the store.
func TestParseTime(t *testing.T) {
	cases := []struct {
		name, in string
		want     string
	}{
		{"a clock", "19:00", "19:00"},
		{"midnight", "00:00", "00:00"},
		{"the last minute of the day", "23:59", "23:59"},
		{"nothing", "", ""},
		{"an hour that is not one", "24:00", ""},
		{"a minute that is not one", "19:60", ""},
		{"twelve hour prose", "7pm", ""},
		{"a bare number", "1900", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			hour, minute, ok := parseTime(test.in)
			if test.want == "" {
				if ok {
					t.Fatalf("parseTime(%q) = %02d:%02d, want a question", test.in, hour, minute)
				}
				return
			}
			if !ok || formatClock(hour, minute) != test.want {
				t.Fatalf("parseTime(%q) = %02d:%02d %v, want %s", test.in, hour, minute, ok, test.want)
			}
		})
	}
}

// namesClock is the filter the question writer runs through: a question that asserts
// a time the user never said is the failure this whole path exists to avoid.
func TestNamesClock(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"call mum at 6pm", true},
		{"call mum at 18:00", true},
		{"call mum at noon", true},
		{"call mum at midnight", true},
		{"call mum at 6", false},
		{"when I'm home", false},
		{"Water the plants", false},
		{"", false},
	}
	for _, test := range cases {
		if got := namesClock(test.in); got != test.want {
			t.Fatalf("namesClock(%q) = %v, want %v", test.in, got, test.want)
		}
	}
}

// TestResolveDay is the token vocabulary read as a calendar. A user can name any
// offset — "in 13 days", "next next tuesday" — so no list of day words could exhaust
// the year; the token carries the shape and the arithmetic happens here. Everything
// outside the vocabulary is refused, which is what leaves the date empty and the Job
// asking the user rather than firing on a day nobody said.
func TestResolveDay(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"today", "today", "2026-10-24"},
		{"tonight", "tonight", "2026-10-24"},
		{"tomorrow", "tomorrow", "2026-10-25"},
		{"a day count", "in:13d", "2026-11-06"},
		{"a count of one day", "in:1d", "2026-10-25"},
		{"a weekday still to come this week", "this:monday", "2026-10-26"},
		{"this weekday", "this:friday", "2026-10-30"},
		{"next weekday", "next:friday", "2026-10-30"},
		{"a repeated next", "next:2:tuesday", "2026-11-03"},
		{"three nexts", "next:3:tuesday", "2026-11-10"},
		{"a month and day still to come", "on:12-01", "2026-12-01"},
		{"a month and day that has gone", "on:03-15", "2027-03-15"},
		{"nothing", "", ""},
		{"prose", "when I'm home", ""},
		{"a day the month does not have", "on:02-30", ""},
		{"a weekday that is not one", "this:october", ""},
		{"a count with no number", "in:d", ""},
		{"a count of no weeks", "next:0:friday", ""},
		{"a date where a token goes", "2026-10-25", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			at, ok := resolveDay(test.in, pinned)
			if test.want == "" {
				if ok {
					t.Fatalf("resolveDay(%q) = %s, want a question", test.in, at)
				}
				return
			}
			if !ok || at.Format("2006-01-02") != test.want {
				t.Fatalf("resolveDay(%q) = %s %v, want %s", test.in, at, ok, test.want)
			}
		})
	}
}

// formatClock is the canonical clock a rule stores, so the tests assert the string
// a RecurringReminder would carry rather than a pair of numbers.
func formatClock(hour, minute int) string {
	return time.Date(2000, 1, 1, hour, minute, 0, 0, time.UTC).Format("15:04")
}
