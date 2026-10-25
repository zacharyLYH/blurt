package agent

import (
	"testing"
	"time"

	"blurt/internal/store"
)

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
		write   Write
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
			rule, at, ok := ruleFor(test.write, now, sg)
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
		write   Write
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
			rule, at, ok := ruleFor(test.write, test.now, test.loc)
			assertRule(t, rule, at, ok, test.want, test.wantAt, test.wantUtc)
		})
	}
}
