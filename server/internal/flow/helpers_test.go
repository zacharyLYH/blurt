package flow

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"blurt/internal/agent"
	"blurt/internal/auth"
	"blurt/internal/capture"
	"blurt/internal/httpapi"
	"blurt/internal/queue"
	"blurt/internal/store"
)

func (p *pump) clip(t *testing.T) string {
	t.Helper()
	name, err := capture.NewName()
	if err != nil {
		t.Fatalf("new name: %v", err)
	}
	if err := p.audio.Save(name, bytes.NewReader(wavBytes())); err != nil {
		t.Fatalf("save audio: %v", err)
	}
	return name
}

func (p *pump) voice(t *testing.T, zone string) store.Job {
	t.Helper()
	return p.create(t, store.Message{Role: "user", Audio: p.clip(t), TZ: zone})
}

func (p *pump) text(t *testing.T, text, zone string) store.Job {
	t.Helper()
	return p.create(t, store.Message{Role: "user", Text: text, TZ: zone})
}

func (p *pump) create(t *testing.T, messages ...store.Message) store.Job {
	t.Helper()
	return p.createFor(t, p.userID, messages...)
}

// createFor is create on behalf of another account, for the isolation flows.
func (p *pump) createFor(t *testing.T, userID int64, messages ...store.Message) store.Job {
	t.Helper()
	job, err := p.jobs.Create(context.Background(), userID, messages...)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	p.publish(job)
	return job
}

// strand creates a Job that never reaches Redis, the way a crash between the SQL
// commit and the XADD leaves one.
func (p *pump) strand(t *testing.T, messages ...store.Message) store.Job {
	t.Helper()
	job, err := p.jobs.Create(context.Background(), p.userID, messages...)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	return job
}

// publish mirrors httpapi's enqueue: the stage is chosen by what the Job still
// owes a transcript for.
func (p *pump) publish(job store.Job) {
	stream := p.agent
	if _, pending := job.PendingAudio(); pending {
		stream = p.transcription
	}
	if err := stream.Publish(context.Background(), job.JobID, 1); err != nil {
		p.t.Fatalf("publish: %v", err)
	}
}

// reply appends one more user turn and re-queues the Job, the way the reply
// endpoint does. The turn may be typed text or another clip.
func (p *pump) reply(t *testing.T, jobID int64, message store.Message) store.Job {
	t.Helper()
	job, err := p.jobs.AppendTurn(context.Background(), p.userID, jobID, message,
		map[string]any{"outcome": "reply"})
	if err != nil {
		t.Fatalf("append turn: %v", err)
	}
	p.publish(job)
	return job
}

// settle waits for the Job to reach a state the test is looking for. Both
// workers poll, so this is the only synchronisation the pipeline needs.
func (p *pump) settle(t *testing.T, jobID int64, want func(store.Job) bool, what string) store.Job {
	t.Helper()
	deadline := time.Now().Add(waitUntil)
	for {
		job, err := p.jobs.Get(context.Background(), jobID)
		if err != nil {
			t.Fatalf("load job: %v", err)
		}
		if want(job) {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; job = %+v", what, job)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func status(want string) func(store.Job) bool {
	return func(job store.Job) bool { return job.Status == want }
}

// recalled reports whether a planned turn was handed one Memory, by its text.
func recalled(in agent.Input, text string) bool {
	for _, memory := range in.Remembered {
		if memory.Text == text {
			return true
		}
	}
	return false
}

// traces returns every Trace row for one stage, oldest first.
func (p *pump) traces(t *testing.T, jobID int64, stage string) []map[string]any {
	t.Helper()
	rows, err := p.handle.Read.Query(
		`SELECT Detail FROM Trace WHERE JobId = ? AND Stage = ? ORDER BY TraceId`, jobID, stage)
	if err != nil {
		t.Fatalf("read traces: %v", err)
	}
	defer rows.Close()
	var details []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan trace: %v", err)
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			t.Fatalf("decode trace: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read traces: %v", err)
	}
	return details
}

// outcomes reads the outcome each Trace row for one stage recorded, in order.
func (p *pump) outcomes(t *testing.T, jobID int64, stage string) []string {
	t.Helper()
	outcomes := []string{}
	for _, detail := range p.traces(t, jobID, stage) {
		outcome, _ := detail["outcome"].(string)
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

// drain asserts a stage stream has nothing left to hand a worker.
func (p *pump) drain(t *testing.T, stream queue.Stream, what string) {
	t.Helper()
	if _, _, _, err := stream.Consume(context.Background()); err != queue.ErrEmpty {
		t.Fatalf("%s is not drained: %v", what, err)
	}
}

func (p *pump) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := p.handle.Read.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", query, err)
	}
	return n
}

func (p *pump) load(t *testing.T, jobID int64) store.Job {
	t.Helper()
	job, err := p.jobs.Get(context.Background(), jobID)
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	return job
}

// reminders and memories tally the rows a flow asserts on: a count is not part of
// the Job the store hands back, so it takes its own query.
func (p *pump) reminders(t *testing.T) int {
	t.Helper()
	return p.count(t, `SELECT count(*) FROM Reminder`)
}

func (p *pump) memories(t *testing.T) int {
	t.Helper()
	return p.count(t, `SELECT count(*) FROM Memory`)
}

// plannedFor returns the turn the loop planned for one capture, so a retrieval
// assertion reads as one line instead of a scan over every run.
func (p *pump) plannedFor(t *testing.T, capture string) agent.Input {
	t.Helper()
	for _, in := range p.model.planned() {
		if captureOf(in) == capture {
			return in
		}
	}
	t.Fatalf("no run was planned for capture %q", capture)
	return agent.Input{}
}

// pending reads the messages a stream has delivered but not acknowledged — the
// Mock's pending list and Redis's PEL alike.
func (p *pump) pending(t *testing.T, stream queue.Stream) []queue.Message {
	t.Helper()
	messages, err := mustClaim(t, stream).Claim(context.Background(), 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return messages
}

// soon polls for something a Job's own state cannot express, like "the worker
// has picked the message up".
func (p *pump) soon(t *testing.T, want func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(waitUntil)
	for !want() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// age backdates a Job's queued_at so the sweeper reads it as stranded.
func (p *pump) age(t *testing.T, jobID int64, old time.Time) {
	t.Helper()
	if _, err := p.handle.Write.Exec(`UPDATE Job SET queued_at = ? WHERE JobId = ?`,
		old.UTC().Format(time.RFC3339), jobID); err != nil {
		t.Fatalf("age job: %v", err)
	}
}

// user ensures a second account, so a flow can run two people's Jobs at once.
func (p *pump) user(t *testing.T, name, email string) int64 {
	t.Helper()
	user, err := store.NewUserStore(p.handle.Write, p.handle.Read).EnsureUser(context.Background(), name, email)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	return user.UserID
}

// answer keys a turn to the capture's own words, so two Jobs in flight each get
// the turn they were meant to get.
func (p *pump) answer(capture string, turn agent.Turn) *pump {
	p.model.mu.Lock()
	defer p.model.mu.Unlock()
	if p.model.byText == nil {
		p.model.byText = map[string]agent.Turn{}
	}
	p.model.byText[capture] = turn
	return p
}

// reminderTurn is the shape a clear capture commits.
func reminderTurn(text, phrase string) agent.Turn {
	return agent.Turn{Say: "Reminder set.", Writes: []agent.Write{{
		Kind: agent.KindReminder, Text: text, Timezone: "Asia/Singapore", TimePhrase: phrase}}}
}

// memoryTurn is the shape a fact commits; it needs no clock.
func memoryTurn(text string) agent.Turn {
	return agent.Turn{Say: "Noted.", Writes: []agent.Write{{Kind: agent.KindMemory, Text: text}}}
}

// httpOrigin must be in the API's allowed origins: the CSRF guard rejects unsafe
// requests that arrive without one.
const httpOrigin = "http://blurt.test"

// api boots the HTTP surface on the pump's own database, audio store and
// streams, and signs a user in through the dev PIN. It is how a flow starts
// where a real client does: at the ingress, not at the store.
func (p *pump) api(t *testing.T, email string) (*http.Client, string) {
	t.Helper()
	server := httptest.NewServer(httpapi.New(httpapi.Deps{
		DB:             p.handle.Write,
		Users:          store.NewUserStore(p.handle.Write, p.handle.Read),
		Sessions:       store.NewSessionStore(p.handle.Write, p.handle.Read),
		Pins:           auth.NewPinStore(time.Minute, auth.DevPIN),
		Pusher:         p.pusher,
		Jobs:           p.jobs,
		Audio:          p.audio,
		Queues:         &queue.Queues{TranscriptionQueue: p.transcription, AgentQueue: p.agent},
		AllowedOrigins: []string{httpOrigin},
	}))
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar}
	p.post(t, client, server.URL+"/api/auth/request-pin", map[string]string{"email": email}, http.StatusOK)
	p.post(t, client, server.URL+"/api/auth/verify-pin",
		map[string]any{"email": email, "pin": auth.DevPIN}, http.StatusOK)
	return client, server.URL
}

// post sends one JSON request with the headers a real client adds, and fails the
// test unless the response is the expected status.
func (p *pump) post(t *testing.T, client *http.Client, url string, body any, want int) []byte {
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
	req.Header.Set("Origin", httpOrigin)
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer res.Body.Close()
	reply, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if res.StatusCode != want {
		t.Fatalf("post %s = %d, want %d: %s", url, res.StatusCode, want, reply)
	}
	return reply
}

// upload sends one WAV capture the way the recorder does.
func (p *pump) upload(t *testing.T, client *http.Client, url, zone string) store.Job {
	t.Helper()
	return envelopeJob(t, p.postAudio(t, client, url, zone, http.StatusCreated))
}

// replyAudio answers a Job with a clip, the way the thread's mic does. It is the
// same upload to a different URL, which is the whole point of the one ingress:
// a turn is a turn, whether it opened a conversation or continued one.
func (p *pump) replyAudio(t *testing.T, client *http.Client, base string, jobID int64, zone string) store.Job {
	t.Helper()
	url := base + "/api/jobs/" + strconv.FormatInt(jobID, 10) + "/messages"
	return envelopeJob(t, p.postAudio(t, client, url, zone, http.StatusOK))
}

// postAudio sends one recorded clip with the headers a real client adds, and
// fails the test unless the response is the expected status.
func (p *pump) postAudio(t *testing.T, client *http.Client, url, zone string, want int) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(wavBytes()))
	if err != nil {
		t.Fatalf("build upload: %v", err)
	}
	req.Header.Set("Content-Type", "audio/wav")
	req.Header.Set("X-Blurt-Duration-Ms", "800")
	req.Header.Set("X-Blurt-Timezone", zone)
	req.Header.Set("Origin", httpOrigin)
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("upload %s: %v", url, err)
	}
	defer res.Body.Close()
	reply, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read upload reply: %v", err)
	}
	if res.StatusCode != want {
		t.Fatalf("upload %s = %d, want %d: %s", url, res.StatusCode, want, reply)
	}
	return reply
}

// envelopeJob reads the {"job": ...} envelope every ingress answers with.
func envelopeJob(t *testing.T, body []byte) store.Job {
	t.Helper()
	var reply struct {
		Job store.Job `json:"job"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	return reply.Job
}

// wavBytes builds the 44-byte RIFF header Save accepts; the worker only ever
// hands the blob to the transcriber, so no samples are needed.
func wavBytes() []byte {
	header := make([]byte, 44)
	copy(header[0:], "RIFF")
	binary.LittleEndian.PutUint32(header[4:], 36)
	copy(header[8:], "WAVE")
	copy(header[12:], "fmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1)
	binary.LittleEndian.PutUint16(header[22:], 1)
	binary.LittleEndian.PutUint32(header[24:], 16000)
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], "data")
	return header
}
