package agent

import (
	"context"
	"testing"

	"blurt/internal/store"
)

// fakeAnswer asks the fake the one question the deterministic stack is exercised over: the
// answer it finalizes for a capture. Every branch a spec needs is reachable by saying it.
func fakeAnswer(t *testing.T, capture string) Answer {
	t.Helper()
	answer, err := Fake{}.Finalize(context.Background(), PlannerInput{
		Conversation: []store.Message{{Role: "user", Text: capture}},
		CurrentTZ:    zone, Now: pinned,
	}, nil)
	if err != nil {
		t.Fatalf("fake finalize: %v", err)
	}
	return answer
}

// assertCommittable is the contract between the fake and the server it runs against:
// everything the fake proposes has to be an answer the whole linter accepts — the fields,
// the day token and the instant still being ahead.
func assertCommittable(t *testing.T, record Record) {
	t.Helper()
	if faults := lint(answerWith(record), pinned); len(faults) != 0 {
		t.Fatalf("lint = %v, want a record the server can commit", faults)
	}
}

// The fake is the model the deterministic stack runs on, so what it can reach is what that
// stack can be exercised over without a gateway: every branch a spec needs has to be
// reachable by saying it, everything it proposes has to be a record the server can commit,
// and anything else has to ask rather than guess. The clock it commits is the clock the
// capture named, which is what makes the fake's time assertions mean something.
func TestFakeProposeReachesEachBranchByItsWords(t *testing.T) {
	cases := []struct {
		name       string
		capture    string
		records    int
		kind       string
		text       string
		day        string
		clock      string
		recurrence string
	}{
		{"one-off tomorrow", "buy glue at 6pm tomorrow", 1, KindReminder, "buy glue", "2026-10-25", "18:00", RecurrenceNone},
		{"an evening clock", "remind me to call mum at 7pm", 1, KindReminder, "call mum", "2026-10-24", "19:00", RecurrenceNone},
		{"a morning clock tomorrow", "buy glue at 9am tomorrow", 1, KindReminder, "buy glue", "2026-10-25", "09:00", RecurrenceNone},
		{"a reminder said as remember to", "remember to buy glue at 8pm", 1, KindReminder, "buy glue", "2026-10-24", "20:00", RecurrenceNone},
		{"daily", "take pills daily at 6pm", 1, KindReminder, "take pills", "", "18:00", store.FrequencyDaily},
		{"daily in words", "take pills every day at 6pm", 1, KindReminder, "take pills", "", "18:00", store.FrequencyDaily},
		{"weekly", "take the trash out every week at 6pm", 1, KindReminder, "take the trash out", "2026-10-24", "18:00", store.FrequencyWeekly},
		{"monthly", "pay the rent every month at 6pm", 1, KindReminder, "pay the rent", "2026-10-24", "18:00", store.FrequencyMonthly},
		{"yearly", "renew the passport every year at 6pm", 1, KindReminder, "renew the passport", "2026-10-24", "18:00", store.FrequencyYearly},
		// A fact is the user's words without the word that named it a fact, which is what
		// the schema asks the real model for.
		{"a fact", "remember milk is cheaper at store a", 1, KindMemory, "milk is cheaper at store a", "", "", ""},
		{"a pattern it cannot read", "clear the gutters every tuesday at 6pm", 0, "", "", "", "", ""},
		{"no clock", "buy glue when I'm home", 0, "", "", "", "", ""},
		{"two events", "dentist at 5pm and trash at 9pm", 0, "", "", "", "", ""},
		{"a clock with no event", "remind me at 6pm", 0, "", "", "", "", ""},
		{"a clock already gone", "buy glue at 9am", 0, "", "", "", "", ""},
	}
	for _, want := range cases {
		t.Run(want.name, func(t *testing.T) {
			answer := fakeAnswer(t, want.capture)
			if len(answer.Records) != want.records {
				t.Fatalf("records = %+v, want %d", answer.Records, want.records)
			}
			if want.records == 0 {
				return
			}
			record := answer.Records[0]
			if record.Kind != want.kind || record.Text != want.text ||
				record.Date != want.day || record.Time != want.clock ||
				record.Recurrence != want.recurrence {
				t.Fatalf("record = %+v, want %s %q %q %q %q",
					record, want.kind, want.text, want.day, want.clock, want.recurrence)
			}
			assertCommittable(t, record)
			// A Reminder carries the anchor zone; a fact carries none, because a Memory
			// is text and nothing else.
			if record.Kind == KindReminder && record.Zone != zone {
				t.Fatalf("record = %+v, want the anchor zone", record)
			}
		})
	}
}

// A capture can name the notification clock and the event's own clock separately, and "soon"
// is the fake's third clock: the minute ahead of the run, so a spec can watch the real emitter
// fire a reminder that is nearly due without waiting for a fixed clock to arrive. The fake
// reads the "remind me" clock as the notification time and keeps the other clock in the event
// text, the same split the prompt asks the real model for. Every one has to be a one-off the
// server accepts.
func TestFakeSplitsNotifyAndEventClocks(t *testing.T) {
	cases := []struct {
		name    string
		capture string
		text    string
		date    string
		clock   string
	}{
		{"notify first", "remind me at 5pm for dinner at 8pm tomorrow", "dinner at 8pm", "2026-10-25", "17:00"},
		{"event first", "dinner at 8pm tomorrow, remind me at 5pm", "dinner at 8pm", "2026-10-25", "17:00"},
		{"soon notify", "dinner at 8pm remind me soon", "dinner at 8pm", "2026-10-24", "18:31"},
		{"soon alone", "buy glue soon", "buy glue", "2026-10-24", "18:31"},
	}
	for _, want := range cases {
		t.Run(want.name, func(t *testing.T) {
			answer := fakeAnswer(t, want.capture)
			if len(answer.Records) != 1 {
				t.Fatalf("records = %+v, want one reminder", answer.Records)
			}
			record := answer.Records[0]
			if record.Kind != KindReminder || record.Text != want.text ||
				record.Date != want.date || record.Time != want.clock ||
				record.Recurrence != RecurrenceNone {
				t.Fatalf("record = %+v, want %q %q %q", record, want.text, want.date, want.clock)
			}
			assertCommittable(t, record)
		})
	}
}
