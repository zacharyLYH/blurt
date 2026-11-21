// Package stt turns a captured audio blob into text. phonon is reached over the
// gateway's OpenAI-compatible audio route: multipart/form-data with a `file`
// part, and the transcript comes back as {"text": "..."}. The HTTP client is
// behind an interface because the worker's retry and fallback rules are what we
// test.
package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"time"

	"blurt/internal/config"
	"blurt/internal/failure"
)

const (
	// audioFilename gives the multipart part a name the runtime can key on.
	audioFilename = "capture.wav"
	// audioContentType is the only shape Blurt captures.
	audioContentType = "audio/wav"
)

// FakeOK is the mode the server boots with when BLURT_FAKE_STT is on: every
// clip comes back as FakeTranscript. The other modes exist for the worker's
// retry and fallback tests.
const FakeOK = "ok"

// FakeTranscript is deterministic so e2e can assert on what the Job stores.
const FakeTranscript = "Remind me to buy glue on the way home."

// Transcriber is one transcription attempt. A blank result with no error means
// the clip was not legible, which is permanent rather than retryable.
type Transcriber interface {
	Transcribe(ctx context.Context, audio io.Reader) (string, error)
}

// HTTP calls phonon over the gateway's OpenAI audio route. It sends a
// multipart/form-data body with one `file` part and reads {"text": "..."}.
type HTTP struct {
	url    string
	apiKey string
	model  string
	client *http.Client
}

// Fake is the dev transcriber (BLURT_FAKE_STT). Modes inject the failures the
// worker must survive: error is transient, illegible is a blank transcript,
// flaky fails once then succeeds.
type Fake struct {
	mode  string
	mu    sync.Mutex
	calls int
}

// NewHTTP points the client at the phonon gateway.
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

// transcriptionReply needs only the text. Timestamps are never requested, so
// the answer stays one line instead of a segment list.
type transcriptionReply struct {
	Text string `json:"text"`
}

func (h *HTTP) Transcribe(ctx context.Context, audio io.Reader) (string, error) {
	raw, err := io.ReadAll(audio)
	if err != nil {
		return "", fmt.Errorf("stt: read audio: %w", err)
	}
	body, contentType, err := h.form(raw)
	if err != nil {
		return "", fmt.Errorf("stt: build request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("stt: build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	if h.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.apiKey)
	}

	res, err := h.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("stt: call phonon: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", failure.ReadRefusal("stt", res)
	}

	var reply transcriptionReply
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&reply); err != nil {
		return "", fmt.Errorf("stt: decode transcript: %w", err)
	}
	return strings.TrimSpace(reply.Text), nil
}

// form builds the OpenAI audio body: one WAV file part plus the model id. The
// gateway pins its configured checkpoint, so `model` is only there for clients
// that expect the OpenAI field.
func (h *HTTP) form(raw []byte) ([]byte, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename="%s"`, audioFilename))
	header.Set("Content-Type", audioContentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(raw); err != nil {
		return nil, "", err
	}
	if h.model != "" {
		if err := writer.WriteField("model", h.model); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

// NewFake returns a transcriber for a mode the worker tests inject.
func NewFake(mode string) *Fake {
	return &Fake{mode: mode}
}
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
