package seed_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"blurt/internal/db"
	"blurt/internal/seed"
	"blurt/internal/store"
)

// The web bundle is the form users see; a seed that says a different version
// or asks different questions would leave setup looking unfinished. Read the
// file the app imports and pin the pairing.
func TestSeedMatchesWebQuestionnaire(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate seed test")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "web", "src", "data", "questionnaire", "v1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read web questionnaire: %v", err)
	}
	var web struct {
		Version int `json:"version"`
		Items   []struct {
			Prompt string `json:"prompt"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &web); err != nil {
		t.Fatalf("decode web questionnaire: %v", err)
	}

	if seed.Version != web.Version {
		t.Fatalf("seed version = %d, web version = %d", seed.Version, web.Version)
	}
	if len(seed.Answers) != len(web.Items) {
		t.Fatalf("seed answers = %d, web items = %d", len(seed.Answers), len(web.Items))
	}
	for index, item := range web.Items {
		if seed.Answers[index].Question != item.Prompt {
			t.Fatalf("answer %d asks %q, want %q", index, seed.Answers[index].Question, item.Prompt)
		}
		if seed.Answers[index].Answer == "" {
			t.Fatalf("answer %d is empty", index)
		}
	}
}

func TestSeedCreatesUserAndQuestionnaire(t *testing.T) {
	dir := t.TempDir()
	user, err := seed.Run(context.Background(), dir, "Lam@Example.com", "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if user.Email != "lam@example.com" {
		t.Fatalf("email = %q, want lowercased", user.Email)
	}
	if user.Name != "lam" {
		t.Fatalf("name = %q, want the email local part", user.Name)
	}
	if user.LastQuestionnaireVersion != seed.Version {
		t.Fatalf("version = %d, want %d", user.LastQuestionnaireVersion, seed.Version)
	}

	handle, err := db.Open(dir)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer handle.Close()

	users := store.NewUserStore(handle.Write, handle.Read)
	again, err := users.ByEmail(context.Background(), "lam@example.com")
	if err != nil {
		t.Fatalf("read seeded user: %v", err)
	}
	if again.UserID != user.UserID || again.LastQuestionnaireVersion != seed.Version {
		t.Fatalf("reread user = %+v, want the seeded row", again)
	}

	var count int
	if err := handle.Read.QueryRow(
		`SELECT count(*) FROM Memory WHERE UserId = ? AND Tags = 'questionnaire'`, user.UserID,
	).Scan(&count); err != nil {
		t.Fatalf("count questionnaire memory: %v", err)
	}
	if count != len(seed.Answers) {
		t.Fatalf("memory rows = %d, want %d", count, len(seed.Answers))
	}

	// Re-seeding refreshes the same row rather than making a second user or
	// duplicating the questionnaire Memory.
	again, err = seed.Run(context.Background(), dir, "lam@example.com", "Lam")
	if err != nil {
		t.Fatalf("reseed: %v", err)
	}
	if again.UserID != user.UserID || again.Name != "Lam" {
		t.Fatalf("reseeded user = %+v", again)
	}
	if err := handle.Read.QueryRow(
		`SELECT count(*) FROM Memory WHERE UserId = ? AND Tags = 'questionnaire'`, user.UserID,
	).Scan(&count); err != nil {
		t.Fatalf("count questionnaire memory after reseed: %v", err)
	}
	if count != len(seed.Answers) {
		t.Fatalf("memory rows after reseed = %d, want %d", count, len(seed.Answers))
	}
}
