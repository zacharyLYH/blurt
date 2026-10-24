package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"blurt/internal/capture"
	"blurt/internal/store"
)

const (
	// durationHeader is the client's capture length. A browser cannot lie about
	// the blob it sends, but this hint still keeps an honest client honest and
	// stops the obvious oversized upload before STT runs.
	durationHeader = "X-Blurt-Duration-Ms"
	// maxJobText keeps one text capture from becoming an unbounded Job row.
	maxJobText = 2000
)

// createJob is the single capture ingress: JSON for typed text, a WAV blob
// for voice. It returns as soon as the Job is durable; transcription is the
// worker's problem.
func (s *Server) createJob(w http.ResponseWriter, r *http.Request, user store.User) {
	if s.Jobs == nil || s.Audio == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Capture is not configured.")
		return
	}

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Send audio or JSON.")
		return
	}
	if mediaType == "application/json" {
		s.createTextJob(w, r, user)
		return
	}
	if mediaType != "audio/wav" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_audio",
			"Audio must be a WAV recording.")
		return
	}
	if r.ContentLength == 0 {
		writeError(w, http.StatusBadRequest, "empty_audio", "The recording is empty.")
		return
	}
	if r.ContentLength > capture.MaxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "audio_too_large", "That recording is too long.")
		return
	}
	if _, err := parseDuration(r.Header.Get(durationHeader)); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_duration", err.Error())
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, capture.MaxUploadBytes)
	header := make([]byte, capture.HeaderBytes)
	n, err := io.ReadFull(r.Body, header)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		if tooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "audio_too_large", "That recording is too long.")
			return
		}
		s.fail(w, "read audio header", err)
		return
	}
	header = header[:n]
	format, err := capture.SniffAudio(header)
	if err != nil {
		// The recorder is the only writer, so a shape it cannot have produced
		// means the upload is not a Blurt capture; the client falls back to
		// the text composer on this code.
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_audio",
			"Audio must be mono 16 kHz WAV.")
		return
	}
	name, err := capture.NewName()
	if err != nil {
		s.fail(w, "mint audio name", err)
		return
	}
	if err := s.Audio.Save(name, io.MultiReader(bytes.NewReader(header), r.Body)); err != nil {
		if tooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "audio_too_large", "That recording is too long.")
			return
		}
		s.fail(w, "save audio", err)
		return
	}

	job, err := s.Jobs.Create(r.Context(), user.UserID, store.Message{Role: "user", Audio: name})
	if err != nil {
		// No Job means no retry path; do not leave a blob behind.
		if removeErr := s.Audio.Remove(name); removeErr != nil {
			slog.Error("remove orphaned audio", "err", removeErr)
		}
		s.fail(w, "create audio job", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"job":   job,
		"audio": map[string]any{"sampleRate": format.SampleRate, "channels": format.Channels},
	})
}

// createTextJob is the fallback when the mic is unavailable or the user would
// rather type. It skipped STT entirely, so it is queued for the agent as-is.
func (s *Server) createTextJob(w http.ResponseWriter, r *http.Request, user store.User) {
	var body struct {
		Text string `json:"text"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" || len(text) > maxJobText {
		writeError(w, http.StatusBadRequest, "invalid_text", "Send between 1 and 2000 characters.")
		return
	}
	job, err := s.Jobs.Create(r.Context(), user.UserID, store.Message{Role: "user", Text: text})
	if err != nil {
		s.fail(w, "create text job", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"job": job})
}

// listJobs backs the open-conversation sidebar: every Job that is not closed
// yet, newest first, scoped to the caller.
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request, user store.User) {
	if s.Jobs == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Capture is not configured.")
		return
	}
	jobs, err := s.Jobs.OpenJobs(r.Context(), user.UserID)
	if err != nil {
		s.fail(w, "list open jobs", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request, user store.User) {
	if s.Jobs == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Capture is not configured.")
		return
	}
	jobID, err := strconv.ParseInt(r.PathValue("jobId"), 10, 64)
	if err != nil || jobID < 1 {
		writeError(w, http.StatusNotFound, "job_not_found", "Job not found.")
		return
	}
	job, err := s.Jobs.ByID(r.Context(), user.UserID, jobID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job_not_found", "Job not found.")
		return
	}
	if err != nil {
		s.fail(w, "load job", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

// parseDuration enforces the one-minute cap. Absent is invalid for an audio
// upload because the client always knows how long it recorded.
func parseDuration(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, fmt.Errorf("Missing %s header.", durationHeader)
	}
	ms, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s must be milliseconds.", durationHeader)
	}
	duration := time.Duration(ms) * time.Millisecond
	if duration < capture.MinDuration {
		return 0, fmt.Errorf("That recording is too short.")
	}
	if duration > capture.MaxDuration {
		return 0, fmt.Errorf("That recording is longer than one minute.")
	}
	return duration, nil
}

func tooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}
