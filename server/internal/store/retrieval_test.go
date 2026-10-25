package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"blurt/internal/db"
)

// The PRD's retrieval window is built on this query: bounded on both sides and
// ordered by the derived instant, never by the human time.
func TestReminderWindowBoundsAndOrders(t *testing.T) {
	_, handle, userID := newJobStack(t)
	ctx := context.Background()

	seed := []struct {
		text string
		emit string
	}{
		{"twenty four days before", "2026-10-01T09:00:00Z"},
		{"just before the window opens", "2026-10-23T11:00:00Z"},
		{"yesterday", "2026-10-24T10:00:00Z"},
		{"almost a fortnight out", "2026-11-08T09:00:00Z"},
		{"fifteen days after", "2026-11-09T09:00:00Z"},
	}
	for _, row := range seed {
		if err := insertReminder(ctx, handle.Write, Reminder{
			UserID: userID, Text: row.text, Timezone: "Asia/Singapore",
			LocalDateTime: "2026-10-25T09:00", EmitAtUtc: row.emit,
		}); err != nil {
			t.Fatalf("seed %s: %v", row.text, err)
		}
	}

	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	found, err := NewReminderStore(handle.Read).Window(ctx, userID, now.Add(-48*time.Hour), now.Add(14*24*time.Hour))
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("window returned %d reminders, want the 2 inside it", len(found))
	}
	if found[0].Text != "yesterday" || found[1].Text != "almost a fortnight out" {
		t.Fatalf("window order = %q, %q", found[0].Text, found[1].Text)
	}
	if found[1].Timezone != "Asia/Singapore" || found[1].LocalDateTime != "2026-10-25T09:00" {
		t.Fatalf("stored human time = %+v, want it read back verbatim", found[1])
	}
}

// Questionnaire answers are already Memory rows, so they must be retrievable before
// the agent ever writes one of its own. This is the test that proves the FTS index
// is actually kept in sync by the schema's triggers.
func TestMemorySearchFindsQuestionnaireMemory(t *testing.T) {
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	ctx := context.Background()

	users := NewUserStore(handle.Write, handle.Read)
	user, err := users.EnsureUser(ctx, "Lam", "lam@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	if _, err := users.SaveQuestionnaire(ctx, user.UserID, 1, []Answer{
		{Question: "What time zone are you usually in?", Answer: "Asia/Singapore"},
		{Question: "Where is home, and how do you usually get back?", Answer: "Tampines, by MRT from Raffles Place"},
	}); err != nil {
		t.Fatalf("save questionnaire: %v", err)
	}

	found, err := NewMemoryStore(handle.Read).Search(ctx, user.UserID, "heading home to Tampines", 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("search found nothing, want the questionnaire answer about home")
	}
	if !strings.Contains(found[0].Text, "Tampines") || found[0].Tags != "questionnaire" {
		t.Fatalf("found = %+v, want the questionnaire answer", found[0])
	}
}

// Raw user text cannot go into FTS5 as-is: its own grammar makes an apostrophe or a
// bare AND a syntax error, which would fail the whole plan turn.
func TestMemorySearchSurvivesFTS5Syntax(t *testing.T) {
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	ctx := context.Background()

	users := NewUserStore(handle.Write, handle.Read)
	user, err := users.EnsureUser(ctx, "Lam", "lam@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	memories := NewMemoryStore(handle.Read)

	if found, err := memories.Search(ctx, user.UserID, "   ", 0); err != nil || found != nil {
		t.Fatalf("empty query = %v, %v, want nothing read", found, err)
	}
	for _, query := range []string{
		`Dana's report`,
		`milk AND store OR NEAR(bread)`,
		`"unclosed quote`,
		`* ( ) : ^ -`,
	} {
		if _, err := memories.Search(ctx, user.UserID, query, 0); err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
	}
}

// One user's Memory can never surface in another user's prompt.
func TestMemorySearchIsScopedToOwner(t *testing.T) {
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	ctx := context.Background()

	users := NewUserStore(handle.Write, handle.Read)
	owner, err := users.EnsureUser(ctx, "Lam", "lam@example.com")
	if err != nil {
		t.Fatalf("ensure owner: %v", err)
	}
	other, err := users.EnsureUser(ctx, "Sam", "sam@example.com")
	if err != nil {
		t.Fatalf("ensure other: %v", err)
	}
	if _, err := users.SaveQuestionnaire(ctx, owner.UserID, 1, []Answer{
		{Question: "Where is home, and how do you usually get back?", Answer: "Tampines, by MRT"},
	}); err != nil {
		t.Fatalf("save questionnaire: %v", err)
	}

	found, err := NewMemoryStore(handle.Read).Search(ctx, other.UserID, "Tampines MRT", 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("found = %+v, want nothing for a different user", found)
	}
}

// Tags are generated features, so a Memory found through its tags outranks one
// that merely shares a word with the query. This is what makes tags a feature
// rather than extra text on the row.
func TestMemorySearchPrefersTagMatches(t *testing.T) {
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	ctx := context.Background()

	users := NewUserStore(handle.Write, handle.Read)
	user, err := users.EnsureUser(ctx, "Lam", "lam@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	// The same word in both rows: one in its text, one only in its tags.
	for _, memory := range []Memory{
		{Text: "the flight leaves at nine"},
		{Text: "a note about the airport", Tags: "flight"},
	} {
		memory.UserID = user.UserID
		if err := insertMemory(ctx, handle.Write, memory); err != nil {
			t.Fatalf("seed memory: %v", err)
		}
	}

	found, err := NewMemoryStore(handle.Read).Search(ctx, user.UserID, "flight", 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("found = %+v, want both rows", found)
	}
	if found[0].Text != "a note about the airport" || found[0].Tags != "flight" {
		t.Fatalf("order = %+v, want the tag match first", found)
	}
}

// The tokenizer stems both sides, so an inflected capture reaches the Memory its
// own spelling never matched: "groceries" finds "grocery", in the text or in a
// generated tag. This is what lets a user's wording drift from the words they
// originally used.
func TestMemorySearchStemsInflections(t *testing.T) {
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	ctx := context.Background()

	users := NewUserStore(handle.Write, handle.Read)
	user, err := users.EnsureUser(ctx, "Lam", "lam@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	// "grocery" appears in one row's text and one row's tags; neither row
	// contains the query's own spelling.
	for _, memory := range []Memory{
		{Text: "a grocery list"},
		{Text: "a note about the market", Tags: "grocery"},
	} {
		memory.UserID = user.UserID
		if err := insertMemory(ctx, handle.Write, memory); err != nil {
			t.Fatalf("seed memory: %v", err)
		}
	}
	memories := NewMemoryStore(handle.Read)

	found, err := memories.Search(ctx, user.UserID, "groceries", 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("found = %+v, want both rows reached through the stem", found)
	}

	// Stemming is not substring matching: a word neither row carries still finds
	// nothing, so the index has not quietly become a fuzzy one.
	if found, err := memories.Search(ctx, user.UserID, "flights", 0); err != nil || len(found) != 0 {
		t.Fatalf("flights = %+v, %v, want nothing", found, err)
	}
}
