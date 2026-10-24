package mail

import (
	"strings"
	"testing"

	"blurt/internal/config"
)

// SMTP settings are optional in the environment, so the mailer is the last
// line of defence against a deploy that would silently send nothing.
func TestNewRequiresSMTPSettings(t *testing.T) {
	if _, err := New(config.SMTP{}); err == nil {
		t.Fatal("New accepted an empty SMTP config")
	}
	if _, err := New(config.SMTP{Host: "smtp.gmail.com", Port: 587}); err == nil {
		t.Fatal("New accepted a missing From address")
	}
}

// A relay that wants no AUTH gets no credentials: the address is all it sees.
func TestNewSkipsAuthWithoutUsername(t *testing.T) {
	mailer, err := New(config.SMTP{Host: "smtp.gmail.com", Port: 587, From: "a@b.c"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if mailer.auth != nil {
		t.Fatal("New built AUTH without a username")
	}
	if mailer.addr != "smtp.gmail.com:587" {
		t.Fatalf("addr = %q, want smtp.gmail.com:587", mailer.addr)
	}

	withAuth, err := New(config.SMTP{Host: "smtp.gmail.com", Port: 587, From: "a@b.c", Username: "u", Password: "p"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if withAuth.auth == nil {
		t.Fatal("New dropped AUTH for a configured mailbox")
	}
}

func TestPINMessageCarriesRecipientAndCode(t *testing.T) {
	message := string(pinMessage("blurt@example.com", "lam@example.com", "123456"))
	for _, want := range []string{
		"From: Blurt <blurt@example.com>",
		"To: lam@example.com",
		"Subject: Your Blurt login PIN",
		"123456",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("message missing %q:\n%s", want, message)
		}
	}
}
