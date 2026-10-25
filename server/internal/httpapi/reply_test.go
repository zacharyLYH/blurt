package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"

	"blurt/internal/store"
)

// postJSON sends a body with the headers a real client adds: the CSRF origin, and
// the device zone the agent anchors relative times against.
func postJSON(t *testing.T, client *http.Client, url string, body any, headers map[string]string) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	reply, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return res.StatusCode, reply
}

// decodeJob reads a job envelope, which every capture and reply answers with.
func decodeJob(t *testing.T, body []byte) store.Job {
	t.Helper()
	var reply struct {
		Job store.Job `json:"job"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	return reply.Job
}

func createTextJob(t *testing.T, client *http.Client, base, text string) store.Job {
	t.Helper()
	status, body := postJSON(t, client, base+"/api/jobs", map[string]string{"text": text}, nil)
	if status != http.StatusCreated {
		t.Fatalf("capture status = %d: %s", status, body)
	}
	return decodeJob(t, body)
}

func messagesURL(base string, jobID int64) string {
	return base + "/api/jobs/" + strconv.FormatInt(jobID, 10) + "/messages"
}

// A reply may be a recording, not just JSON: the same two shapes a capture takes,
// because answering a question should not cost a voice-first user the keyboard.
// The clip belongs to the transcriber, so that is the stream it lands on.
func TestAudioReplyResumesAClarifyingJob(t *testing.T) {
	client, base, mailer, handle, queues := newQueueClient(t)
	login(t, client, base, "lam@example.com", mailer)
	job := createTextJob(t, client, base, "buy glue")

	ctx := context.Background()
	jobs := store.NewJobStore(handle.Write, handle.Read)
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, store.AgentStep{
		Reply:  store.Message{Text: "When should I remind you?"},
		Status: store.JobClarifying,
		Detail: map[string]any{"outcome": store.JobClarifying},
	}); err != nil {
		t.Fatalf("seed clarification: %v", err)
	}

	status, body := upload(t, client, messagesURL(base, job.JobID), "audio/wav", "800", wavBytes(16000, 1))
	if status != http.StatusOK {
		t.Fatalf("audio reply status = %d, want 200: %s", status, body)
	}
	replied := decodeJob(t, body)
	if replied.Status != store.JobOpen {
		t.Fatalf("status = %q, want the Job back in the queue", replied.Status)
	}
	if last := replied.Conversation[len(replied.Conversation)-1]; last.Role != "user" || last.Audio == "" || last.Text != "" {
		t.Fatalf("reply turn = %+v, want an untranscribed clip", last)
	}
	if id, attempt := consume(t, queues.TranscriptionQueue); id != job.JobID || attempt != 1 {
		t.Fatalf("transcription message = %d/%d, want %d/1", id, attempt, job.JobID)
	}
}

// A closed Job takes no clip at all, and the refusal must not leave the blob
// behind: nothing would ever sweep a file no turn points at.
func TestAudioReplyToAClosedJobIsRefused(t *testing.T) {
	client, base, mailer, handle, _ := newQueueClient(t)
	user := login(t, client, base, "lam@example.com", mailer)
	job := createTextJob(t, client, base, "buy glue")

	ctx := context.Background()
	jobs := store.NewJobStore(handle.Write, handle.Read)
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, store.AgentStep{
		Reply:  store.Message{Text: "Reminder set."},
		Status: store.JobComplete,
		Detail: map[string]any{"outcome": store.JobComplete},
	}); err != nil {
		t.Fatalf("close job: %v", err)
	}

	status, body := upload(t, client, messagesURL(base, job.JobID), "audio/wav", "800", wavBytes(16000, 1))
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", status, body)
	}
	if !bytes.Contains(body, []byte("job_closed")) {
		t.Fatalf("body = %s, want the job_closed code", body)
	}
	stored, err := jobs.ByID(ctx, user.UserID, job.JobID)
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	if stored.Status != store.JobComplete || len(stored.Conversation) != 2 {
		t.Fatalf("job = %+v, want it left alone", stored)
	}
}

// A capture carries the device's zone so the agent can anchor relative times,
// and a zone Blurt cannot load is refused at the edge.
func TestCaptureCarriesTheDeviceTimezone(t *testing.T) {
	client, base, mailer, _ := newClient(t)
	login(t, client, base, "lam@example.com", mailer)

	cases := []struct {
		name   string
		zone   string
		status int
	}{
		{"the device's zone", "Asia/Singapore", http.StatusCreated},
		{"an unknown zone", "Mars/Olympus", http.StatusBadRequest},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status, body := postJSON(t, client, base+"/api/jobs", map[string]string{"text": "buy glue"},
				map[string]string{"X-Blurt-Timezone": test.zone})
			if status != test.status {
				t.Fatalf("status = %d, want %d: %s", status, test.status, body)
			}
			if test.status == http.StatusCreated {
				if zone := decodeJob(t, body).Conversation[0].TZ; zone != test.zone {
					t.Fatalf("zone = %q, want the device's", zone)
				}
			} else if !bytes.Contains(body, []byte("invalid_timezone")) {
				t.Fatalf("body = %s, want the invalid_timezone code", body)
			}
		})
	}
}

// The clarification reply: one more turn in the same Conversation, and the Job goes
// back to the queue with a fresh attempt budget so the model resumes on it.
func TestReplyResumesAClarifyingJob(t *testing.T) {
	client, base, mailer, handle := newClient(t)
	user := login(t, client, base, "lam@example.com", mailer)
	job := createTextJob(t, client, base, "buy glue")

	// What the AgentLoop leaves behind when it cannot pin a time.
	ctx := context.Background()
	jobs := store.NewJobStore(handle.Write, handle.Read)
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, store.AgentStep{
		Reply:  store.Message{Text: "When should I remind you?"},
		Status: store.JobClarifying,
		Detail: map[string]any{"outcome": store.JobClarifying},
	}); err != nil {
		t.Fatalf("seed clarification: %v", err)
	}
	if err := jobs.Retry(ctx, store.AgentStage, job.JobID, 2, map[string]any{"outcome": "retry"}); err != nil {
		t.Fatalf("seed attempts: %v", err)
	}

	status, body := postJSON(t, client, messagesURL(base, job.JobID), map[string]string{"text": "at 6pm tomorrow"},
		map[string]string{"X-Blurt-Timezone": "Asia/Singapore"})
	if status != http.StatusOK {
		t.Fatalf("reply status = %d, want 200: %s", status, body)
	}
	replied := decodeJob(t, body)
	if replied.Status != store.JobOpen {
		t.Fatalf("status = %q, want the Job back in the queue", replied.Status)
	}
	last := replied.Conversation[len(replied.Conversation)-1]
	if last.Role != "user" || last.Text != "at 6pm tomorrow" || last.TZ != "Asia/Singapore" {
		t.Fatalf("appended turn = %+v", last)
	}
	// A fresh answer deserves the whole attempt budget again.
	wrote, err := jobs.ByID(ctx, user.UserID, job.JobID)
	if err != nil || wrote.RetryCount != 0 {
		t.Fatalf("attempts = %+v, %v, want a reset counter", wrote, err)
	}
}

// A finished Job has nothing to attach a reply to, and a reply must not reopen it.
func TestReplyToAFinishedJobIsRefused(t *testing.T) {
	client, base, mailer, handle := newClient(t)
	user := login(t, client, base, "lam@example.com", mailer)
	job := createTextJob(t, client, base, "buy glue")
	ctx := context.Background()
	jobs := store.NewJobStore(handle.Write, handle.Read)
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, store.AgentStep{
		Reply:  store.Message{Text: "Reminder set."},
		Status: store.JobComplete,
		Detail: map[string]any{"outcome": store.JobComplete},
	}); err != nil {
		t.Fatalf("close job: %v", err)
	}

	status, body := postJSON(t, client, messagesURL(base, job.JobID),
		map[string]string{"text": "actually make it 7pm"}, nil)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", status, body)
	}
	if !bytes.Contains(body, []byte("job_closed")) {
		t.Fatalf("body = %s, want the job_closed code", body)
	}
	stored, err := jobs.ByID(ctx, user.UserID, job.JobID)
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	if stored.Status != store.JobComplete || len(stored.Conversation) != 2 {
		t.Fatalf("job = %+v, want it left alone", stored)
	}
}

// A reply is scoped to its owner, the same way reading a Job is.
func TestReplyToAForeignJobIsNotFound(t *testing.T) {
	client, base, mailer, handle := newClient(t)
	owner := login(t, client, base, "lam@example.com", mailer)
	job := createTextJob(t, client, base, "buy glue")
	jobs := store.NewJobStore(handle.Write, handle.Read)
	if _, err := jobs.CommitAgentStep(context.Background(), job.JobID, store.AgentStep{
		Reply:  store.Message{Text: "When should I remind you?"},
		Status: store.JobClarifying,
		Detail: map[string]any{"outcome": store.JobClarifying},
	}); err != nil {
		t.Fatalf("seed clarification: %v", err)
	}

	// A second user signs in on the same browser and tries the first user's Job. A PIN
	// is only issued without a session, so the first user signs out first.
	if status, body := call(t, client, http.MethodPost, base+"/api/auth/logout", nil); status != http.StatusNoContent {
		t.Fatalf("logout status = %d: %s", status, body)
	}
	login(t, client, base, "sam@example.com", mailer)
	status, body := postJSON(t, client, messagesURL(base, job.JobID), map[string]string{"text": "at 6pm"}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", status, body)
	}
	if stored, err := jobs.ByID(context.Background(), owner.UserID, job.JobID); err != nil ||
		len(stored.Conversation) != 2 {
		t.Fatalf("job = %+v, %v, want the conversation untouched", stored, err)
	}
}

// A failed Job is still open for recovery, and a typed answer is exactly that: the
// user retyping what the clip could not deliver.
func TestReplyRecoversAFailedJob(t *testing.T) {
	client, base, mailer, handle := newClient(t)
	user := login(t, client, base, "lam@example.com", mailer)
	job := createTextJob(t, client, base, "buy glue")
	ctx := context.Background()
	jobs := store.NewJobStore(handle.Write, handle.Read)
	if err := jobs.Fail(ctx, store.AgentStage, job.JobID, 3, "I couldn't process that.", nil); err != nil {
		t.Fatalf("fail job: %v", err)
	}

	status, body := postJSON(t, client, messagesURL(base, job.JobID),
		map[string]string{"text": "buy glue at 6pm tomorrow"}, nil)
	if status != http.StatusOK {
		t.Fatalf("reply status = %d, want 200: %s", status, body)
	}
	replied := decodeJob(t, body)
	if replied.Status != store.JobOpen {
		t.Fatalf("status = %q, want the Job back in the queue", replied.Status)
	}
	if user.UserID <= 0 {
		t.Fatal("the signed-in user went missing")
	}
}

// A clarification reply goes back onto the agent stream, which is what lets the
// AgentLoop resume the Job with the answer in hand instead of re-transcribing.
func TestReplyResumesAClarifyingJobOnTheAgentStream(t *testing.T) {
	client, base, mailer, handle, queues := newQueueClient(t)
	login(t, client, base, "lam@example.com", mailer)
	job := createTextJob(t, client, base, "buy glue")

	// Drain the capture's own first hop so the reply is the next message.
	consume(t, queues.AgentQueue)

	ctx := context.Background()
	jobs := store.NewJobStore(handle.Write, handle.Read)
	if _, err := jobs.CommitAgentStep(ctx, job.JobID, store.AgentStep{
		Reply:  store.Message{Text: "When should I remind you?"},
		Status: store.JobClarifying,
		Detail: map[string]any{"outcome": store.JobClarifying},
	}); err != nil {
		t.Fatalf("seed clarification: %v", err)
	}

	status, body := postJSON(t, client, messagesURL(base, job.JobID),
		map[string]string{"text": "at 6pm tomorrow"}, nil)
	if status != http.StatusOK {
		t.Fatalf("reply status = %d, want 200: %s", status, body)
	}
	if id, attempt := consume(t, queues.AgentQueue); id != job.JobID || attempt != 1 {
		t.Fatalf("requeued = %d/%d, want %d/1", id, attempt, job.JobID)
	}
}
