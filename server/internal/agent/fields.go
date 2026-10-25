package agent

import (
	"strings"
	"time"

	"blurt/internal/store"
)

// MissingFields names the fields of a proposed write the server cannot accept as
// they stand. No model call and no edit: whatever it names is exactly what the
// question asks the user for, and a write it accepts is one the server can commit.
func MissingFields(write Write, now time.Time) []Field {
	missing := []Field{}
	if strings.TrimSpace(write.Text) == "" {
		missing = append(missing, FieldEvent)
	}
	if write.Kind == KindMemory {
		// A Memory is text and nothing else: it carries no zone, day, clock or
		// pattern, so none of them can be missing.
		return missing
	}
	name := strings.TrimSpace(write.Zone)
	zone, zoneErr := time.LoadLocation(name)
	// An empty name is not a zone: LoadLocation answers UTC for "", which would
	// silently pin a reminder to a zone the user never named.
	zoneOK := name != "" && zoneErr == nil
	if !zoneOK {
		missing = append(missing, FieldZone)
	}
	date, dateOK := parseDate(write.Date)
	hour, minute, timeOK := parseTime(write.Time)
	switch write.Recurrence {
	case RecurrenceNone:
		// A one-off needs its whole local time, and one still ahead of us.
		if !dateOK {
			missing = append(missing, FieldDate)
		}
		if !timeOK {
			missing = append(missing, FieldTime)
		}
		if dateOK && timeOK && zoneOK &&
			!localTime(date.Year(), date.Month(), date.Day(), hour, minute, zone).After(now) {
			missing = append(missing, FieldTime)
		}
	case store.FrequencyDaily:
		if !timeOK {
			missing = append(missing, FieldTime)
		}
	case store.FrequencyWeekly, store.FrequencyMonthly, store.FrequencyYearly:
		// A repeat's calendar part comes from its first occurrence, so the date
		// is as required as the clock.
		if !timeOK {
			missing = append(missing, FieldTime)
		}
		if !dateOK {
			missing = append(missing, FieldDate)
		}
	default:
		missing = append(missing, FieldRecurrence)
	}
	return missing
}
