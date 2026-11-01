package agent

import (
	"testing"
	"time"

	"blurt/internal/store"
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
		{"a month and day still to come", "12-01", "2026-12-01"},
		{"a month and day that has gone", "03-15", "2027-03-15"},
		{"nothing", "", ""},
		{"prose", "when I'm home", ""},
		{"a day the month does not have", "02-30", ""},
		// A leap day is placed in the next year that has one, not rolled forward
		// into March 1st by time.Date.
		{"a leap day in a non-leap year", "02-29", "2028-02-29"},
		// A full date is the one a row printed, so it is taken as it stands: it is how a
		// change keeps the day the row already has, and walking it forward would move a row
		// the user only meant to re-clock.
		{"an exact date", "2026-12-01", "2026-12-01"},
		{"an exact date that has gone", "2026-01-05", "2026-01-05"},
		{"an exact date the calendar lacks", "2026-02-30", ""},
		{"a weekday that is not one", "this:october", ""},
		{"a count with no number", "in:d", ""},
		{"a count of no weeks", "next:0:friday", ""},
		{"a date the calendar lacks, written wrong", "2026-13-01", ""},
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

// assertRule is the assertion both rule tables share: the pattern, the first firing in
// the local zone, and the same instant in UTC. A zero rule means the pattern cannot be
// placed at all, which is the question path.
func assertRule(t *testing.T, rule store.Rule, at time.Time, ok bool, want store.Rule, wantAt, wantUtc string) {
	t.Helper()
	if want == (store.Rule{}) {
		if ok {
			t.Fatalf("placed %+v at %s, want nothing to place", rule, at.Format(localLayout))
		}
		return
	}
	if !ok {
		t.Fatalf("placed nothing, want %+v", want)
	}
	if rule != want {
		t.Fatalf("rule = %+v, want %+v", rule, want)
	}
	if got := at.Format(localLayout); got != wantAt {
		t.Fatalf("first firing = %s, want %s", got, wantAt)
	}
	if got := at.UTC().Format(time.RFC3339); got != wantUtc {
		t.Fatalf("first firing in UTC = %s, want %s", got, wantUtc)
	}
}

// TestRuleForPinsThePattern is the structured reader: the pattern the model named, with
// the calendar part read from the first occurrence it resolved, and the first instant the
// rule fires. The instant is derived from the rule and the zone every time, never by
// adding a duration to the fire before it.
func TestRuleForPinsThePattern(t *testing.T) {
	sg := mustLoad(t, "Asia/Singapore")
	// A Saturday evening, so a clock already behind it has to roll a day.
	now := time.Date(2026, 10, 24, 18, 30, 0, 0, sg)

	cases := []struct {
		name    string
		record  Record
		want    store.Rule
		wantAt  string
		wantUtc string
	}{
		{"daily with no fixed day", repeating("Stretch", "", "15:00", store.FrequencyDaily),
			store.Rule{Frequency: store.FrequencyDaily, Time: "15:00"}, "2026-10-25T15:00", "2026-10-25T07:00:00Z"},
		{"daily whose clock has gone", repeating("Stretch", "", "08:00", store.FrequencyDaily),
			store.Rule{Frequency: store.FrequencyDaily, Time: "08:00"}, "2026-10-25T08:00", "2026-10-25T00:00:00Z"},
		// The first occurrence the model resolved is honoured while it is still ahead.
		{"daily with a day of its own", repeating("Stretch", "2026-11-30", "15:00", store.FrequencyDaily),
			store.Rule{Frequency: store.FrequencyDaily, Time: "15:00"}, "2026-11-30T15:00", "2026-11-30T07:00:00Z"},
		{"weekly", repeating("Trash", "2026-10-27", "19:00", store.FrequencyWeekly),
			store.Rule{Frequency: store.FrequencyWeekly, Time: "19:00", Weekday: 2}, "2026-10-27T19:00", "2026-10-27T11:00:00Z"},
		{"monthly", repeating("Rent", "2026-11-05", "09:00", store.FrequencyMonthly),
			store.Rule{Frequency: store.FrequencyMonthly, Time: "09:00", Day: 5}, "2026-11-05T09:00", "2026-11-05T01:00:00Z"},
		{"yearly", repeating("Insurance", "2026-11-01", "08:00", store.FrequencyYearly),
			store.Rule{Frequency: store.FrequencyYearly, Time: "08:00", Month: 11, Day: 1}, "2026-11-01T08:00", "2026-11-01T00:00:00Z"},
		// A stale first occurrence is not an error: the pattern is walked forward from
		// now, which is what the emitter does after every firing.
		{"a weekly whose first day has gone", repeating("Trash", "2026-10-20", "19:00", store.FrequencyWeekly),
			store.Rule{Frequency: store.FrequencyWeekly, Time: "19:00", Weekday: 2}, "2026-10-27T19:00", "2026-10-27T11:00:00Z"},

		// A monthly with no day to land on cannot be placed, so the Job asks.
		{"a monthly with no day", repeating("Rent", "", "09:00", store.FrequencyMonthly),
			store.Rule{}, "", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			rule, at, ok := ruleFor(test.record, now, sg)
			assertRule(t, rule, at, ok, test.want, test.wantAt, test.wantUtc)
		})
	}
}

// TestRuleForCalendarEdges pins the calendar cases the PRD names: a monthly day the
// month does not have, a leap day, and a local time a DST gap skips.
func TestRuleForCalendarEdges(t *testing.T) {
	sg := mustLoad(t, "Asia/Singapore")
	ny := mustLoad(t, "America/New_York")

	cases := []struct {
		name    string
		record  Record
		now     time.Time
		loc     *time.Location
		want    store.Rule
		wantAt  string
		wantUtc string
	}{
		// February has no 31st, so the next firing is the March one rather than the
		// March 3 Go would silently roll the date into.
		{"a day the month does not have is skipped",
			repeating("Rent", "2026-01-31", "09:00", store.FrequencyMonthly),
			time.Date(2026, 2, 1, 10, 0, 0, 0, sg), sg,
			store.Rule{Frequency: store.FrequencyMonthly, Time: "09:00", Day: 31},
			"2026-03-31T09:00", "2026-03-31T01:00:00Z"},
		// A leap day is the model's to place, and the pattern then keeps it.
		{"a leap day the model placed",
			repeating("Birthday", "2028-02-29", "08:00", store.FrequencyYearly),
			time.Date(2027, 3, 1, 10, 0, 0, 0, sg), sg,
			store.Rule{Frequency: store.FrequencyYearly, Time: "08:00", Month: 2, Day: 29},
			"2028-02-29T08:00", "2028-02-29T00:00:00Z"},
		// 2026-03-08 02:30 EST never happens: the PRD shifts forward through the gap,
		// and the rule keeps the local time the user said.
		{"a clock the gap skips shifts forward",
			repeating("Backup", "2026-03-08", "02:30", store.FrequencyWeekly),
			time.Date(2026, 3, 7, 12, 0, 0, 0, ny), ny,
			store.Rule{Frequency: store.FrequencyWeekly, Time: "02:30", Weekday: 0},
			"2026-03-08T03:30", "2026-03-08T07:30:00Z"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			rule, at, ok := ruleFor(test.record, test.now, test.loc)
			assertRule(t, rule, at, ok, test.want, test.wantAt, test.wantUtc)
		})
	}
}
