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
	// DefaultMaxTurns is the AgentLoop's starting budget from the PRD, tuned
	// from Trace data later. It is exported so a loop built without one still
	// shares this single source.
	DefaultMaxTurns = 5
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

// STT is the phonon gateway Blurt sends captures to. The route is the
// gateway's own /v1/infer, not an OpenAI-compatible one: a base64 WAV data URI
// in, {"output": {"text": "..."}} out.
type STT struct {
	URL    string
	APIKey string
	Model  string
}

// Decision is the laya model Blurt asks for typed decisions; today that is the
// transcript-quality gate. It shares the STT gateway, so the URL and key fall
// back to the STT ones and only the model differs.
type Decision struct {
	URL    string
	APIKey string
	Model  string
}

// Agent is the model Blurt asks to turn a capture into records. The gateway's
// /v1/infer serves many chat models, so unlike phonon and laya the model id has
// no default and must be named. MaxTurns is the AgentLoop's per-Job budget from
// the PRD.
type Agent struct {
	URL      string
	APIKey   string
	Model    string
	MaxTurns int
}

// fakeSTTModes are the dev failure-injection modes. They mirror BLURT_FAKE_PIN:
// useful locally and in e2e, fatal on a public host.
var fakeSTTModes = map[string]bool{"ok": true, "error": true, "illegible": true, "flaky": true}

// Config is the fully resolved runtime configuration.
type Config struct {
	DataDir       string   // SQLite lives here; mounted as a docker volume
	RedisURL      string   // Redis Streams URL
	Bind          string   // listen address, e.g. :8080
	PublicOrigins []string // allowed browser origins: public host plus localhost
	SessionSecret string
	EncryptionKey []byte
	// FakePIN is dev only: login accepts a fixed PIN and no mail is sent, so a
	// local stack needs no mailbox. Never set it on a public host.
	FakePIN bool
	// FakeSTT is dev only: a deterministic transcriber replaces the phonon
	// gateway so capture can be exercised without it. Never set it on a public
	// host. The failure-injection modes are unit-test only; the switch stays a
	// bool so it reads the same way as BLURT_FAKE_PIN.
	FakeSTT bool
	SMTP    SMTP
	VAPID   VAPID
	STT     STT
	// Decision is the typed-decision gateway. An empty URL turns the gate off
	// rather than failing boot: it is an advisory filter, not a hard dependency.
	Decision Decision
	// FakeAgent is dev only: a scripted model stands in so the AgentLoop can be
	// exercised without the gateway. Never set it on a public host.
	FakeAgent bool
	// Agent is the model. Unlike the gate it is not advisory — a capture with no
	// model never becomes a record — so a URL and a model are required unless the
	// fake is on.
	Agent Agent
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
		RedisURL:      req("BLURT_REDIS_URL"),
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

	// phonon is reached over the gateway's own transcription route. The fake is
	// the local/e2e escape hatch, so a real URL is required otherwise.
	cfg.FakeSTT = envBool(&problems, "BLURT_FAKE_STT")
	cfg.STT = STT{
		URL:    opt("BLURT_STT_URL"),
		APIKey: opt("BLURT_STT_API_KEY"),
		Model:  opt("BLURT_STT_MODEL"),
	}
	// The gateway serves exactly one audio model and rejects any other id, so
	// the default must be the id it answers to.
	if cfg.STT.Model == "" {
		cfg.STT.Model = "phonon"
	}
	if cfg.STT.URL == "" {
		if !cfg.FakeSTT {
			problems = append(problems, "BLURT_STT_URL is required unless BLURT_FAKE_STT is true")
		}
	} else if err := checkHTTPURL(cfg.STT.URL); err != nil {
		problems = append(problems, "BLURT_STT_URL "+err.Error())
	}

	// laya shares the STT gateway, so only the model is required to differ. With
	// no URL the gate is simply skipped, which is how a fake-STT stack runs.
	cfg.Decision = Decision{
		URL:    opt("BLURT_DECISION_URL"),
		APIKey: opt("BLURT_DECISION_API_KEY"),
		Model:  opt("BLURT_DECISION_MODEL"),
	}
	if cfg.Decision.Model == "" {
		cfg.Decision.Model = "laya"
	}
	if cfg.Decision.URL == "" {
		cfg.Decision.URL = cfg.STT.URL
	}
	if cfg.Decision.APIKey == "" {
		cfg.Decision.APIKey = cfg.STT.APIKey
	}
	if cfg.Decision.URL != "" {
		if err := checkHTTPURL(cfg.Decision.URL); err != nil {
			problems = append(problems, "BLURT_DECISION_URL "+err.Error())
		}
	}

	// The model rides the same gateway. It is not advisory: with no URL the
	// capture path would accept Jobs it can never resolve, so this one is
	// required unless the fake is on.
	cfg.FakeAgent = envBool(&problems, "BLURT_FAKE_AGENT")
	cfg.Agent = Agent{
		URL:      opt("BLURT_AGENT_URL"),
		APIKey:   opt("BLURT_AGENT_API_KEY"),
		Model:    opt("BLURT_AGENT_MODEL"),
		MaxTurns: DefaultMaxTurns,
	}
	if cfg.Agent.URL == "" {
		cfg.Agent.URL = cfg.STT.URL
	}
	if cfg.Agent.APIKey == "" {
		cfg.Agent.APIKey = cfg.STT.APIKey
	}
	if cfg.Agent.URL == "" {
		if !cfg.FakeAgent {
			problems = append(problems, "BLURT_AGENT_URL is required unless BLURT_FAKE_AGENT is true")
		}
	} else if err := checkHTTPURL(cfg.Agent.URL); err != nil {
		problems = append(problems, "BLURT_AGENT_URL "+err.Error())
	}
	if cfg.Agent.Model == "" && !cfg.FakeAgent {
		problems = append(problems, "BLURT_AGENT_MODEL is required unless BLURT_FAKE_AGENT is true")
	}
	if raw := opt("BLURT_AGENT_MAX_TURNS"); raw != "" {
		turns, err := strconv.Atoi(raw)
		if err != nil || turns < 1 {
			problems = append(problems, "BLURT_AGENT_MAX_TURNS must be a positive integer")
		} else {
			cfg.Agent.MaxTurns = turns
		}
	}

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

// checkHTTPURL rejects a typo at boot instead of failing every transcription.
func checkHTTPURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("must be an http(s) URL")
	}
	return nil
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
