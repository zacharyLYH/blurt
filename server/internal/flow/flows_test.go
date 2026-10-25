package flow

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"blurt/internal/agent"
	"blurt/internal/config"
	"blurt/internal/store"
	"blurt/internal/stt"
)

// TestPipelineVoiceClarifyThenReplyCommits is the headline flow: a voice capture
// the agent cannot act on, a typed answer, and the Reminder that lands. It
// crosses the capture worker, both streams, the agent worker, and the store.
func TestPipelineVoiceClarifyThenReplyCommits(t *testing.T) {
	p := newPump(t,
		agent.Turn{Say: "Which afternoon should I remind you?"},
		agent.Turn{Say: "Reminder set.", Writes: []agent.Write{{
			Kind: agent.KindReminder, Text: "buy glue", Timezone: "Asia/Singapore", TimePhrase: "tomorrow at 6pm"}}},
	).start()
	job := p.voice(t, "Asia/Singapore")

	asked := p.settle(t, job.JobID, status(store.JobClarifying), "the clarification")
	// The transcript lands on the clip's own turn before the question arrives.
	if first := asked.Conversation[0]; first.Text != stt.FakeTranscript {
		t.Fatalf("clip turn = %+v, want the transcript", first)
	}
	if last := asked.Conversation[len(asked.Conversation)-1]; last.Role != "assistant" || last.Text == "" {
		t.Fatalf("clarification = %+v", last)
	}

	p.reply(t, job.JobID, store.Message{Text: "tomorrow at 6pm", TZ: "Asia/Singapore"})

	done := p.settle(t, job.JobID, status(store.JobComplete), "the committed Reminder")
	// The answer earns the whole budget back.
	if done.RetryCount != 0 {
		t.Fatalf("retry count = %d, want a fresh budget after the reply", done.RetryCount)
	}
	var text, zone, local, emit string
	if err := p.handle.Read.QueryRow(
		`SELECT Text, Timezone, LocalDateTime, EmitAtUtc FROM Reminder`).Scan(&text, &zone, &local, &emit); err != nil {
		t.Fatalf("read reminder: %v", err)
	}
	if text != "buy glue" || zone != "Asia/Singapore" || local != "2026-10-25T18:00" || emit != "2026-10-25T10:00:00Z" {
		t.Fatalf("reminder = %q %q %q %q", text, zone, local, emit)
	}
}

// A voice capture can also be answered by voice: the clip is transcribed,
// re-queued to the agent, and commits. This is the plumbing behind "one ingress
// for capture, clarification, and correction".
func TestPipelineVoiceReplyAfterClarificationCommits(t *testing.T) {
	p := newPump(t,
		agent.Turn{Say: "Which afternoon should I remind you?"},
		agent.Turn{Say: "Reminder set.", Writes: []agent.Write{{
			Kind: agent.KindReminder, Text: "buy glue", Timezone: "Asia/Singapore", TimePhrase: "tomorrow at 6pm"}}},
	)
	// Every clip carries a clock, so the voiced answer can resolve to a time.
	p.captureWorker.STT = scriptedTranscriber{text: "buy glue tomorrow at 6pm"}
	p.start()
	job := p.voice(t, "Asia/Singapore")
	p.settle(t, job.JobID, status(store.JobClarifying), "the clarification")

	p.reply(t, job.JobID, store.Message{Audio: p.clip(t)})

	done := p.settle(t, job.JobID, status(store.JobComplete), "the voiced answer to commit")
	if len(done.Conversation) != 4 {
		t.Fatalf("conversation = %+v, want clip, question, clip, reply", done.Conversation)
	}
	for _, turn := range done.Conversation {
		if turn.Role == "user" && turn.Text == "" {
			t.Fatalf("a user turn was left untranscribed: %+v", turn)
		}
		if turn.Audio == "" {
			continue
		}
		if file, err := p.audio.Open(turn.Audio); err == nil {
			file.Close()
			t.Fatalf("blob %q was left behind", turn.Audio)
		}
	}
}

// A second capture that leans on an earlier Reminder, and the clarification it
// needs: the prompt has to carry the device zone, the nearby Reminder, and the
// whole conversation, with the capture last. This is the PRD's litmus test 2.
func TestPipelinePromptCarriesZoneReferenceAndReply(t *testing.T) {
	server, script := newGatewayScript(t,
		`{"say":"Reminder set for tomorrow at 5pm.","text":"fetch kids from school","timezone":"Asia/Singapore","timePhrase":"tomorrow at 5pm","memoryText":""}`,
		`{"say":"When should I remind you about the glue?","text":"","timezone":"","timePhrase":"","memoryText":""}`,
		`{"say":"Reminder set for tomorrow at 6pm.","text":"buy glue","timezone":"Asia/Tokyo","timePhrase":"tomorrow at 6pm","memoryText":""}`,
	)
	p := newPump(t)
	p.agentWorker.Loop.Model = agent.NewGateway(config.Agent{URL: server.URL, Model: "local-model"})
	p.start()

	first := p.text(t, "fetch kids from school tomorrow at 5pm", "Asia/Singapore")
	p.settle(t, first.JobID, status(store.JobComplete), "the reference Reminder")

	second := p.text(t, "buy glue on the way home from school", "Asia/Tokyo")
	p.settle(t, second.JobID, status(store.JobClarifying), "the capture that needs a time")

	// The capture's own turn is last, and the context above it carries the
	// device zone and the Reminder it refers to.
	prompt := script.call(t, "buy glue on the way home from school")
	content := prompt.Messages[len(prompt.Messages)-1].Content
	for _, want := range []string{
		"current_tz: Asia/Tokyo",
		"nearby reminders:",
		"- fetch kids from school",
		"capture: buy glue on the way home from school",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("prompt = %q, want %q", content, want)
		}
	}

	// The answer is the next run's capture, and the earlier turns travel as chat
	// messages, so the model sees what it already asked.
	p.reply(t, second.JobID, store.Message{Text: "tomorrow at 6pm", TZ: "Asia/Tokyo"})
	p.settle(t, second.JobID, status(store.JobComplete), "the answered Reminder")

	reply := script.call(t, "tomorrow at 6pm")
	roles := make([]string, 0, len(reply.Messages))
	for _, message := range reply.Messages {
		roles = append(roles, message.Role)
	}
	if got := strings.Join(roles, ","); got != "system,user,assistant,user" {
		t.Fatalf("roles = %s, want the whole conversation before the capture", got)
	}
}

// A Job that trips the poison pill is recoverable: the user retries it, and the
// same Job commits on the fresh budget. This is the PRD's "clarification gets
// stuck" recovery, and it crosses the fail path, the reply path, and a re-run.
func TestPipelineFailedJobRecoversOnReply(t *testing.T) {
	transient := errors.New("agent: call model: connection refused")
	p := newPump(t, agent.Turn{Say: "Reminder set.", Writes: []agent.Write{{
		Kind: agent.KindReminder, Text: "buy glue", Timezone: "Asia/Singapore", TimePhrase: "tomorrow at 6pm"}}})
	p.model.errs = []error{transient, transient, transient}
	p.start()
	job := p.text(t, "buy glue tomorrow at 6pm", "Asia/Singapore")

	failed := p.settle(t, job.JobID, status(store.JobFailed), "the poison pill")
	if failed.RetryCount != store.MaxAttempts {
		t.Fatalf("retry count = %d, want %d", failed.RetryCount, store.MaxAttempts)
	}

	p.reply(t, job.JobID, store.Message{Text: "buy glue tomorrow at 6pm", TZ: "Asia/Singapore"})

	done := p.settle(t, job.JobID, status(store.JobComplete), "the recovered Job")
	if done.RetryCount != 0 {
		t.Fatalf("retry count = %d, want the reply to reset the budget", done.RetryCount)
	}
	if got := p.reminders(t); got != 1 {
		t.Fatalf("reminders = %d, want the one the recovery committed", got)
	}
}

// One capture can commit a Reminder and a Memory together, and the Memory is
// findable by a later capture's BM25 search even though the wording differs.
// The agent writes no Tags, so retrieval here rides on the Memory's own text —
// which is exactly why the FTS index carries text as its searchable column.
func TestPipelineMemoryIsRetrievableByALaterCapture(t *testing.T) {
	const remembered = "I have AirAsia points to use by November"
	p := newPump(t,
		agent.Turn{Say: "Reminder set.", Writes: []agent.Write{
			{Kind: agent.KindReminder, Text: "book the flight", Timezone: "Asia/Singapore", TimePhrase: "tomorrow at 6pm"},
			{Kind: agent.KindMemory, Text: remembered},
		}},
		agent.Turn{Say: "Noted.", Writes: []agent.Write{{Kind: agent.KindMemory, Text: "airasia points"}}},
	).start()

	first := p.text(t, "remind me to book the flight tomorrow at 6pm and remember I have AirAsia points to use by November", "Asia/Singapore")
	p.settle(t, first.JobID, status(store.JobComplete), "the Reminder and Memory")
	if got := p.reminders(t); got != 1 {
		t.Fatalf("reminders = %d, want one", got)
	}
	if got := p.memories(t); got != 1 {
		t.Fatalf("memories = %d, want one", got)
	}

	const capture = "book a dinner flight using my AirAsia points tomorrow at 9pm"
	second := p.text(t, capture, "Asia/Singapore")
	p.settle(t, second.JobID, status(store.JobComplete), "the capture that recalls it")

	// The recalled Memory reaches the model's turn, not just the database.
	if in := p.plannedFor(t, capture); !recalled(in, remembered) {
		t.Fatalf("remembered = %+v, want the AirAsia Memory", in.Remembered)
	}
}

// Two people's Jobs run at once without touching each other: each commit lands on
// its own owner, and no run's retrieval sees the other's records.
func TestPipelineKeepsConcurrentUsersIsolated(t *testing.T) {
	p := newPump(t).
		answer("buy glue tomorrow at 6pm", reminderTurn("buy glue", "tomorrow at 6pm")).
		answer("take the trash out tomorrow at 7pm", reminderTurn("take the trash out", "tomorrow at 7pm"))
	p.start()

	other := p.user(t, "Sam", "sam@example.com")
	first := p.text(t, "buy glue tomorrow at 6pm", "Asia/Singapore")
	second := p.createFor(t, other,
		store.Message{Role: "user", Text: "take the trash out tomorrow at 7pm", TZ: "Asia/Singapore"})

	p.settle(t, first.JobID, status(store.JobComplete), "the first user's Job")
	p.settle(t, second.JobID, status(store.JobComplete), "the second user's Job")

	var owner int64
	if err := p.handle.Read.QueryRow(
		`SELECT UserId FROM Reminder WHERE Text = 'take the trash out'`).Scan(&owner); err != nil {
		t.Fatalf("read reminder: %v", err)
	}
	if owner != other {
		t.Fatalf("reminder owner = %d, want %d", owner, other)
	}
	// Neither run was planned against the other's records.
	for _, in := range p.model.planned() {
		if len(in.Nearby) != 0 || len(in.Remembered) != 0 {
			t.Fatalf("a run saw another user's records: %+v", in)
		}
	}
}

// The ingress is only real end to end when what the client posts reaches a
// worker: this drives createJob → enqueue → XADD → the running workers, for a
// typed capture and a recorded one.
func TestPipelineHTTPIngressReachesTheWorkers(t *testing.T) {
	p := newPump(t).
		answer("buy glue tomorrow at 6pm", reminderTurn("buy glue", "tomorrow at 6pm")).
		answer(stt.FakeTranscript, memoryTurn("buy glue"))
	p.start()
	client, base := p.api(t, "lam@example.com")

	posted := envelopeJob(t, p.post(t, client, base+"/api/jobs",
		map[string]any{"text": "buy glue tomorrow at 6pm"}, http.StatusCreated))
	p.settle(t, posted.JobID, status(store.JobComplete), "the typed capture")

	// The clip's default transcript names no time, so this one commits a Memory.
	uploaded := p.upload(t, client, base+"/api/jobs", "Asia/Singapore")
	p.settle(t, uploaded.JobID, status(store.JobComplete), "the recorded capture")
}

// A clarification answered by voice, driven the way the thread drives it: a
// recording posted to the Job's messages route resumes the same conversation.
// This is the one-ingress promise — a turn is a turn — over real HTTP.
func TestPipelineHTTPVoiceReplyAfterClarificationCommits(t *testing.T) {
	p := newPump(t).
		answer("buy glue", agent.Turn{Say: "Which afternoon should I remind you?"}).
		answer("buy glue tomorrow at 6pm", reminderTurn("buy glue", "tomorrow at 6pm"))
	p.captureWorker.STT = scriptedTranscriber{text: "buy glue tomorrow at 6pm"}
	p.start()
	client, base := p.api(t, "lam@example.com")

	typed := envelopeJob(t, p.post(t, client, base+"/api/jobs",
		map[string]any{"text": "buy glue"}, http.StatusCreated))
	p.settle(t, typed.JobID, status(store.JobClarifying), "the clarification")

	replied := p.replyAudio(t, client, base, typed.JobID, "Asia/Singapore")
	if replied.Status != store.JobOpen {
		t.Fatalf("reply status = %q, want the Job back in the queue", replied.Status)
	}

	done := p.settle(t, typed.JobID, status(store.JobComplete), "the voiced answer to commit")
	if len(done.Conversation) != 4 {
		t.Fatalf("conversation = %+v, want capture, question, clip, reply", done.Conversation)
	}
	// The clip's own turn carries its transcript and the blob is gone: the
	// transcriber owned that turn, not the agent.
	clip := done.Conversation[2]
	if clip.Text != "buy glue tomorrow at 6pm" || clip.Audio == "" {
		t.Fatalf("clip turn = %+v", clip)
	}
	if file, err := p.audio.Open(clip.Audio); err == nil {
		file.Close()
		t.Fatal("the reply's blob was left behind")
	}
	if got := p.reminders(t); got != 1 {
		t.Fatalf("reminders = %d, want the one the voiced answer committed", got)
	}
}

// Tags are search features rather than extra text: a Memory whose own words a
// later capture never uses still comes back, because its tag does.
func TestPipelineTagsMakeAMemoryFindableByOtherWording(t *testing.T) {
	const fact = "milk is cheaper at store a"
	p := newPump(t).
		answer("remember "+fact, memoryTurn(fact)).
		answer("buy grocery", memoryTurn("grocery"))
	p.agentWorker.Loop.Tagger = &fixedTagger{tags: []string{"grocery", "price", "milk"}}
	p.start()

	first := p.text(t, "remember "+fact, "Asia/Singapore")
	p.settle(t, first.JobID, status(store.JobComplete), "the tagged Memory")

	var tags string
	if err := p.handle.Read.QueryRow(`SELECT Tags FROM Memory`).Scan(&tags); err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if tags != "grocery,price,milk" {
		t.Fatalf("tags = %q, want the generated features on the row", tags)
	}

	// "buy grocery" shares no word with the Memory's text; only the tag brings it
	// back, which is the whole point of generating features.
	second := p.text(t, "buy grocery", "Asia/Singapore")
	p.settle(t, second.JobID, status(store.JobComplete), "the capture that recalls by tag")

	if in := p.plannedFor(t, "buy grocery"); !recalled(in, fact) {
		t.Fatalf("remembered = %+v, want the tag-recalled Memory", in.Remembered)
	}
}
