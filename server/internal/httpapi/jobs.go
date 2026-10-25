package httpapi

import (
	"bytes"
	"context"
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
	// timezoneHeader carries the device's IANA zone. It is the anchor the
	// AgentLoop resolves a relative time against, so a client that omits it only
	// costs itself a clarification.
	timezoneHeader = "X-Blurt-Timezone"
)

// createJob is the single capture ingress: JSON for typed text, a WAV blob
// for voice. It returns as soon as the Job is durable; transcription is the
// worker's problem.
func (s *Server) createJob(w http.ResponseWriter, r *http.Request, user store.User) {
	if s.Jobs == nil || s.Audio == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Capture is not configured.")
		return
	}

	turn, format, ok := s.readTurn(w, r)
	if !ok {
		return
	}
	job, err := s.Jobs.Create(r.Context(), user.UserID, turn)
	if err != nil {
		// No Job means no retry path; do not leave a blob behind.
		s.discard(turn.Audio)
		s.fail(w, "create job", err)
		return
	}
	s.enqueue(r.Context(), job)
	// Only a clip has a shape worth echoing back.
	reply := map[string]any{"job": job}
	if turn.Audio != "" {
		reply["audio"] = map[string]any{"sampleRate": format.SampleRate, "channels": format.Channels}
	}
	writeJSON(w, http.StatusCreated, reply)
}

// mediaTypeOf parses the request's content type. It answers a malformed one
// itself, so "" means the response is already written.
func mediaTypeOf(w http.ResponseWriter, r *http.Request) string {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Send audio or JSON.")
		return ""
	}
	return mediaType
}

// clip is one stored upload: the blob's filename, the device zone it arrived
// with, and the shape its header declared.
type clip struct {
	name   string
	zone   string
	format capture.Format
}

// readAudio validates and stores one uploaded clip. It answers its own refusals,
// so ok=false means the response is already written. The caller owns the blob
// from here on, including removing it if the turn it belongs to never lands.
func (s *Server) readAudio(w http.ResponseWriter, r *http.Request) (clip, bool) {
	if r.ContentLength == 0 {
		writeError(w, http.StatusBadRequest, "empty_audio", "The recording is empty.")
		return clip{}, false
	}
	if r.ContentLength > capture.MaxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "audio_too_large", "That recording is too long.")
		return clip{}, false
	}
	if _, err := parseDuration(r.Header.Get(durationHeader)); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_duration", err.Error())
		return clip{}, false
	}
	zone, err := parseTimezone(r.Header.Get(timezoneHeader))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_timezone", err.Error())
		return clip{}, false
	}

	r.Body = http.MaxBytesReader(w, r.Body, capture.MaxUploadBytes)
	header := make([]byte, capture.HeaderBytes)
	n, err := io.ReadFull(r.Body, header)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		if tooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "audio_too_large", "That recording is too long.")
			return clip{}, false
		}
		s.fail(w, "read audio header", err)
		return clip{}, false
	}
	header = header[:n]
	format, err := capture.SniffAudio(header)
	if err != nil {
		// The recorder is the only writer, so a shape it cannot have produced
		// means the upload is not a Blurt capture; the client falls back to
		// the text composer on this code.
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_audio",
			"Audio must be mono 16 kHz WAV.")
		return clip{}, false
	}
	name, err := capture.NewName()
	if err != nil {
		s.fail(w, "mint audio name", err)
		return clip{}, false
	}
	if err := s.Audio.Save(name, io.MultiReader(bytes.NewReader(header), r.Body)); err != nil {
		if tooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "audio_too_large", "That recording is too long.")
			return clip{}, false
		}
		s.fail(w, "save audio", err)
		return clip{}, false
	}
	return clip{name: name, zone: zone, format: format}, true
}

// readText validates the typed half of a turn: one bounded body, and the device
// zone the agent anchors a relative time against. Like readAudio it answers its
// own refusals.
func (s *Server) readText(w http.ResponseWriter, r *http.Request) (text, zone string, ok bool) {
	var body struct {
		Text string `json:"text"`
	}
	if !decodeJSON(w, r, &body) {
		return "", "", false
	}
	text = strings.TrimSpace(body.Text)
	if text == "" || len(text) > maxJobText {
		writeError(w, http.StatusBadRequest, "invalid_text", "Send between 1 and 2000 characters.")
		return "", "", false
	}
	zone, err := parseTimezone(r.Header.Get(timezoneHeader))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_timezone", err.Error())
		return "", "", false
	}
	return text, zone, true
}

// readTurn reads one turn from the request — typed text or a recorded clip. It is
// the whole of the ingress: a capture opens a Job with a turn, a reply adds one to
// a Job that already exists, and both take the same two shapes. The turn is always
// the user's, so its role is set here rather than left to each handler to
// remember; a Job's Create does not set it, and a turn without one is invisible to
// the agent's own read of the conversation.
//
// It answers its own refusals, so ok=false means the response is already written.
// The format is the clip's declared shape, and zero for text.
func (s *Server) readTurn(w http.ResponseWriter, r *http.Request) (turn store.Message, format capture.Format, ok bool) {
	mediaType := mediaTypeOf(w, r)
	if mediaType == "" {
		return store.Message{}, capture.Format{}, false
	}
	switch mediaType {
	case "application/json":
		text, zone, read := s.readText(w, r)
		if !read {
			return store.Message{}, capture.Format{}, false
		}
		return store.Message{Role: "user", Text: text, TZ: zone}, capture.Format{}, true
	case "audio/wav":
		if s.Audio == nil {
			writeError(w, http.StatusNotImplemented, "not_implemented", "Capture is not configured.")
			return store.Message{}, capture.Format{}, false
		}
		recorded, read := s.readAudio(w, r)
		if !read {
			return store.Message{}, capture.Format{}, false
		}
		return store.Message{Role: "user", Audio: recorded.name, TZ: recorded.zone}, recorded.format, true
	default:
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_audio", "Send audio or JSON.")
		return store.Message{}, capture.Format{}, false
	}
}

// replyToJob is the clarification reply: the same Conversation, one more turn. The
// Job goes back to the queue and the AgentLoop picks it up with the answer in hand,
// which is what makes a clarification a normal chat turn instead of a workflow.
//
// The turn may be typed or spoken. Blurt is voice first, so the ingress takes the
// same two shapes a capture does: answering should never cost the user a keyboard.
func (s *Server) replyToJob(w http.ResponseWriter, r *http.Request, user store.User) {
	if s.Jobs == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "Capture is not configured.")
		return
	}
	jobID, err := strconv.ParseInt(r.PathValue("jobId"), 10, 64)
	if err != nil || jobID < 1 {
		writeError(w, http.StatusNotFound, "job_not_found", "Job not found.")
		return
	}

	turn, _, ok := s.readTurn(w, r)
	if !ok {
		return
	}

	job, err := s.Jobs.AppendTurn(r.Context(), user.UserID, jobID, turn,
		map[string]any{"outcome": "reply"})
	if err != nil {
		// A refused reply leaves no Job to retry it, so its clip would be a blob
		// nothing sweeps. Drop it before reporting why the turn bounced.
		s.discard(turn.Audio)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job_not_found", "Job not found.")
		return
	}
	if errors.Is(err, store.ErrJobClosed) {
		writeError(w, http.StatusConflict, "job_closed", "That capture is already finished.")
		return
	}
	if err != nil {
		s.fail(w, "append job turn", err)
		return
	}
	s.enqueue(r.Context(), job)
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

// discard removes a clip whose turn never landed. Best effort: the caller is
// already reporting the real failure.
func (s *Server) discard(name string) {
	if name == "" {
		return
	}
	if err := s.Audio.Remove(name); err != nil {
		slog.Warn("remove rejected audio", "err", err)
	}
}

// parseTimezone validates the device zone at the edge. Absent is fine: the model
// then has no anchor and asks. A zone Blurt cannot load is refused here rather than
// turning into a question about a zone the user never chose.
func parseTimezone(raw string) (string, error) {
	zone := strings.TrimSpace(raw)
	if zone == "" {
		return "", nil
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return "", fmt.Errorf("%s must be an IANA zone.", timezoneHeader)
	}
	return zone, nil
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

// enqueue sends the new Job to its first stage. A failed publish is not fatal:
// the sweeper republishes the open row once Redis is reachable again.
func (s *Server) enqueue(ctx context.Context, job store.Job) {
	if s.Queues == nil {
		return
	}
	stream := s.Queues.AgentQueue
	if _, pending := job.PendingAudio(); pending {
		stream = s.Queues.TranscriptionQueue
	}
	if stream == nil {
		return
	}
	if err := stream.Publish(ctx, job.JobID, 1); err != nil {
		slog.Error("publish job", "job_id", job.JobID, "err", err)
	}
}

func tooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}
