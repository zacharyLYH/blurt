package httpapi_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"blurt/internal/store"
)

// wavBytes builds the canonical 44-byte RIFF header the recorder writes. The
// HTTP layer only sniffs the shape, so no samples are needed.
func wavBytes(sampleRate uint32, channels uint16) []byte {
	header := make([]byte, 44)
	copy(header[0:], "RIFF")
	binary.LittleEndian.PutUint32(header[4:], 36)
	copy(header[8:], "WAVE")
	copy(header[12:], "fmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1) // PCM
	binary.LittleEndian.PutUint16(header[22:], channels)
	binary.LittleEndian.PutUint32(header[24:], sampleRate)
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], "data")
	return header
}

func upload(t *testing.T, client *http.Client, url, contentType, duration string, audio []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(audio))
	if err != nil {
		t.Fatalf("build upload: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Origin", testOrigin)
	if duration != "" {
		req.Header.Set("X-Blurt-Duration-Ms", duration)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read upload reply: %v", err)
	}
	return res.StatusCode, body
}

func TestAudioJobStoresClipAndReturnsJob(t *testing.T) {
	client, base, mailer, _ := newClient(t)
	login(t, client, base, "lam@example.com", mailer)

	status, body := upload(t, client, base+"/api/jobs", "audio/wav", "800", wavBytes(16000, 1))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", status, body)
	}
	job := decodeJob(t, body)
	if job.JobID == 0 || job.Status != store.JobOpen {
		t.Fatalf("unexpected job: %+v", job)
	}
	if len(job.Conversation) != 1 {
		t.Fatalf("conversation = %+v", job.Conversation)
	}
	message := job.Conversation[0]
	if message.Role != "user" || message.Audio == "" || message.Text != "" {
		t.Fatalf("unexpected message: %+v", message)
	}

	// Polling is the only way the client sees the transcript arrive.
	status, body = call(t, client, http.MethodGet,
		base+"/api/jobs/"+strconv.FormatInt(job.JobID, 10), nil)
	if status != http.StatusOK {
		t.Fatalf("GET job status = %d: %s", status, body)
	}
	if fetched := decodeJob(t, body); fetched.JobID != job.JobID {
		t.Fatalf("fetched job = %+v, want id %d", fetched, job.JobID)
	}
}

// The recorder is the only writer: a non-mono 16 kHz WAV, a stale Opus client,
// or a duration outside the spend guard is a client bug and is refused here.
func TestAudioJobRejectsBadInput(t *testing.T) {
	client, base, mailer, _ := newClient(t)
	login(t, client, base, "lam@example.com", mailer)

	cases := []struct {
		name        string
		contentType string
		duration    string
		audio       []byte
		status      int
		code        string
	}{
		{"stereo wav", "audio/wav", "800", wavBytes(16000, 2), http.StatusUnsupportedMediaType, "unsupported_audio"},
		{"44.1 kHz wav", "audio/wav", "800", wavBytes(44100, 1), http.StatusUnsupportedMediaType, "unsupported_audio"},
		{"mp4 content type", "audio/mp4", "800", wavBytes(16000, 1), http.StatusUnsupportedMediaType, "unsupported_audio"},
		{"stale opus client", "audio/webm", "800", wavBytes(16000, 1), http.StatusUnsupportedMediaType, "unsupported_audio"},
		{"not audio at all", "audio/wav", "800", []byte("not audio"), http.StatusUnsupportedMediaType, "unsupported_audio"},
		{"missing duration", "audio/wav", "", wavBytes(16000, 1), http.StatusBadRequest, "invalid_duration"},
		{"too short", "audio/wav", "100", wavBytes(16000, 1), http.StatusBadRequest, "invalid_duration"},
		{"too long", "audio/wav", "180000", wavBytes(16000, 1), http.StatusBadRequest, "invalid_duration"},
		{"not a number", "audio/wav", "soon", wavBytes(16000, 1), http.StatusBadRequest, "invalid_duration"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status, body := upload(t, client, base+"/api/jobs", test.contentType, test.duration, test.audio)
			if status != test.status || !strings.Contains(string(body), test.code) {
				t.Fatalf("status = %d, body = %s", status, body)
			}
		})
	}
}

func TestTextJobSkipsTranscription(t *testing.T) {
	client, base, mailer, _ := newClient(t)
	login(t, client, base, "lam@example.com", mailer)

	status, body := call(t, client, http.MethodPost, base+"/api/jobs",
		map[string]string{"text": "  Remind me to buy glue.  "})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", status, body)
	}
	if message := decodeJob(t, body).Conversation[0]; message.Text != "Remind me to buy glue." || message.Audio != "" {
		t.Fatalf("unexpected message: %+v", message)
	}

	if status, body := call(t, client, http.MethodPost, base+"/api/jobs",
		map[string]string{"text": "   "}); status != http.StatusBadRequest {
		t.Fatalf("blank text status = %d, want 400: %s", status, body)
	}
}

func TestJobIsScopedToItsOwner(t *testing.T) {
	first, base, mailer, _ := newClient(t)
	login(t, first, base, "lam@example.com", mailer)
	status, body := call(t, first, http.MethodPost, base+"/api/jobs", map[string]string{"text": "Mine."})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d: %s", status, body)
	}
	job := decodeJob(t, body)
	other := newUserClient(t, base, "other@example.com", mailer)
	status, body = call(t, other, http.MethodGet,
		base+"/api/jobs/"+strconv.FormatInt(job.JobID, 10), nil)
	if status != http.StatusNotFound {
		t.Fatalf("foreign job status = %d, want 404: %s", status, body)
	}
}

// The sidebar lists what is still open, newest first, so a capture stays
// reachable after the user moves on.
func TestListOpenJobsNewestFirst(t *testing.T) {
	client, base, mailer, _ := newClient(t)
	login(t, client, base, "lam@example.com", mailer)

	for _, text := range []string{"Buy glue.", "Book the flight."} {
		if status, body := call(t, client, http.MethodPost, base+"/api/jobs",
			map[string]string{"text": text}); status != http.StatusCreated {
			t.Fatalf("create %q status = %d: %s", text, status, body)
		}
	}

	status, body := call(t, client, http.MethodGet, base+"/api/jobs", nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d: %s", status, body)
	}
	var reply struct {
		Jobs []store.OpenJob `json:"jobs"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	if len(reply.Jobs) != 2 {
		t.Fatalf("jobs = %+v, want 2", reply.Jobs)
	}
	// Newest first, and each row carries the preview the sidebar renders rather
	// than the whole conversation the list would otherwise have to poll for.
	if reply.Jobs[0].Preview != "Book the flight." || reply.Jobs[1].Preview != "Buy glue." {
		t.Fatalf("previews = %q, %q", reply.Jobs[0].Preview, reply.Jobs[1].Preview)
	}
	if reply.Jobs[0].Status != store.JobOpen || reply.Jobs[0].CreatedAt == "" {
		t.Fatalf("row = %+v", reply.Jobs[0])
	}
}

// An untranscribed clip has no words yet, so its row carries an empty preview
// and the client falls back to its own copy.
func TestListOpenJobsHasNoPreviewBeforeTranscription(t *testing.T) {
	client, base, mailer, _ := newClient(t)
	login(t, client, base, "lam@example.com", mailer)

	if status, body := upload(t, client, base+"/api/jobs", "audio/wav", "800", wavBytes(16000, 1)); status != http.StatusCreated {
		t.Fatalf("upload status = %d: %s", status, body)
	}

	status, body := call(t, client, http.MethodGet, base+"/api/jobs", nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d: %s", status, body)
	}
	var reply struct {
		Jobs []store.OpenJob `json:"jobs"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	if len(reply.Jobs) != 1 || reply.Jobs[0].Preview != "" {
		t.Fatalf("jobs = %+v, want one row with an empty preview", reply.Jobs)
	}
}

// An empty sidebar is [] rather than null, so the client can map over it, and
// one user never sees another's Jobs.
func TestListOpenJobsIsEmptyAndScoped(t *testing.T) {
	first, base, mailer, _ := newClient(t)
	login(t, first, base, "lam@example.com", mailer)
	if status, body := call(t, first, http.MethodPost, base+"/api/jobs",
		map[string]string{"text": "Mine."}); status != http.StatusCreated {
		t.Fatalf("create status = %d: %s", status, body)
	}

	other := newUserClient(t, base, "other@example.com", mailer)
	status, body := call(t, other, http.MethodGet, base+"/api/jobs", nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d: %s", status, body)
	}
	if strings.Contains(string(body), "Mine.") {
		t.Fatalf("another user's job leaked into the sidebar: %s", body)
	}
	if !strings.Contains(string(body), `"jobs":[]`) {
		t.Fatalf("empty sidebar = %s, want an empty array", body)
	}
}
