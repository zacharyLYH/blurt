package agent

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	// clockRe requires am/pm and clock24Re a colon, so a bare hour is not read as
	// a time: a wrong fire time is the PRD's second worst failure mode.
	clockRe   = regexp.MustCompile(`([0-9]{1,2})(?::([0-9]{2}))?\s*(am|pm)`)
	clock24Re = regexp.MustCompile(`\b([0-9]{1,2}):([0-9]{2})\b`)
)

// namesClock reports whether a string names a clock time. It is the scripted model's
// reader; the question guard has its own, which knows where each clock came from.
func namesClock(text string) bool {
	text = strings.ToLower(text)
	return clockRe.MatchString(text) || clock24Re.MatchString(text) ||
		strings.Contains(text, "noon") || strings.Contains(text, "midnight")
}

// parseDate reads the ISO date the model returns, refusing a date the calendar
// does not have: the schema pins the shape, and this pins the day.
func parseDate(s string) (time.Time, bool) {
	date, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, false
	}
	return date, true
}

// parseTime reads the 24-hour clock the model returns, refusing 24:00 and 19:60.
func parseTime(s string) (int, int, bool) {
	clock, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, 0, false
	}
	return clock.Hour(), clock.Minute(), true
}

// dayNames is indexable by time.Weekday, which is the whole weekday table this needs.
var dayNames = [...]string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

// resolveDay reads the day token the model returns into the local date it names, and
// refuses every other string: a token the server cannot read leaves the date empty,
// which is the question rather than a wrong fire day. A user can name any offset —
// "in 13 days", "next next tuesday" — so no list of day words can exhaust the calendar.
// The token carries the shape, and the arithmetic happens here, on the clock the prompt
// was handed, rather than in a model that cannot add days.
func resolveDay(token string, now time.Time) (time.Time, bool) {
	token = strings.TrimSpace(strings.ToLower(token))
	parts := strings.Split(token, ":")
	var at time.Time
	switch {
	case token == "today" || token == "tonight":
		at = now
	case token == "tomorrow":
		at = now.AddDate(0, 0, 1)
	case len(parts) == 2 && parts[0] == "in" && strings.HasSuffix(parts[1], "d"):
		days, err := strconv.Atoi(strings.TrimSuffix(parts[1], "d"))
		if err != nil {
			return time.Time{}, false
		}
		at = now.AddDate(0, 0, days)
	case len(parts) == 2 && (parts[0] == "this" || parts[0] == "next"):
		ahead, ok := weekdayAhead(parts[1], now)
		if !ok {
			return time.Time{}, false
		}
		at = now.AddDate(0, 0, ahead)
	case len(parts) == 3 && parts[0] == "next":
		count, err := strconv.Atoi(parts[1])
		ahead, ok := weekdayAhead(parts[2], now)
		if err != nil || !ok || count < 1 {
			return time.Time{}, false
		}
		at = now.AddDate(0, 0, ahead+7*(count-1))
	default:
		return resolveDate(token, now)
	}
	return startOfDay(at), true
}

// resolveDate reads a date the model wrote. A full date is the one a row printed, so it is taken
// as it stands: walking it forward would move a row the user only meant to change the clock of. A
// month and day alone is a day the user named, and is walked to the next year that has it.
func resolveDate(token string, now time.Time) (time.Time, bool) {
	if date, err := time.Parse("2006-01-02", token); err == nil {
		return startOfDay(date), true
	}
	monthDay, err := time.Parse("01-02", token)
	if err != nil {
		return time.Time{}, false
	}
	// Walk forward to the next year that both has the date and has not gone: time.Date
	// rolls a date the year lacks (02-29 in a common year) into the next month, and
	// AddDate rolls it the same way, so neither can be trusted to find 02-29 here.
	for year := now.Year(); year-now.Year() <= 8; year++ {
		at := time.Date(year, monthDay.Month(), monthDay.Day(), 0, 0, 0, 0, now.Location())
		if at.Month() == monthDay.Month() && at.Day() == monthDay.Day() &&
			!at.Before(startOfDay(now)) {
			return at, true
		}
	}
	return time.Time{}, false
}

// weekdayAhead counts the days from now to the named weekday, counting today as the
// first of them: "next friday" said on a Saturday is the coming Friday, six days out,
// and "next:2:tuesday" puts the weeks on top of that.
func weekdayAhead(name string, now time.Time) (int, bool) {
	for i, day := range dayNames {
		if day == name {
			return (i - int(now.Weekday()) + 7) % 7, true
		}
	}
	return 0, false
}

// inZone anchors an instant to the user's own zone, so a day token resolves on their
// calendar rather than the server's. A zone the server cannot read leaves the clock
// where it was; the linter names that zone.
func inZone(now time.Time, zone string) time.Time {
	if loc, err := time.LoadLocation(strings.TrimSpace(zone)); err == nil {
		return now.In(loc)
	}
	return now
}

// startOfDay is the calendar day an instant belongs to, which is all a day token
// resolves to: the clock is a field of its own, and a day with a clock on it would
// read as one to the next reader.
func startOfDay(at time.Time) time.Time {
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
}
