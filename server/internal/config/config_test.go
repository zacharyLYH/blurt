package config

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

// testVAPIDKeys mints a real pair, because the loader now proves the public
// key is the one the private key signs for.
func testVAPIDKeys(t *testing.T) (publicKey, privateKey string) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(key.Bytes())
}

func setValid(t *testing.T) {
	t.Helper()
	publicKey, privateKey := testVAPIDKeys(t)
	env := map[string]string{
		"BLURT_DATA_DIR":          "/data",
		"BLURT_REDIS_URL":         "redis://localhost:6379/0",
		"BLURT_BIND":              ":8080",
		"BLURT_PUBLIC_ORIGINS":    "https://blurt.bytesbylyh.dev",
		"BLURT_SESSION_SECRET":    strings.Repeat("s", 32),
		"BLURT_ENCRYPTION_KEY":    base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"BLURT_SMTP_HOST":         "smtp.gmail.com",
		"BLURT_SMTP_PORT":         "587",
		"BLURT_SMTP_USERNAME":     "sender@example.com",
		"BLURT_SMTP_PASSWORD":     "app-password",
		"BLURT_SMTP_FROM":         "sender@example.com",
		"BLURT_VAPID_PUBLIC_KEY":  publicKey,
		"BLURT_VAPID_PRIVATE_KEY": privateKey,
		"BLURT_VAPID_SUBJECT":     "mailto:sender@example.com",
		"BLURT_STT_URL":           "https://stt.example.com/v1/audio/transcriptions",
		// The model is required whenever the STT gateway is, so it belongs in
		// the baseline; its own cases are their own test.
		"BLURT_AGENT_MODEL": "local-model",
	}
	for name, value := range env {
		t.Setenv(name, value)
	}
}

func TestLoadValid(t *testing.T) {
	setValid(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SMTP.From != "sender@example.com" || cfg.SMTP.Port != 587 {
		t.Fatalf("unexpected SMTP config: %+v", cfg.SMTP)
	}
	if len(cfg.EncryptionKey) != 32 {
		t.Fatalf("encryption key length = %d, want 32", len(cfg.EncryptionKey))
	}
}

func TestLoadReportsEveryMissingValue(t *testing.T) {
	// t.Setenv with an empty value is indistinguishable from unset here.
	for _, name := range []string{
		"BLURT_DATA_DIR", "BLURT_REDIS_URL", "BLURT_BIND", "BLURT_SESSION_SECRET", "BLURT_ENCRYPTION_KEY",
		"BLURT_VAPID_PUBLIC_KEY", "BLURT_VAPID_PRIVATE_KEY", "BLURT_VAPID_SUBJECT",
	} {
		t.Setenv(name, "")
	}

	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded with an empty environment")
	}
	message := err.Error()
	for _, name := range []string{
		"BLURT_VAPID_PRIVATE_KEY is required",
		"BLURT_DATA_DIR is required",
	} {
		if !strings.Contains(message, name) {
			t.Fatalf("error missing %q:\n%s", name, message)
		}
	}
}

// SMTP is optional: the e2e build records PINs in-process and a local stack
// starts without mail. mail.New is what rejects a half-configured mailbox.
func TestSMTPIsOptional(t *testing.T) {
	setValid(t)
	for _, name := range []string{
		"BLURT_SMTP_HOST", "BLURT_SMTP_PORT", "BLURT_SMTP_USERNAME",
		"BLURT_SMTP_PASSWORD", "BLURT_SMTP_FROM",
	} {
		t.Setenv(name, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load without SMTP: %v", err)
	}
	if cfg.SMTP.Host != "" || cfg.SMTP.Port != 0 || cfg.SMTP.From != "" {
		t.Fatalf("SMTP = %+v, want empty", cfg.SMTP)
	}
}

func TestOriginsLocalhostOnlyWithoutPublicOrigin(t *testing.T) {
	setValid(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"https://blurt.bytesbylyh.dev"}
	if len(cfg.PublicOrigins) != len(want) || cfg.PublicOrigins[0] != want[0] {
		t.Fatalf("origins = %v, want %v", cfg.PublicOrigins, want)
	}

	t.Setenv("BLURT_PUBLIC_ORIGINS", "")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load without a public origin: %v", err)
	}
	want = []string{"http://localhost:9998", "http://localhost:5173"}
	if len(cfg.PublicOrigins) != len(want) {
		t.Fatalf("origins = %v, want %v", cfg.PublicOrigins, want)
	}
	for i, origin := range want {
		if cfg.PublicOrigins[i] != origin {
			t.Fatalf("origins = %v, want %v", cfg.PublicOrigins, want)
		}
	}
}

func TestSMTPPasswordNormalization(t *testing.T) {
	setValid(t)
	t.Setenv("BLURT_SMTP_PASSWORD", "abcd efgh ijkl mnop")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SMTP.Password != "abcdefghijklmnop" {
		t.Fatalf("password = %q, want spaces removed", cfg.SMTP.Password)
	}
}

// BLURT_FAKE_PIN is the dev switch: off unless explicitly true, and a typo is
// an error rather than a silently disabled flag.
func TestFakePIN(t *testing.T) {
	setValid(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.FakePIN {
		t.Fatal("FakePIN defaulted on")
	}

	t.Setenv("BLURT_FAKE_PIN", "true")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load with BLURT_FAKE_PIN: %v", err)
	}
	if !cfg.FakePIN {
		t.Fatal("BLURT_FAKE_PIN=true did not turn FakePIN on")
	}
}

// A match is what a correct deploy has: the public half is the one the private
// half signs for. Getting this wrong is the classic copy-paste mistake, and
// every push would then fail with a relay 403.
func TestLoadRejectsMismatchedVAPIDPair(t *testing.T) {
	setValid(t)
	publicKey, _ := testVAPIDKeys(t)
	t.Setenv("BLURT_VAPID_PUBLIC_KEY", publicKey)

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Load with a mismatched pair: %v", err)
	}
}

func TestLoadRejectsMalformedSecrets(t *testing.T) {
	setValid(t)
	t.Setenv("BLURT_SMTP_PORT", "not-a-port")
	t.Setenv("BLURT_SESSION_SECRET", "short")
	t.Setenv("BLURT_VAPID_PUBLIC_KEY", "not base64!")
	t.Setenv("BLURT_FAKE_PIN", "yes please")
	t.Setenv("BLURT_PUBLIC_ORIGINS", "https://blurt.bytesbylyh.dev/app")
	t.Setenv("BLURT_FAKE_STT", "yes please")
	t.Setenv("BLURT_STT_URL", "not-a-url")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted malformed values")
	}
	for _, want := range []string{
		"BLURT_SMTP_PORT", "BLURT_SESSION_SECRET",
		"BLURT_VAPID_PUBLIC_KEY", "BLURT_FAKE_PIN", "BLURT_PUBLIC_ORIGINS",
		"BLURT_FAKE_STT", "BLURT_STT_URL",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q:\n%s", want, err)
		}
	}
}

// Real STT is required in production; the fake is the local/e2e escape hatch.
func TestSTTRequiresURLUnlessFake(t *testing.T) {
	setValid(t)
	t.Setenv("BLURT_STT_URL", "")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BLURT_STT_URL is required") {
		t.Fatalf("Load without an STT URL: %v", err)
	}

	// With phonon faked there is no gateway at all, so the model that shares
	// that gateway has to be faked too — which is what the e2e stack does.
	t.Setenv("BLURT_FAKE_STT", "true")
	t.Setenv("BLURT_FAKE_AGENT", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with BLURT_FAKE_STT: %v", err)
	}
	if !cfg.FakeSTT {
		t.Fatal("FakeSTT = false, want true")
	}
	if cfg.STT.Model != "phonon" {
		t.Fatalf("STT model = %q, want default phonon", cfg.STT.Model)
	}
}

// The model is not advisory like the gate: a capture with no model could
// never become a record, so its settings are required unless the fake is on, and
// the per-Job turn budget is a positive number rather than a silent default.
func TestAgentModelConfig(t *testing.T) {
	setValid(t)
	t.Setenv("BLURT_AGENT_MODEL", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BLURT_AGENT_MODEL is required") {
		t.Fatalf("Load without a model model: %v", err)
	}

	t.Setenv("BLURT_FAKE_AGENT", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with BLURT_FAKE_AGENT: %v", err)
	}
	if !cfg.FakeAgent {
		t.Fatal("FakeAgent = false, want true")
	}
	if cfg.Agent.MaxTurns != DefaultMaxTurns {
		t.Fatalf("max turns = %d, want the default %d", cfg.Agent.MaxTurns, DefaultMaxTurns)
	}

	t.Setenv("BLURT_FAKE_AGENT", "false")
	t.Setenv("BLURT_AGENT_MODEL", "local-model")
	t.Setenv("BLURT_AGENT_MAX_TURNS", "4")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.MaxTurns != 4 {
		t.Fatalf("max turns = %d, want 4", cfg.Agent.MaxTurns)
	}
	// The model rides the same gateway as phonon and laya, so only the model
	// id has to be named.
	if cfg.Agent.URL != cfg.STT.URL || cfg.Agent.APIKey != cfg.STT.APIKey {
		t.Fatalf("agent gateway = %+v, want the STT gateway", cfg.Agent)
	}

	t.Setenv("BLURT_AGENT_MAX_TURNS", "0")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BLURT_AGENT_MAX_TURNS") {
		t.Fatalf("Load with a zero turn budget: %v", err)
	}
}

// laya shares the STT gateway: a deploy that sets only BLURT_STT_* still gets a
// working gate, and with the fake on there is no gateway, so the gate is off.
func TestDecisionDefaultsToSTTGateway(t *testing.T) {
	setValid(t)
	t.Setenv("BLURT_STT_API_KEY", "stt-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Decision.Model != "laya" {
		t.Fatalf("decision model = %q, want the default laya", cfg.Decision.Model)
	}
	if cfg.Decision.URL != cfg.STT.URL || cfg.Decision.APIKey != cfg.STT.APIKey {
		t.Fatalf("decision gateway = %+v, want the STT gateway", cfg.Decision)
	}

	// The fake has no gateway to ask, so the gate stays off rather than failing
	// the boot. The model shares that gateway, so it takes its fake too.
	t.Setenv("BLURT_FAKE_STT", "true")
	t.Setenv("BLURT_FAKE_AGENT", "true")
	t.Setenv("BLURT_STT_URL", "")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load with the fake: %v", err)
	}
	if cfg.Decision.URL != "" {
		t.Fatalf("decision URL = %q, want empty so the gate is skipped", cfg.Decision.URL)
	}
	if cfg.Decision.Model != "laya" {
		t.Fatalf("decision model = %q, want laya even with no gateway", cfg.Decision.Model)
	}

	// An explicit override still has to be a usable URL.
	t.Setenv("BLURT_FAKE_STT", "false")
	t.Setenv("BLURT_STT_URL", "https://stt.example.com/v1/infer")
	t.Setenv("BLURT_DECISION_URL", "not-a-url")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BLURT_DECISION_URL") {
		t.Fatalf("Load with a malformed decision URL: %v", err)
	}
}
