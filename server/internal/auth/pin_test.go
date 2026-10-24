package auth

import (
	"testing"
	"time"
)

func TestPinIsSingleUse(t *testing.T) {
	store := NewPinStore(time.Minute, "")
	code, err := store.Issue("Lam@Example.com ")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !store.Verify("lam@example.com", code) {
		t.Fatal("correct PIN rejected")
	}
	if store.Verify("lam@example.com", code) {
		t.Fatal("PIN worked twice")
	}
}

func TestPinRejectsWrongCodeAndExpires(t *testing.T) {
	store := NewPinStore(20*time.Millisecond, "")
	code, err := store.Issue("lam@example.com")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	if store.Verify("lam@example.com", wrong) {
		t.Fatal("wrong PIN accepted")
	}
	if !store.Verify("lam@example.com", code) {
		t.Fatal("correct PIN rejected after one miss")
	}

	expiring, err := store.Issue("other@example.com")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if store.Verify("other@example.com", expiring) {
		t.Fatal("expired PIN accepted")
	}
}

func TestPinAttemptCap(t *testing.T) {
	store := NewPinStore(time.Minute, "")
	code, err := store.Issue("lam@example.com")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	for range maxAttempts {
		if store.Verify("lam@example.com", "999999") {
			t.Fatal("wrong PIN accepted")
		}
	}
	if store.Verify("lam@example.com", code) {
		t.Fatal("PIN survived the attempt cap")
	}
}

func TestIssueReplacesOldPin(t *testing.T) {
	store := NewPinStore(time.Minute, "")
	first, err := store.Issue("lam@example.com")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	second, err := store.Issue("lam@example.com")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if first != second && store.Verify("lam@example.com", first) {
		t.Fatal("superseded PIN still works")
	}
	if !store.Verify("lam@example.com", second) {
		t.Fatal("latest PIN rejected")
	}
}

// Dev mode: every address gets the same known code, and the usual single-use
// rules still apply.
func TestFixedPinIsIssuedForEveryAddress(t *testing.T) {
	store := NewPinStore(time.Minute, DevPIN)

	code, err := store.Issue("lam@example.com")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if code != DevPIN {
		t.Fatalf("code = %q, want %q", code, DevPIN)
	}
	other, err := store.Issue("someone@example.com")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if other != DevPIN {
		t.Fatalf("code = %q, want %q", other, DevPIN)
	}

	if store.Verify("lam@example.com", "000000") {
		t.Fatal("wrong PIN accepted")
	}
	if !store.Verify("lam@example.com", DevPIN) {
		t.Fatal("fixed PIN rejected")
	}
	if store.Verify("lam@example.com", DevPIN) {
		t.Fatal("fixed PIN worked twice")
	}
}
