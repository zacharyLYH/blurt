package agent

import (
	"context"
	"testing"

	"blurt/internal/store"
)

// The fake is the model the deterministic stack runs on, so what it can reach is what that
// stack can be exercised over without a gateway: every branch a spec needs has to be
// reachable by saying it, everything it proposes has to be a write the server can commit,
// and anything else has to ask rather than guess.
func TestFakeProposeReachesEachBranchByItsWords(t *testing.T) {
	cases := []struct {
		name       string
		capture    string
		writes     int
		kind       string
		text       string
		day        string
		recurrence string
	}{
		{"one-off tomorrow", "buy glue at 6pm tomorrow", 1, KindReminder, "buy glue", "2026-10-25", RecurrenceNone},
		{"daily", "take pills daily at 6pm", 1, KindReminder, "take pills", "", store.FrequencyDaily},
		{"daily in words", "take pills every day at 6pm", 1, KindReminder, "take pills", "", store.FrequencyDaily},
		{"weekly", "take the trash out every week at 6pm", 1, KindReminder, "take the trash out", "2026-10-24", store.FrequencyWeekly},
		{"monthly", "pay the rent every month at 6pm", 1, KindReminder, "pay the rent", "2026-10-24", store.FrequencyMonthly},
		{"yearly", "renew the passport every year at 6pm", 1, KindReminder, "renew the passport", "2026-10-24", store.FrequencyYearly},
		{"a fact", "remember milk is cheaper at store a", 1, KindMemory, "remember milk is cheaper at store a", "", ""},
		{"a pattern it cannot read", "clear the gutters every tuesday at 6pm", 0, "", "", "", ""},
		{"no clock", "buy glue when I'm home", 0, "", "", "", ""},
		{"two clocks", "dentist at 5pm and trash at 9pm", 0, "", "", "", ""},
	}
	for _, want := range cases {
		t.Run(want.name, func(t *testing.T) {
			turn, err := Fake{}.Propose(context.Background(), Input{
				Conversation: []store.Message{{Role: "user", Text: want.capture}},
				CurrentTZ:    "Asia/Singapore", Now: pinned,
			})
			if err != nil {
				t.Fatalf("propose: %v", err)
			}
			if len(turn.Writes) != want.writes {
				t.Fatalf("writes = %+v, want %d", turn.Writes, want.writes)
			}
			if want.writes == 0 {
				return
			}
			write := turn.Writes[0]
			if write.Kind != want.kind || write.Text != want.text ||
				write.Date != want.day || write.Recurrence != want.recurrence {
				t.Fatalf("write = %+v, want %s %q %q %q",
					write, want.kind, want.text, want.day, want.recurrence)
			}
			// Every write the fake proposes has to be one MissingFields accepts, which is
			// the contract between the dev stack and the server it runs against.
			if missing := MissingFields(write, pinned); len(missing) != 0 {
				t.Fatalf("missing = %v, want a write the server can commit", missing)
			}
			// A Reminder carries the anchor zone and the fake's one clock; a fact carries
			// neither, because a Memory is text and nothing else.
			if write.Kind == KindReminder && (write.Zone != "Asia/Singapore" || write.Time != FakeTime) {
				t.Fatalf("write = %+v, want the anchor zone and the fake clock", write)
			}
		})
	}
}

// "Soon" is the one branch whose clock is not the fake's own 18:00: it is the minute
// ahead of the run, so a spec can watch the real emitter fire a reminder that is nearly
// due without waiting for a fixed clock to arrive. It still has to be a write the server
// accepts — a one-off has to be strictly ahead of the run that resolved it — and the event
// still comes back as the user's own words, without the word that named the clock.
func TestFakeSoonIsTheMinuteAhead(t *testing.T) {
	turn, err := Fake{}.Propose(context.Background(), Input{
		Conversation: []store.Message{{Role: "user", Text: "buy glue soon"}},
		CurrentTZ:    "Asia/Singapore", Now: pinned,
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if len(turn.Writes) != 1 {
		t.Fatalf("writes = %+v, want one reminder", turn.Writes)
	}
	write := turn.Writes[0]
	if write.Kind != KindReminder || write.Text != "buy glue" || write.Time != "18:31" ||
		write.Date != "2026-10-24" || write.Recurrence != RecurrenceNone {
		t.Fatalf("write = %+v, want the minute ahead as an ordinary one-off", write)
	}
	if missing := MissingFields(write, pinned); len(missing) != 0 {
		t.Fatalf("missing = %v, want a write the server can commit", missing)
	}
}
