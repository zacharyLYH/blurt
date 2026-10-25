package store

import (
	"context"
	"testing"
)

// One step commits the records, the assistant turn, the status and the trace
// together, and a repeated step adds nothing: the Conversation is what a re-planned
// turn reads, so the same proposal must not leave two Reminders behind.
func TestCommitAgentStepIsAtomicAndSkipsRepeats(t *testing.T) {
	jobs, handle, userID := newJobStack(t)
	ctx := context.Background()

	job, err := jobs.Create(ctx, userID, Message{Role: "user", Text: "buy glue at 6pm"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	step := AgentStep{
		Reminders: []Reminder{{
			Text: "Buy glue", Timezone: "Asia/Singapore",
			LocalDateTime: "2026-10-25T18:00", EmitAtUtc: "2026-10-25T10:00:00Z",
		}},
		Memories: []Memory{{Text: "Milk is cheaper at store A"}},
		Reply:    Message{Text: "Reminder set."},
		Status:   JobComplete,
		Detail:   map[string]any{"outcome": JobComplete},
	}
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, step); err != nil {
		t.Fatalf("commit: %v", err)
	}

	stored, err := jobs.ByID(ctx, userID, job.JobID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stored.Status != JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	if len(stored.Conversation) != 2 || stored.Conversation[1].Text != "Reminder set." {
		t.Fatalf("conversation = %+v", stored.Conversation)
	}
	var source int64
	if err := handle.Read.QueryRow(`SELECT SourceJobId FROM Reminder`).Scan(&source); err != nil {
		t.Fatalf("read reminder: %v", err)
	}
	if source != job.JobID {
		t.Fatalf("reminder source = %d, want the Job that committed it", source)
	}
	var stages []string
	rows, err := handle.Read.Query(`SELECT Stage FROM Trace WHERE JobId = ?`, job.JobID)
	if err != nil {
		t.Fatalf("read traces: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var stage string
		if err := rows.Scan(&stage); err != nil {
			t.Fatalf("scan trace: %v", err)
		}
		stages = append(stages, stage)
	}
	if len(stages) != 1 || stages[0] == "" {
		t.Fatalf("trace stages = %v, want one agent row", stages)
	}

	// The same step again, which is what a resumed Job produces.
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, step); err != nil {
		t.Fatalf("recommit: %v", err)
	}
	for _, table := range []string{"Reminder", "Memory"} {
		var count int
		if err := handle.Read.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("%s rows = %d, want 1 after a repeated step", table, count)
		}
	}
}
