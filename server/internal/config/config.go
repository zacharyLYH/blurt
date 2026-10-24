// Package config loads every setting Blurt needs from the environment.
// Load returns one error listing everything missing or malformed, so a bad
// deploy never boots half-configured. Secrets are never included in errors.
package config

import (
	"bytes"
	"crypto/ecdh"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	// defaultOrigins are the local dev origins, used only when no public host
	// is configured. A deploy that sets BLURT_PUBLIC_ORIGINS trusts exactly
	// what it lists, not localhost.
	defaultOrigins = "http://localhost:9998,http://localhost:5173"
	// minSessionSecretLen keeps opaque session tokens unguessable even if the
	// signing material is reused for hashing.
	minSessionSecretLen = 32
	// encryptionKeyLen is the AES-256 key size for model API keys at rest.
	encryptionKeyLen = 32
	// vapidPublicKeyLen is an uncompressed P-256 point (0x04 + X + Y).
	vapidPublicKeyLen = 65
	// vapidPrivateKeyLen is a P-256 scalar.
	vapidPrivateKeyLen = 32
)

// SMTP describes the outbound mailbox that sends login PINs.
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

// VAPID is the Web Push application server identity used for notifications.
type VAPID struct {
	PublicKey  string
	PrivateKey string
	Subject    string
}

// Config is the fully resolved runtime configuration.
type Config struct {
	DataDir       string   // SQLite lives here; mounted as a docker volume
	Bind          string   // listen address, e.g. :8080
	PublicOrigins []string // allowed browser origins: public host plus localhost
	SessionSecret string
	EncryptionKey []byte
	// FakePIN is dev only: login accepts a fixed PIN and no mail is sent, so a
	// local stack needs no mailbox. Never set it on a public host.
	FakePIN bool
	SMTP    SMTP
	VAPID   VAPID
}

// Load reads the environment and returns every problem it finds at once.
func Load() (*Config, error) {
	var problems []string
	req := func(key string) string {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" {
			problems = append(problems, key+" is required")
		}
		return value
	}
	opt := func(key string) string { return strings.TrimSpace(os.Getenv(key)) }

	cfg := &Config{
		DataDir:       req("BLURT_DATA_DIR"),
		Bind:          req("BLURT_BIND"),
		SessionSecret: req("BLURT_SESSION_SECRET"),
	}
	cfg.PublicOrigins = parseOrigins(&problems, os.Getenv("BLURT_PUBLIC_ORIGINS"))

	if len(cfg.SessionSecret) > 0 && len(cfg.SessionSecret) < minSessionSecretLen {
		problems = append(problems, fmt.Sprintf("BLURT_SESSION_SECRET must be at least %d characters", minSessionSecretLen))
	}

	if raw := req("BLURT_ENCRYPTION_KEY"); raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(key) != encryptionKeyLen {
			problems = append(problems, fmt.Sprintf("BLURT_ENCRYPTION_KEY must be base64 of exactly %d bytes", encryptionKeyLen))
		} else {
			cfg.EncryptionKey = key
		}
	}

	// SMTP is optional here: the server boots without mail so tests and tools
	// can run, and mail.New is what refuses a half-configured mailbox.
	cfg.SMTP = SMTP{
		Host:     opt("BLURT_SMTP_HOST"),
		Username: opt("BLURT_SMTP_USERNAME"),
		// Gmail shows App Passwords in groups of four; SMTP wants them joined.
		Password: strings.ReplaceAll(opt("BLURT_SMTP_PASSWORD"), " ", ""),
		From:     opt("BLURT_SMTP_FROM"),
	}
	if raw := opt("BLURT_SMTP_PORT"); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			problems = append(problems, "BLURT_SMTP_PORT must be a port number")
		} else {
			cfg.SMTP.Port = port
		}
	}

	cfg.FakePIN = envBool(&problems, "BLURT_FAKE_PIN")

	cfg.VAPID = VAPID{
		PublicKey:  req("BLURT_VAPID_PUBLIC_KEY"),
		PrivateKey: req("BLURT_VAPID_PRIVATE_KEY"),
		Subject:    req("BLURT_VAPID_SUBJECT"),
	}
	checkVAPIDPair(&problems,
		checkWebPushKey(&problems, "BLURT_VAPID_PUBLIC_KEY", cfg.VAPID.PublicKey, vapidPublicKeyLen),
		checkWebPushKey(&problems, "BLURT_VAPID_PRIVATE_KEY", cfg.VAPID.PrivateKey, vapidPrivateKeyLen))
	if subject := cfg.VAPID.Subject; subject != "" && !strings.Contains(subject, ":") {
		problems = append(problems, "BLURT_VAPID_SUBJECT must be a mailto: or https: URI")
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  %s", strings.Join(problems, "\n  "))
	}
	return cfg, nil
}

// parseOrigins splits the comma-separated allowlist. Localhost defaults are
// used only when no public origin is set, so a real deploy never trusts
// localhost by accident.
func parseOrigins(problems *[]string, raw string) []string {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = defaultOrigins
	}

	var origins []string
	seen := map[string]bool{}
	valid := true
	add := func(value string) {
		value = strings.TrimRight(strings.TrimSpace(value), "/")
		if value == "" || seen[value] {
			return
		}
		parsed, err := url.Parse(value)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
			(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" {
			valid = false
			return
		}
		seen[value] = true
		origins = append(origins, value)
	}
	for _, part := range strings.Split(value, ",") {
		add(part)
	}
	if !valid {
		*problems = append(*problems, "BLURT_PUBLIC_ORIGINS must be comma-separated http(s) origins")
	}
	return origins
}

// envBool reads an optional switch, reporting a typo instead of quietly
// leaving it off.
func envBool(problems *[]string, key string) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return false
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		*problems = append(*problems, key+" must be true or false")
	}
	return value
}

// checkWebPushKey returns the decoded key, or nil when it already reported a
// problem, so the pair check never repeats a complaint.
func checkWebPushKey(problems *[]string, name, value string, want int) []byte {
	if value == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		*problems = append(*problems, name+" must be unpadded base64url")
		return nil
	}
	if len(raw) != want {
		*problems = append(*problems, fmt.Sprintf("%s must decode to %d bytes", name, want))
		return nil
	}
	return raw
}

// checkVAPIDPair proves the public key is the one the private key signs for.
// A copy-paste mismatch only shows up later as a relay 403 on every push, so
// it is worth catching at boot.
func checkVAPIDPair(problems *[]string, publicKey, privateKey []byte) {
	if publicKey == nil || privateKey == nil {
		return
	}
	key, err := ecdh.P256().NewPrivateKey(privateKey)
	if err != nil {
		*problems = append(*problems, "BLURT_VAPID_PRIVATE_KEY is not a P-256 scalar")
		return
	}
	if !bytes.Equal(key.PublicKey().Bytes(), publicKey) {
		*problems = append(*problems, "BLURT_VAPID_PUBLIC_KEY does not match BLURT_VAPID_PRIVATE_KEY")
	}
}
