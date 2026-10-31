package flow

import (
	"testing"
	"time"

	"blurt/internal/store"
)

// seedMemory records a Memory the way an earlier capture would have left one. The
// FTS triggers index it, so these flows exercise retrieval through the real
// AgentLoop without making the model record the rows first.
func (p *pump) seedMemory(t *testing.T, userID int64, text, tags string) {
	t.Helper()
	if _, err := p.handle.Write.Exec(
		`INSERT INTO Memory (UserId, Text, Tags, CreatedAt) VALUES (?, ?, ?, ?)`,
		userID, text, tags, pinned.UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
}

// Retrieval is what makes the Memory store useful: a later capture reaches the
// rows it needs and leaves the rest alone. The first intended row shares no word
// with the capture, so it comes back only through its generated tags; the second
// comes back through its own text.
func TestPipelineRetrievalPullsTheIntendedMemories(t *testing.T) {
	const capture = "book a flight to Chongqing"
	p := newPump(t).answer(capture, reminderTurn("book a flight", "tomorrow at 6pm"))
	p.seedMemory(t, p.userID, "AirAsia points are worth keeping", "travel,flight,airline,points,booking")
	p.seedMemory(t, p.userID, "Favourite noodle shop is in Chongqing", "food,noodle,restaurant")
	p.seedMemory(t, p.userID, "Milk costs less at Store B", "grocery,dairy,price")
	p.seedMemory(t, p.userID, "Dentist is on the second floor", "health,teeth,clinic")
	p.start()

	job := p.text(t, capture, "Asia/Singapore")
	p.settle(t, job.JobID, status(store.JobComplete), "the capture that recalls Memories")

	in := p.plannedFor(t, capture)
	for _, want := range []string{
		"AirAsia points are worth keeping",
		"Favourite noodle shop is in Chongqing",
	} {
		if !recalled(in, want) {
			t.Fatalf("remembered = %+v, want %q", in.Remembered, want)
		}
	}
	for _, unwanted := range []string{
		"Milk costs less at Store B",
		"Dentist is on the second floor",
	} {
		if recalled(in, unwanted) {
			t.Fatalf("remembered = %+v, did not want %q", in.Remembered, unwanted)
		}
	}
	if len(in.Remembered) != 2 {
		t.Fatalf("remembered = %d Memories, want exactly the two intended: %+v",
			len(in.Remembered), in.Remembered)
	}
	// Retrieval runs on the server between the screen and the plan: the seeded
	// Memories cost no model call of their own.
	p.wantAsks(t, 1, 2, 1, 0)
}

// A capture that shares no search term with any Memory still plans normally: an
// empty retrieval is a miss, not a failure.
func TestPipelineRetrievalReturnsNothingWhenNoMemoryMatches(t *testing.T) {
	const capture = "buy glue"
	p := newPump(t).answer(capture, reminderTurn("buy glue", "tomorrow at 6pm"))
	p.seedMemory(t, p.userID, "Milk costs less at Store B", "grocery,dairy,price")
	p.seedMemory(t, p.userID, "Dentist is on the second floor", "health,teeth,clinic")
	p.start()

	job := p.text(t, capture, "Asia/Singapore")
	p.settle(t, job.JobID, status(store.JobComplete), "the capture with no matching Memory")

	if in := p.plannedFor(t, capture); len(in.Remembered) != 0 {
		t.Fatalf("remembered = %+v, want none", in.Remembered)
	}
	p.wantAsks(t, 1, 2, 1, 0)
}

// The capture is prose, so it can carry punctuation an FTS5 MATCH expression
// would reject if it were passed through raw. Retrieval still finds the row.
func TestPipelineRetrievalSurvivesPunctuation(t *testing.T) {
	const capture = "Dana's flight to Chongqing"
	const fact = "AirAsia points are worth keeping"
	p := newPump(t).answer(capture, reminderTurn("book a flight", "tomorrow at 6pm"))
	p.seedMemory(t, p.userID, fact, "travel,flight,airline,points,booking")
	p.start()

	job := p.text(t, capture, "Asia/Singapore")
	p.settle(t, job.JobID, status(store.JobComplete), "the punctuation capture")

	if in := p.plannedFor(t, capture); !recalled(in, fact) {
		t.Fatalf("remembered = %+v, want the flight Memory", in.Remembered)
	}
	p.wantAsks(t, 1, 2, 1, 0)
}

// Retrieval is owner-scoped inside the loop, not only in the store: another
// user's Memory with the same tags must not reach this user's prompt.
func TestPipelineRetrievalStaysInsideTheOwner(t *testing.T) {
	const capture = "book a flight"
	const mine = "AirAsia points are worth keeping"
	const theirs = "AirAsia points belong to Sam"
	p := newPump(t).answer(capture, reminderTurn("book a flight", "tomorrow at 6pm"))
	other := p.user(t, "Sam", "sam@example.com")
	p.seedMemory(t, p.userID, mine, "travel,flight,airline,points,booking")
	p.seedMemory(t, other, theirs, "travel,flight,airline,points,booking")
	p.start()

	job := p.text(t, capture, "Asia/Singapore")
	p.settle(t, job.JobID, status(store.JobComplete), "the owner-scoped capture")

	in := p.plannedFor(t, capture)
	if !recalled(in, mine) {
		t.Fatalf("remembered = %+v, want the owner's Memory", in.Remembered)
	}
	if recalled(in, theirs) {
		t.Fatalf("remembered = %+v, did not want another user's Memory", in.Remembered)
	}
	if len(in.Remembered) != 1 {
		t.Fatalf("remembered = %d Memories, want only the owner's: %+v",
			len(in.Remembered), in.Remembered)
	}
	p.wantAsks(t, 1, 2, 1, 0)
}
