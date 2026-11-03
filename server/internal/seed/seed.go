// Package seed provisions the row the app cannot create for itself: the owner
// named by BLURT_SMTP_USERNAME. Nobody can approve the owner, so boot creates
// them active; the CLI half also fills the questionnaire Memory a local stack
// wants so nobody has to answer the form by hand.
package seed

import (
	"context"
	"fmt"
	"strings"

	"blurt/internal/db"
	"blurt/internal/store"
)

// Version is the questionnaire version written by a seed. It must match
// web/src/data/questionnaire/v1.json; a test reads that file and pins it.
const Version = 2

// Answers are the default questionnaire answers. The wording is what a real
// submission stores in Memory, so it stays close to the form's own labels.
var Answers = []store.Answer{
	{Question: "When do you usually wake up and go to sleep?", Answer: "Wake 07:00, sleep 23:00"},
	{Question: "Which days do you usually work?", Answer: "Weekdays"},
	{Question: "When do you usually work?", Answer: "09:00–18:00"},
	{Question: "Where do you usually work?", Answer: "At home"},
	{Question: "Where is home?", Answer: "Singapore"},
	{Question: "How do you usually get around?", Answer: "MRT or bus"},
	{Question: "When should Blurt stay quiet?", Answer: "Quiet from 22:00 until 07:00"},
}

// Owner creates the owner's row, or brings an existing one back to active. It
// runs on every boot, so a volume that predates this flow still lets the owner
// in. An empty address means no owner is configured, which dev mode allows.
func Owner(ctx context.Context, users *store.UserStore, email, name string) (store.User, error) {
	address, display, err := identity(email, name)
	if err != nil {
		return store.User{}, err
	}
	user, err := users.EnsureActive(ctx, display, address)
	if err != nil {
		return store.User{}, fmt.Errorf("seed: ensure owner: %w", err)
	}
	return user, nil
}

// Run opens the data directory, makes the owner active, and replaces their
// questionnaire Memory with the defaults.
func Run(ctx context.Context, dataDir, email, name string) (store.User, error) {
	handle, err := db.Open(dataDir)
	if err != nil {
		return store.User{}, fmt.Errorf("seed: open database: %w", err)
	}
	defer handle.Close()

	users := store.NewUserStore(handle.Write, handle.Read)
	user, err := Owner(ctx, users, email, name)
	if err != nil {
		return store.User{}, err
	}
	user, err = users.SaveQuestionnaire(ctx, user.UserID, Version, Answers)
	if err != nil {
		return store.User{}, fmt.Errorf("seed: save questionnaire: %w", err)
	}
	return user, nil
}

// identity normalises the owner address and falls back to its local part for a
// missing display name.
func identity(email, name string) (address, display string, err error) {
	address = strings.ToLower(strings.TrimSpace(email))
	if address == "" || !strings.Contains(address, "@") {
		return "", "", fmt.Errorf("seed: a valid email is required")
	}
	display = strings.TrimSpace(name)
	if display == "" {
		display = strings.Split(address, "@")[0]
	}
	return address, display, nil
}
