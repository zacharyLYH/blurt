package agent

import (
	"time"

	"blurt/internal/store"
)

// localLayout is the human-time shape a Reminder stores: local date and time with
// no offset. UTC is derived from this plus the zone, never the other way round.
const localLayout = "2006-01-02T15:04"

// reminderFor turns a write MissingFields accepted into the row Blurt stores: a
// one-off keeps its local time and derived instant, a repeat keeps the rule its
// next instant is derived from. now must already be in loc.
func reminderFor(write Write, now time.Time, loc *time.Location) (store.Reminder, bool) {
	reminder := store.Reminder{Text: write.Text, Timezone: write.Zone}
	hour, minute, _ := parseTime(write.Time)
	if write.Recurrence == RecurrenceNone {
		date, _ := parseDate(write.Date)
		at := localTime(date.Year(), date.Month(), date.Day(), hour, minute, loc)
		reminder.LocalDateTime = at.Format(localLayout)
		reminder.EmitAtUtc = at.UTC().Format(time.RFC3339)
		return reminder, true
	}
	rule, at, ok := ruleFor(write, now, loc)
	if !ok {
		return store.Reminder{}, false
	}
	reminder.Rule, reminder.NextEmitAtUtc = &rule, at.UTC().Format(time.RFC3339)
	return reminder, true
}

// ruleFor is the pattern a recurring write repeats on, with the calendar parts
// taken from the first occurrence the model resolved: the weekday a weekly rule
// lands on, the day a monthly one does, the month and day a yearly one does.
// now must already be in loc.
func ruleFor(write Write, now time.Time, loc *time.Location) (store.Rule, time.Time, bool) {
	hour, minute, ok := parseTime(write.Time)
	if !ok {
		return store.Rule{}, time.Time{}, false
	}
	rule := store.Rule{Frequency: write.Recurrence, Time: write.Time}
	if date, ok := parseDate(write.Date); ok {
		switch rule.Frequency {
		case store.FrequencyWeekly:
			rule.Weekday = int(date.Weekday())
		case store.FrequencyMonthly:
			rule.Day = date.Day()
		case store.FrequencyYearly:
			rule.Month, rule.Day = int(date.Month()), date.Day()
		}
	}
	at := nextEmit(rule, write.Date, now, hour, minute, loc)
	if at.IsZero() {
		return store.Rule{}, time.Time{}, false
	}
	return rule, at, true
}

// NextEmit is the first instant at or after now that a rule fires, in the zone the
// Reminder stores. It is exported for the one caller outside this package: the
// emitter, which advances a repeat with the same derivation the creation path used.
// The instant is rebuilt from the rule and the zone every time, never carried
// forward by a duration.
func NextEmit(rule store.Rule, now time.Time, loc *time.Location) (time.Time, bool) {
	hour, minute, ok := parseTime(rule.Time)
	if !ok {
		return time.Time{}, false
	}
	at := nextEmit(rule, "", now, hour, minute, loc)
	return at, !at.IsZero()
}

// nextEmit is the first instant at or after now that the rule fires. The first
// occurrence the model resolved is honoured while it is still ahead; otherwise
// the pattern is walked forward one period at a time. The instant is derived from
// the rule and the zone every time, never by adding a duration to the one before.
func nextEmit(rule store.Rule, first string, now time.Time, hour, minute int, loc *time.Location) time.Time {
	if date, ok := parseDate(first); ok {
		if at := localTime(date.Year(), date.Month(), date.Day(), hour, minute, loc); at.After(now) {
			return at
		}
	}
	year, month, day := now.Year(), now.Month(), now.Day()
	for i := range 13 {
		var at time.Time
		switch rule.Frequency {
		case store.FrequencyDaily:
			at = localTime(year, month, day+i, hour, minute, loc)
		case store.FrequencyWeekly:
			ahead := (rule.Weekday - int(now.Weekday()) + 7) % 7
			at = localTime(year, month, day+ahead+7*i, hour, minute, loc)
		case store.FrequencyMonthly:
			at = localTime(year, month+time.Month(i), rule.Day, hour, minute, loc)
			if at.Day() != rule.Day {
				continue // this month has no such day
			}
		default:
			at = localTime(year+i, time.Month(rule.Month), rule.Day, hour, minute, loc)
			if at.Month() != time.Month(rule.Month) || at.Day() != rule.Day {
				continue // this year has no such date
			}
		}
		if at.After(now) {
			return at
		}
	}
	return time.Time{}
}

// localTime builds one local instant, shifted through a DST gap the way the PRD
// asks: a local time that does not exist moves forward to the first that does.
func localTime(year int, month time.Month, day, hour, minute int, loc *time.Location) time.Time {
	return shiftThroughGap(time.Date(year, month, day, hour, minute, 0, 0, loc), hour, minute)
}

// shiftThroughGap fixes the one case Go resolves differently from the PRD. A local
// time inside a DST gap does not exist; Go answers with an instant whose wall clock
// is an hour behind, while the PRD shifts forward through the gap. Moving the
// instant by the wall-clock difference covers a whole-hour and a half-hour gap and
// is a no-op everywhere else.
//
// The ambiguous case needs nothing: Go answers with the first occurrence of a
// repeated hour, which is the PRD's own rule, and this leaves it alone because the
// requested wall clock is the one it returned.
func shiftThroughGap(at time.Time, hour, minute int) time.Time {
	y, m, d := at.Date()
	gotHour, gotMinute, _ := at.Clock()
	if gotHour == hour && gotMinute == minute {
		return at
	}
	want := time.Date(y, m, d, hour, minute, 0, 0, time.UTC)
	got := time.Date(y, m, d, gotHour, gotMinute, 0, 0, time.UTC)
	return at.Add(want.Sub(got))
}
