package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"blurt/internal/store"
)

// stubRecall answers both lookups from what it was given and records how each was called,
// which is how a test sees that a lookup is scoped by the Job's owner.
type stubRecall struct {
	reminders []store.Reminder
	memories  []store.Memory
	err       error
	asked     []string
}

func (s *stubRecall) FindReminders(_ context.Context, userID int64, query string, limit int) ([]store.Reminder, error) {
	s.asked = append(s.asked, fmt.Sprintf("reminders user=%d query=%q limit=%d", userID, query, limit))
	return s.reminders, s.err
}

func (s *stubRecall) FindMemories(_ context.Context, userID int64, query string, limit int) ([]store.Memory, error) {
	s.asked = append(s.asked, fmt.Sprintf("memories user=%d query=%q limit=%d", userID, query, limit))
	return s.memories, s.err
}

var _ Recall = (*stubRecall)(nil)

// toolCall builds one call as the model writes it.
func toolCall(name, arguments string) ToolCall {
	return ToolCall{ID: "call-1", Type: functionType,
		Function: CallFunction{Name: name, Arguments: arguments}}
}

// TestToolsAnswerTheAgent pins what each tool returns: the rows a lookup found, the
// linter's faults, or the end of the loop. Every result carries the calls left, which is
// the same cap the prompt states, and every lookup is scoped by the Job's owner rather than
// by anything the model wrote.
func TestToolsAnswerTheAgent(t *testing.T) {
	recall := &stubRecall{
		reminders: []store.Reminder{{ReminderID: 42, Text: "Dentist", Timezone: zone,
			LocalDateTime: "2026-10-27T10:00", Status: store.ReminderResolved}},
		memories: []store.Memory{{Text: "Milk is cheaper at store A", Tags: "milk,grocery"}},
	}
	r := &jobRun{loop: &AgentLoop{Recall: recall}, job: store.Job{JobID: 1, UserID: 7}, started: pinned}

	cases := []struct {
		name   string
		call   ToolCall
		want   []string
		submit bool
	}{
		{"find_reminders", toolCall(toolFindReminders, `{"query":"dentist"}`),
			[]string{"id=42 Dentist | 2026-10-27T10:00 Asia/Singapore | resolved", "loop calls left: 3"}, false},
		{"find_memories", toolCall(toolFindMemories, `{"query":"milk"}`),
			[]string{"- Milk is cheaper at store A [milk,grocery]", "loop calls left: 3"}, false},
		{"submit_plan", toolCall(toolSubmitPlan, `{}`), []string{"submitted", "loop calls left: 3"}, true},
		{"a tool that is not one", toolCall("drop_tables", `{}`), []string{"no such tool"}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result, submit := r.runTool(context.Background(), test.call, 3)
			for _, want := range test.want {
				if !strings.Contains(result, want) {
					t.Fatalf("result = %q, want %q", result, want)
				}
			}
			if submit != test.submit {
				t.Fatalf("submit = %v, want %v", submit, test.submit)
			}
		})
	}
	// The lookup is the server's own scope: the id the model passed is not one of them.
	if got := strings.Join(recall.asked, "\n"); !strings.Contains(got, "reminders user=7 query=\"dentist\" limit=5") ||
		!strings.Contains(got, "memories user=7 query=\"milk\" limit=5") {
		t.Fatalf("lookups = %s, want each capped and scoped by the Job's owner", got)
	}
}

// A lookup that fails or is called with arguments the schema did not describe is answered
// in the result rather than failing the run: the model decides what to do without it.
func TestToolsAnswerTheirOwnFailures(t *testing.T) {
	r := &jobRun{loop: &AgentLoop{Recall: &stubRecall{err: errors.New("index is down")}},
		job: store.Job{JobID: 1, UserID: 7}, started: pinned}

	failed, submit := r.runTool(context.Background(), toolCall(toolFindReminders, `{"query":"dentist"}`), 4)
	if submit || !strings.Contains(failed, "find_reminders failed") || !strings.Contains(failed, "loop calls left: 4") {
		t.Fatalf("failed lookup = %q, want the failure answered with the calls left", failed)
	}
	bad, _ := r.runTool(context.Background(), toolCall(toolFindReminders, `not json`), 4)
	if !strings.Contains(bad, "needs") && !strings.Contains(bad, "arguments") {
		t.Fatalf("bad arguments = %q, want the model told what the call takes", bad)
	}
}
