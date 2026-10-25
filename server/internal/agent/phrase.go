package agent

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// localLayout is the human-time shape a Reminder stores: local date and time with
// no offset. UTC is derived from this plus the zone, never the other way round.
const localLayout = "2006-01-02T15:04"

var weekdayNames = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

var monthNames = []string{"january", "february", "march", "april", "may", "june",
	"july", "august", "september", "october", "november", "december"}

var (
	// clockRe requires am/pm: "at 6" is not a time Blurt can pin, and a wrong
	// fire time is the PRD's second worst failure mode.
	clockRe = regexp.MustCompile(`([0-9]{1,2})(?::([0-9]{2}))?\s*(am|pm)`)
	// clock24Re is only a time when it cannot be a 12-hour reading, so "18:00"
	// resolves and "6:30" asks.
	clock24Re  = regexp.MustCompile(`\b([0-9]{1,2}):([0-9]{2})\b`)
	isoDateRe  = regexp.MustCompile(`\b([0-9]{4})-([0-9]{2})-([0-9]{2})\b`)
	monthDayRe = regexp.MustCompile(`\b([a-z]{3,9})\.?\s+([0-9]{1,2})\b`)
	dayMonthRe = regexp.MustCompile(`\b([0-9]{1,2})\s+([a-z]{3,9})\.?\b`)
)

// ResolveTime turns the model's own words for when into the local instant the
// user meant, and reports false when the phrase names no time Blurt can pin.
//
// The model is deliberately not asked for a timestamp. A small model behind a
// constrained schema cannot leave a declared field empty for long: asked for a
// local date and time it invented one for "when I'm home from school" and read
// "6pm tomorrow" as 18:30, the clock out of now_local. Echoing the user's words is
// a copy, and a copy is what it gets right, so the arithmetic lives here where it
// is deterministic and testable.
//
// now must already be in loc: it is what "tomorrow" hangs off.
func ResolveTime(phrase string, now time.Time, loc *time.Location) (time.Time, bool) {
	text := strings.ToLower(strings.TrimSpace(phrase))
	if text == "" {
		return time.Time{}, false
	}
	hour, minute, ok := clockOf(text)
	if !ok {
		return time.Time{}, false
	}
	date, weekly := dateOf(text, now, loc)
	at := time.Date(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, loc)
	at = shiftThroughGap(at, hour, minute)
	if weekly && !at.After(now) {
		// "Friday" said on a Friday evening means the next one.
		at = at.AddDate(0, 0, 7)
	}
	if !at.After(now) {
		// A time already behind us is not one Blurt can pin: which day the user
		// meant has to come from the user.
		return time.Time{}, false
	}
	return at, true
}

// clockOf reads the clock time. A bare hour ("at 6") and a 12-hour colon form
// ("6:30") are both ambiguous, so they resolve to nothing and the Job asks rather
// than guessing.
func clockOf(text string) (int, int, bool) {
	if m := clockRe.FindStringSubmatch(text); m != nil {
		hour, err := strconv.Atoi(m[1])
		if err != nil || hour > 12 {
			return 0, 0, false
		}
		minute := 0
		if m[2] != "" {
			if minute, err = strconv.Atoi(m[2]); err != nil || minute > 59 {
				return 0, 0, false
			}
		}
		switch m[3] {
		case "am":
			if hour == 12 {
				hour = 0
			}
		case "pm":
			if hour != 12 {
				hour += 12
			}
		}
		return hour, minute, true
	}
	if m := clock24Re.FindStringSubmatch(text); m != nil {
		hour, _ := strconv.Atoi(m[1])
		minute, _ := strconv.Atoi(m[2])
		if hour < 13 || hour > 23 || minute > 59 {
			return 0, 0, false
		}
		return hour, minute, true
	}
	switch {
	case strings.Contains(text, "noon"):
		return 12, 0, true
	case strings.Contains(text, "midnight"):
		return 0, 0, true
	}
	return 0, 0, false
}

// namesClock reports whether a string names a clock time, in any of the forms Blurt reads.
// It is deliberately broader than clockOf: clockOf refuses an ambiguous 12-hour reading so the
// Job asks, while this only has to notice that time words are present at all, which is what
// keeps them out of the event a Reminder is titled with.
func namesClock(text string) bool {
	text = strings.ToLower(text)
	return clockRe.MatchString(text) || clock24Re.MatchString(text) ||
		strings.Contains(text, "noon") || strings.Contains(text, "midnight")
}

// dateOf finds the day the phrase points at, defaulting to today. weekly reports
// that a weekday name set it, which is the one case allowed to roll a week on.
func dateOf(text string, now time.Time, loc *time.Location) (time.Time, bool) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	switch {
	case strings.Contains(text, "day after tomorrow"):
		return today.AddDate(0, 0, 2), false
	case strings.Contains(text, "tomorrow"):
		return today.AddDate(0, 0, 1), false
	case strings.Contains(text, "today"), strings.Contains(text, "tonight"):
		return today, false
	}
	if date, ok := isoDateOf(text, loc); ok {
		return date, false
	}
	if date, ok := namedDateOf(text, now, loc); ok {
		return date, false
	}
	for index, name := range weekdayNames {
		if !strings.Contains(text, name) {
			continue
		}
		// "next friday" is still the next Friday; the roll in ResolveTime covers a
		// weekday that has already been and gone today.
		ahead := (index - int(today.Weekday()) + 7) % 7
		return today.AddDate(0, 0, ahead), true
	}
	return today, false
}

func isoDateOf(text string, loc *time.Location) (time.Time, bool) {
	m := isoDateRe.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	year, month, day := atoi(m[1]), atoi(m[2]), atoi(m[3])
	if year == 0 || month == 0 || day == 0 {
		return time.Time{}, false
	}
	return validDate(year, time.Month(month), day, loc)
}

// namedDateOf reads "november 1" and "1 november" alike. No year is inferred: the
// date is read in the current year, and one already past resolves to nothing, so
// "november 1" said in December asks rather than firing eleven months out.
func namedDateOf(text string, now time.Time, loc *time.Location) (time.Time, bool) {
	if m := monthDayRe.FindStringSubmatch(text); m != nil {
		if month, ok := monthOf(m[1]); ok {
			if date, ok := validDate(now.Year(), month, atoi(m[2]), loc); ok {
				return date, true
			}
		}
	}
	if m := dayMonthRe.FindStringSubmatch(text); m != nil {
		if month, ok := monthOf(m[2]); ok {
			if date, ok := validDate(now.Year(), month, atoi(m[1]), loc); ok {
				return date, true
			}
		}
	}
	return time.Time{}, false
}

// atoi reads a regex-captured digit run. A group the pattern cannot fill reads as
// zero, which validDate then refuses, so a malformed date asks instead of firing.
func atoi(value string) int {
	n, _ := strconv.Atoi(value)
	return n
}

// validDate builds the date and refuses one the user could not have meant:
// time.Date rolls 31 November into December, and a silent roll is a wrong fire time
// rather than a question.
func validDate(year int, month time.Month, day int, loc *time.Location) (time.Time, bool) {
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return time.Time{}, false
	}
	date := time.Date(year, month, day, 0, 0, 0, 0, loc)
	if date.Day() != day || date.Month() != month {
		return time.Time{}, false
	}
	return date, true
}

// monthOf reads a month name, full or three letters.
func monthOf(name string) (time.Month, bool) {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".")
	for index, full := range monthNames {
		if name == full || (len(name) == 3 && strings.HasPrefix(full, name)) {
			return time.Month(index + 1), true
		}
	}
	return 0, false
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
