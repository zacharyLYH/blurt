package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"blurt/internal/db"
	"blurt/internal/decision"
	"blurt/internal/push"
	"blurt/internal/store"
	"blurt/internal/stt"
)

type recordedPush struct {
	calls   int
	userID  int64
	message push.Message
}

func (p *recordedPush) NotifyUser(_ context.Context, userID int64, msg push.Message) (push.Result, error) {
	p.calls++
	p.userID, p.message = userID, msg
	return push.Result{Sent: 1}, nil
}

type fixture struct {
	worker *Worker
	jobs   *store.JobStore
	audio  *Store
	handle *db.DB
	pusher *recordedPush
	userID int64
}

func newFixture(t *testing.T, mode string) *fixture {
	t.Helper()
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })

	ctx := context.Background()
	user, err := store.NewUserStore(handle.Write, handle.Read).EnsureUser(ctx, "Lam", "lam@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	audio, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("open audio store: %v", err)
	}
	pusher := &recordedPush{}
	return &fixture{
		worker: &Worker{
			Jobs: store.NewJobStore(handle.Write, handle.Read), Audio: audio,
			STT: stt.NewFake(mode), Pusher: pusher, Model: "phonon",
		},
		jobs:   store.NewJobStore(handle.Write, handle.Read),
		audio:  audio,
		handle: handle,
		pusher: pusher,
		userID: user.UserID,
	}
}

func (f *fixture) audioJob(t *testing.T) store.Job {
	t.Helper()
	name, err := NewName()
	if err != nil {
		t.Fatalf("new name: %v", err)
	}
	if err := f.audio.Save(name, bytes.NewReader(wav(16000, 1, 16, 1))); err != nil {
		t.Fatalf("save audio: %v", err)
	}
	job, err := f.jobs.Create(context.Background(), f.userID, store.Message{Role: "user", Audio: name})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	return job
}

func (f *fixture) load(t *testing.T, jobID int64) store.Job {
	t.Helper()
	job, err := f.jobs.ByID(context.Background(), f.userID, jobID)
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	return job
}

func (f *fixture) trace(t *testing.T, jobID int64) map[string]any {
	t.Helper()
	var raw string
	if err := f.handle.Read.QueryRow(
		`SELECT Detail FROM Trace WHERE JobId = ? ORDER BY TraceId DESC LIMIT 1`, jobID).Scan(&raw); err != nil {
		t.Fatalf("read trace: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	return detail
}

func TestWorkerSavesTranscriptAndDropsAudio(t *testing.T) {
	f := newFixture(t, "ok")
	job := f.audioJob(t)
	audioName := job.Conversation[0].Audio

	f.worker.process(context.Background(), job)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobQueued || stored.Conversation[0].Text != stt.FakeTranscript {
		t.Fatalf("stored job = %+v", stored)
	}
	if file, err := f.audio.Open(audioName); err == nil {
		file.Close()
		t.Fatal("audio blob survived a successful transcription")
	}
	if outcome := f.trace(t, job.JobID)["outcome"]; outcome != "ok" {
		t.Fatalf("trace outcome = %v, want ok", outcome)
	}
	if f.pusher.calls != 0 {
		t.Fatalf("push calls = %d, want 0", f.pusher.calls)
	}
}

// A transient provider failure must keep the blob and requeue the same Job.
func TestWorkerRetriesThenSucceeds(t *testing.T) {
	f := newFixture(t, "flaky")
	job := f.audioJob(t)
	audioName := job.Conversation[0].Audio

	f.worker.process(context.Background(), job)
	afterFirst := f.load(t, job.JobID)
	if afterFirst.Status != store.JobQueued || afterFirst.RetryCount != 1 {
		t.Fatalf("after first attempt = %+v", afterFirst)
	}
	if file, err := f.audio.Open(audioName); err != nil {
		t.Fatalf("audio should survive a retry: %v", err)
	} else {
		file.Close()
	}

	f.worker.process(context.Background(), afterFirst)
	stored := f.load(t, job.JobID)
	if stored.Status != store.JobQueued || stored.Conversation[0].Text != stt.FakeTranscript {
		t.Fatalf("after retry = %+v", stored)
	}
	if file, err := f.audio.Open(audioName); err == nil {
		file.Close()
		t.Fatal("audio blob was not removed after success")
	}
}

// Three failed attempts is the poison pill: fallback copy, failed status,
// removed blob, and one best-effort push.
func TestWorkerFailsAfterThreeAttempts(t *testing.T) {
	f := newFixture(t, "error")
	job := f.audioJob(t)

	for i := 0; i < 3; i++ {
		job = f.load(t, job.JobID)
		f.worker.process(context.Background(), job)
	}

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobFailed || stored.RetryCount != 3 {
		t.Fatalf("stored job = %+v", stored)
	}
	last := stored.Conversation[len(stored.Conversation)-1]
	if last.Role != "assistant" || last.Text != FallbackMessage {
		t.Fatalf("fallback message = %+v", last)
	}
	if f.pusher.calls != 1 || f.pusher.message.Body != FallbackMessage {
		t.Fatalf("push = %+v", f.pusher)
	}
	if file, err := f.audio.Open(stored.Conversation[0].Audio); err == nil {
		file.Close()
		t.Fatal("failed job kept its audio blob")
	}
}

// A blank transcript is permanent: retrying the same unintelligible clip just
// burns STT spend.
func TestWorkerRejectsIllegibleClip(t *testing.T) {
	f := newFixture(t, "illegible")
	job := f.audioJob(t)

	f.worker.process(context.Background(), job)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobFailed || stored.RetryCount != 1 {
		t.Fatalf("stored job = %+v", stored)
	}
	if outcome := f.trace(t, job.JobID)["outcome"]; outcome != "illegible" {
		t.Fatalf("trace outcome = %v, want illegible", outcome)
	}
}

func TestRequeueInterruptedCountsAnAttempt(t *testing.T) {
	f := newFixture(t, "ok")
	job := f.audioJob(t)
	if _, err := f.handle.Write.Exec(`UPDATE Job SET Status = ? WHERE JobId = ?`,
		store.JobProcessing, job.JobID); err != nil {
		t.Fatalf("mark processing: %v", err)
	}

	count, err := f.jobs.RequeueInterrupted(context.Background())
	if err != nil {
		t.Fatalf("RequeueInterrupted: %v", err)
	}
	if count != 1 {
		t.Fatalf("requeued = %d, want 1", count)
	}
	stored := f.load(t, job.JobID)
	if stored.Status != store.JobQueued || stored.RetryCount != 1 {
		t.Fatalf("stored job = %+v", stored)
	}
}

// fakeDecider stands in for the gateway so the gate's policy is tested without
// it: it labels every question, or fails outright.
type fakeDecider struct {
	label string
	err   error
}

func (f *fakeDecider) Decide(_ context.Context, _ string, questions decision.Questions) (decision.Decisions, error) {
	if f.err != nil {
		return nil, f.err
	}
	answers := decision.Decisions{}
	for id := range questions {
		answers[id] = decision.Answer{Kind: decision.Choice, Choice: f.label,
			Probabilities: map[string]float64{f.label: 0.9}}
	}
	return answers, nil
}

// A noise verdict is permanent: the same clip cannot become a request on a
// retry, so it takes the illegible path — fallback, permanent failure, push,
// and no blob left behind.
func TestWorkerRejectsNoiseTranscript(t *testing.T) {
	f := newFixture(t, "ok")
	f.worker.Gate = &fakeDecider{label: noiseLabel}
	f.worker.GateModel = "laya"
	job := f.audioJob(t)

	f.worker.process(context.Background(), job)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobFailed || stored.RetryCount != 1 {
		t.Fatalf("stored job = %+v", stored)
	}
	last := stored.Conversation[len(stored.Conversation)-1]
	if last.Role != "assistant" || last.Text != FallbackMessage {
		t.Fatalf("fallback = %+v", last)
	}
	detail := f.trace(t, job.JobID)
	if detail["outcome"] != "noise" || detail["label"] != noiseLabel || detail["gateModel"] != "laya" {
		t.Fatalf("trace = %v", detail)
	}
	if f.pusher.calls != 1 {
		t.Fatalf("push calls = %d, want 1", f.pusher.calls)
	}
	if file, err := f.audio.Open(stored.Conversation[0].Audio); err == nil {
		file.Close()
		t.Fatal("a noise job kept its audio blob")
	}
}

// A request passes the gate and is transcribed as usual.
func TestWorkerPassesRequestTranscript(t *testing.T) {
	f := newFixture(t, "ok")
	f.worker.Gate = &fakeDecider{label: requestLabel}
	job := f.audioJob(t)

	f.worker.process(context.Background(), job)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobQueued || stored.Conversation[0].Text != stt.FakeTranscript {
		t.Fatalf("stored job = %+v", stored)
	}
	detail := f.trace(t, job.JobID)
	if detail["outcome"] != "ok" || detail["label"] != requestLabel {
		t.Fatalf("trace = %v", detail)
	}
	if f.pusher.calls != 0 {
		t.Fatalf("push calls = %d, want 0", f.pusher.calls)
	}
}

// The filter is advisory: a classifier that is down must not block a capture.
// The transcript is saved and the reason lands in the trace.
func TestWorkerGateErrorFailsOpen(t *testing.T) {
	f := newFixture(t, "ok")
	f.worker.Gate = &fakeDecider{err: errors.New("decision: call laya: connection refused")}
	job := f.audioJob(t)

	f.worker.process(context.Background(), job)

	stored := f.load(t, job.JobID)
	if stored.Status != store.JobQueued || stored.Conversation[0].Text != stt.FakeTranscript {
		t.Fatalf("stored job = %+v", stored)
	}
	detail := f.trace(t, job.JobID)
	if detail["outcome"] != "ok" || detail["gate"] != "error" {
		t.Fatalf("trace = %v", detail)
	}
	if reason, _ := detail["gateError"].(string); !strings.Contains(reason, "refused") {
		t.Fatalf("gateError = %v", detail["gateError"])
	}
}
