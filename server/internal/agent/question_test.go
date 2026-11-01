package agent

import (
	"testing"

	"blurt/internal/store"
)

// TestClocksOf pins the normalization the guard turns on: two spellings of one clock have to
// compare equal, or quoting the user would be refused as if the model had invented it.
func TestClocksOf(t *testing.T) {
	for _, test := range []struct {
		in   string
		want []string
	}{
		{"kids at 5pm or trash at 9pm?", []string{"17:00", "21:00"}},
		{"18:30 and 17:00", []string{"17:00", "18:30"}},
		{"at 1:30pm", []string{"13:30"}},
		{"at 12am", []string{"00:00"}},
		{"at 12pm", []string{"12:00"}},
		{"around noon", []string{"12:00"}},
		{"by midnight", []string{"00:00"}},
		{"at 9am tomorrow", []string{"09:00"}},
		{"loop calls left: 9", nil},
		{"no clock here", nil},
	} {
		got := clocksOf(test.in)
		for _, want := range test.want {
			if !got[want] {
				t.Errorf("clocksOf(%q) = %v, want %s", test.in, got, want)
			}
		}
		if len(got) != len(test.want) {
			t.Errorf("clocksOf(%q) = %v, want %d clocks", test.in, got, len(test.want))
		}
	}
}

// TestQuestionQuotesOnlyTheUsersClocks is the guard's own contract: the user's clocks and a
// row's clocks may be repeated back to them, and a clock nobody said may not.
func TestQuestionQuotesOnlyTheUsersClocks(t *testing.T) {
	capture := "remind me to pick up the kids at 5pm, and take out the trash at 9pm"
	asked := &jobRun{job: store.Job{Conversation: []store.Message{{Role: "user", Text: capture}}}}

	looked := &jobRun{
		job:    store.Job{Conversation: []store.Message{{Role: "user", Text: "push the dentist to next friday"}}},
		looked: []string{"id=1 Dentist | 2026-10-27 10:00 Asia/Singapore | pending\nloop calls left: 9"},
	}

	for _, test := range []struct {
		name string
		run  *jobRun
		want string
		ok   bool
	}{
		{"quotes both of the user's clocks", asked, "Which first: pick up the kids at 5pm, or take out the trash at 9pm?", true},
		{"reformats the user's clock", asked, "Should I remind you at 17:00 for the kids?", true},
		{"names no clock", asked, "Which one should I set first?", true},
		{"invents a clock", asked, "Should I remind you at 6pm for the kids?", false},
		{"quotes the user and invents one", asked, "Kids at 5pm or trash at 6pm?", false},
		{"quotes a clock it looked up", looked, "Should I keep the dentist at 10:00?", true},
		{"invents one beside a looked-up clock", looked, "Should I move the dentist to 10:30?", false},
		{"is not a question", asked, "Take out the trash at 9pm.", false},
	} {
		got := test.run.usableQuestion(test.want)
		if got != test.ok {
			t.Errorf("%s: usableQuestion(%q) = %v, want %v", test.name, test.want, got, test.ok)
		}
	}
}
