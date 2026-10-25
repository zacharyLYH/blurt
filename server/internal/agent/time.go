package agent

import (
	"regexp"
	"strings"
	"time"
)

var (
	// clockRe requires am/pm and clock24Re a colon, so a bare hour is not read as
	// a time: a wrong fire time is the PRD's second worst failure mode.
	clockRe   = regexp.MustCompile(`([0-9]{1,2})(?::([0-9]{2}))?\s*(am|pm)`)
	clock24Re = regexp.MustCompile(`\b([0-9]{1,2}):([0-9]{2})\b`)
)

// namesClock reports whether a string names a clock time. It is the filter the
// question writer runs through: a question that asserts a time the user never
// said is the failure this whole path exists to avoid.
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
