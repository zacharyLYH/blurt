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

// formatClock is the canonical clock a rule stores, so the tests assert the string
// a RecurringReminder would carry rather than a pair of numbers.
func formatClock(hour, minute int) string {
	return time.Date(2000, 1, 1, hour, minute, 0, 0, time.UTC).Format("15:04")
}
