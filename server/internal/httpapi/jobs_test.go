package httpapi_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
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
	var reply struct {
		Job store.Job `json:"job"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	if reply.Job.JobID == 0 || reply.Job.Status != store.JobQueued {
		t.Fatalf("unexpected job: %+v", reply.Job)
	}
	if len(reply.Job.Conversation) != 1 {
		t.Fatalf("conversation = %+v", reply.Job.Conversation)
	}
	message := reply.Job.Conversation[0]
	if message.Role != "user" || message.Audio == "" || message.Text != "" {
		t.Fatalf("unexpected message: %+v", message)
	}

	// Polling is the only way the client sees the transcript arrive.
	status, body = call(t, client, http.MethodGet,
		base+"/api/jobs/"+strconv.FormatInt(reply.Job.JobID, 10), nil)
	if status != http.StatusOK {
		t.Fatalf("GET job status = %d: %s", status, body)
	}
	var fetched struct {
		Job store.Job `json:"job"`
	}
	if err := json.Unmarshal(body, &fetched); err != nil {
		t.Fatalf("decode fetched job: %v", err)
	}
	if fetched.Job.JobID != reply.Job.JobID {
		t.Fatalf("fetched job = %+v, want id %d", fetched.Job, reply.Job.JobID)
	}
}

// The recorder is the only writer, so anything that is not a mono 16 kHz WAV is
// a client bug — or a stale cached client still sending Opus. Rejecting it here
// is what makes the fallback copy honest.
func TestAudioJobRejectsWrongFormat(t *testing.T) {
	client, base, mailer, _ := newClient(t)
	login(t, client, base, "lam@example.com", mailer)

	for _, test := range []struct {
		name        string
		contentType string
		audio       []byte
	}{
		{"stereo wav", "audio/wav", wavBytes(16000, 2)},
		{"44.1 kHz wav", "audio/wav", wavBytes(44100, 1)},
		{"mp4 content type", "audio/mp4", wavBytes(16000, 1)},
		{"stale opus client", "audio/webm", wavBytes(16000, 1)},
		{"not audio at all", "audio/wav", []byte("not audio")},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := upload(t, client, base+"/api/jobs", test.contentType, "800", test.audio)
			if status != http.StatusUnsupportedMediaType || !strings.Contains(string(body), "unsupported_audio") {
				t.Fatalf("status = %d, body = %s", status, body)
			}
		})
	}
}

// The duration header is the spend guard the PRD asks for. It is required
// because the client always knows the length of its own clip.
func TestAudioJobRequiresSaneDuration(t *testing.T) {
	client, base, mailer, _ := newClient(t)
	login(t, client, base, "lam@example.com", mailer)

	for _, test := range []struct{ name, duration string }{
		{"missing", ""},
		{"too short", "100"},
		{"too long", "180000"},
		{"not a number", "soon"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := upload(t, client, base+"/api/jobs", "audio/wav", test.duration, wavBytes(16000, 1))
			if status != http.StatusBadRequest || !strings.Contains(string(body), "invalid_duration") {
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
	var reply struct {
		Job store.Job `json:"job"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	if message := reply.Job.Conversation[0]; message.Text != "Remind me to buy glue." || message.Audio != "" {
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
	var reply struct {
		Job store.Job `json:"job"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode job: %v", err)
	}

	second, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	other := &http.Client{Jar: second}
	login(t, other, base, "other@example.com", mailer)
	status, body = call(t, other, http.MethodGet,
		base+"/api/jobs/"+strconv.FormatInt(reply.Job.JobID, 10), nil)
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
		Jobs []store.Job `json:"jobs"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	if len(reply.Jobs) != 2 {
		t.Fatalf("jobs = %+v, want 2", reply.Jobs)
	}
	if reply.Jobs[0].Conversation[0].Text != "Book the flight." {
		t.Fatalf("newest first = %+v", reply.Jobs[0].Conversation)
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

	second, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	other := &http.Client{Jar: second}
	login(t, other, base, "other@example.com", mailer)
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
