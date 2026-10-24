// Package stt turns a captured audio blob into text. phonon is reached over the
// gateway's own route, which is not OpenAI's: the body carries the audio as a
// base64 WAV data URI and the transcript comes back under output.text. The HTTP
// client is behind an interface because the worker's retry and fallback rules
// are what we test.
package stt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"blurt/internal/config"
)

// Transcriber is one transcription attempt. A blank result with no error means
// the clip was not legible, which is permanent rather than retryable.
type Transcriber interface {
	Transcribe(ctx context.Context, audio io.Reader) (string, error)
}

// Error is an HTTP-level refusal from the STT service. Status 0 is never used
// here: network failures stay ordinary errors and are retried.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("stt: %d %s", e.Status, e.Message)
}

// Retryable is false for a 4xx that a retry cannot fix (bad key, bad clip).
func (e *Error) Retryable() bool {
	return e.Status == http.StatusRequestTimeout || e.Status == http.StatusTooManyRequests ||
		e.Status >= http.StatusInternalServerError
}

// HTTP calls phonon. The route is the gateway's own /v1/infer; it takes a
// base64 WAV data URI and answers {"output": {"text": "..."}}.
type HTTP struct {
	url    string
	apiKey string
	model  string
	client *http.Client
}

// audioPrefix declares the payload as a WAV data URI. Blurt only ever captures
// WAV, so it is a constant rather than a parameter.
const audioPrefix = "data:audio/wav;base64,"

func NewHTTP(cfg config.STT) *HTTP {
	return &HTTP{
		url:    cfg.URL,
		apiKey: cfg.APIKey,
		model:  cfg.Model,
		// The gateway decodes a one-minute clip in seconds, but the host is
		// shared and measured at 30s under load, so this is deliberately
		// generous; the worker's attempt timeout sits just above it.
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

// inferRequest is the gateway's body: model plus a payload object.
type inferRequest struct {
	Model   string `json:"model"`
	Payload struct {
		Audio string `json:"audio"`
	} `json:"payload"`
}

// inferReply needs only the text. Timestamps are never requested, so the
// answer stays one line instead of a segment list.
type inferReply struct {
	Output struct {
		Text string `json:"text"`
	} `json:"output"`
}

func (t *HTTP) Transcribe(ctx context.Context, audio io.Reader) (string, error) {
	raw, err := io.ReadAll(audio)
	if err != nil {
		return "", fmt.Errorf("stt: read audio: %w", err)
	}
	request := inferRequest{Model: t.model}
	request.Payload.Audio = audioPrefix + base64.StdEncoding.EncodeToString(raw)
	body, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("stt: build request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("stt: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}

	res, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("stt: call phonon: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", refusal(res)
	}

	var reply inferReply
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&reply); err != nil {
		return "", fmt.Errorf("stt: decode transcript: %w", err)
	}
	return strings.TrimSpace(reply.Output.Text), nil
}

// refusal turns the gateway's error body into the refusal the worker reasons
// about: {"error": {"code": "unauthorized", "message": "missing or invalid api key"}}.
func refusal(res *http.Response) *Error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	// A gateway that answers with something else still yields the status,
	// which is what Retryable actually needs.
	_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body)
	message := strings.TrimSpace(body.Error.Message)
	if message == "" {
		message = strings.TrimSpace(body.Error.Code)
	}
	return &Error{Status: res.StatusCode, Message: message}
}

// Fake is the dev transcriber (BLURT_FAKE_STT). Modes inject the failures the
// worker must survive: error is transient, illegible is a blank transcript,
// flaky fails once then succeeds.
type Fake struct {
	mode  string
	mu    sync.Mutex
	calls int
}

func NewFake(mode string) *Fake {
	return &Fake{mode: mode}
}

// FakeOK is the mode the server boots with when BLURT_FAKE_STT is on: every
// clip comes back as FakeTranscript. The other modes exist for the worker's
// retry and fallback tests.
const FakeOK = "ok"

// FakeTranscript is deterministic so e2e can assert on what the Job stores.
const FakeTranscript = "Remind me to buy glue on the way home."

func (f *Fake) Transcribe(_ context.Context, audio io.Reader) (string, error) {
	_, _ = io.Copy(io.Discard, audio)
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()

	switch f.mode {
	case "error":
		return "", errors.New("fake stt: injected failure")
	case "illegible":
		return "", nil
	case "flaky":
		if call == 1 {
			return "", errors.New("fake stt: injected first-attempt failure")
		}
	}
	return FakeTranscript, nil
}
