package agent

import (
	"strings"
	"time"

	"blurt/internal/store"
)

// MissingFields names the fields of a proposed record the server cannot accept as
// they stand: the shape of every field, and nothing about the run's clock. Whether a
// one-off's instant is still ahead is checkAfterNow's, so a clock that is simply too
// late is refused in its own words. No model call and no edit: whatever it names is
// exactly what the model is told to fix, and, if it still cannot, what the question
// would have asked the user for.
func MissingFields(record Record) []Field {
	missing := []Field{}
	if strings.TrimSpace(record.Text) == "" {
		missing = append(missing, FieldEvent)
	}
	if record.Kind == KindMemory {
		// A Memory is text and nothing else: it carries no zone, day, clock or
		// pattern, so none of them can be missing.
		return missing
	}
	name := strings.TrimSpace(record.Zone)
	_, zoneErr := time.LoadLocation(name)
	// An empty name is not a zone: LoadLocation answers UTC for "", which would
	// silently pin a reminder to a zone the user never named.
	zoneOK := name != "" && zoneErr == nil
	if !zoneOK {
		missing = append(missing, FieldZone)
	}
	_, dateOK := parseDate(record.Date)
	_, _, timeOK := parseTime(record.Time)
	switch record.Recurrence {
	case RecurrenceNone:
		// A one-off needs its whole local time.
		if !dateOK {
			missing = append(missing, FieldDate)
		}
		if !timeOK {
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
