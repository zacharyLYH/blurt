package flow

import (
	"encoding/json"
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
		agent.Turn{},
		reminderTurn("buy glue", "tomorrow at 6pm"),
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
		agent.Turn{},
		reminderTurn("buy glue", "tomorrow at 6pm"),
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
		`{"text":"fetch kids from school","zone":"Asia/Singapore","date":"2026-10-25","time":"17:00","recurrence":"none","evidence":"the user said \"tomorrow at 5pm\""}`,
		`{"text":"","zone":"","date":"","time":"","recurrence":"none","evidence":""}`,
		`{"text":"buy glue","zone":"Asia/Tokyo","date":"2026-10-25","time":"18:00","recurrence":"none","evidence":"the user said \"tomorrow at 6pm\""}`,
	)
	p := newPump(t)
	p.agentWorker.Loop.Model = agent.NewGateway(config.Agent{URL: server.URL, Model: "local-model"})
	p.start()

	first := p.text(t, "fetch kids from school tomorrow at 5pm", "Asia/Singapore")
	p.settle(t, first.JobID, status(store.JobComplete), "the reference Reminder")

	second := p.text(t, "buy glue on the way home from school", "Asia/Tokyo")
	p.settle(t, second.JobID, status(store.JobClarifying), "the capture that needs a time")

	// The committed run traced the words it read the time from, and the ask traced the
	// fields the capture never gave: what the model did is checkable after the fact.
	if got := traceField(p.lastTrace(t, first.JobID), "evidence"); got != `the user said "tomorrow at 5pm"` {
		t.Fatalf("evidence = %q, want the model's own words for the time", got)
	}
	if got := strings.Join(traceFields(p.lastTrace(t, second.JobID), "missing"), ","); got != "event,zone,date,time" {
		t.Fatalf("missing = %s, want the fields an empty proposal leaves open", got)
	}

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
	// The answered capture committed its own Reminder, and its run traced the reply's
	// words for the time rather than the first capture's.
	if got := traceField(p.lastTrace(t, second.JobID), "evidence"); got != `the user said "tomorrow at 6pm"` {
		t.Fatalf("evidence = %q, want the reply's own words", got)
	}
	if got := p.reminders(t); got != 2 {
		t.Fatalf("reminders = %d, want one per capture", got)
	}
}

// A Job that trips the poison pill is recoverable: the user retries it, and the
// same Job commits on the fresh budget. This is the PRD's "clarification gets
// stuck" recovery, and it crosses the fail path, the reply path, and a re-run.
func TestPipelineFailedJobRecoversOnReply(t *testing.T) {
	transient := errors.New("agent: call model: connection refused")
	p := newPump(t, reminderTurn("buy glue", "tomorrow at 6pm"))
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
		answer("buy glue tomorrow at 6pm", reminderTurn("buy glue", "tomorrow at 6pm"))
	// A clip whose transcript carries a clock reaches the same commit as the
	// typed capture, through its own ingress.
	p.captureWorker.STT = scriptedTranscriber{text: "buy glue tomorrow at 6pm"}
	p.start()
	client, base := p.api(t, "lam@example.com")

	posted := envelopeJob(t, p.post(t, client, base+"/api/jobs",
		map[string]any{"text": "buy glue tomorrow at 6pm"}, http.StatusCreated))
	p.settle(t, posted.JobID, status(store.JobComplete), "the typed capture")

	uploaded := p.upload(t, client, base+"/api/jobs", "Asia/Singapore")
	p.settle(t, uploaded.JobID, status(store.JobComplete), "the recorded capture")
}

// A clarification answered by voice, driven the way the thread drives it: a
// recording posted to the Job's messages route resumes the same conversation.
// This is the one-ingress promise — a turn is a turn — over real HTTP.
func TestPipelineHTTPVoiceReplyAfterClarificationCommits(t *testing.T) {
	p := newPump(t).
		answer("buy glue", agent.Turn{}).
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

// Tags are search features rather than extra text: a Memory whose own words a later capture
// never uses still comes back, because its tag does. This is the pipeline proof that a fact is
// tagged on creation and that the tags are what BM25 finds it by next time.
func TestPipelineTagsMakeAMemoryFindableByOtherWording(t *testing.T) {
	const fact = "milk is cheaper at store a"
	p := newPump(t).
		answer("remember "+fact, memoryTurn(fact)).
		answer("buy grocery", memoryTurn("grocery"))
	// The tagger rides the model port, exactly as the gateway and the dev fake do.
	p.model.tags = []string{"grocery", "price", "milk"}
	p.start()

	first := p.text(t, "remember "+fact, "Asia/Singapore")
	p.settle(t, first.JobID, status(store.JobComplete), "the tagged Memory")

	// The features are generated behind the commit, so the flow waits for them rather than
	// reading the row the moment the Job closes.
	if tags := p.tagged(t, fact); tags != "grocery,price,milk" {
		t.Fatalf("tags = %q, want the generated features on the row", tags)
	}

	// "buy grocery" shares no word with the Memory's text; only the tag brings it back,
	// which is the whole point of generating features.
	second := p.text(t, "buy grocery", "Asia/Singapore")
	p.settle(t, second.JobID, status(store.JobComplete), "the capture that recalls by tag")

	if in := p.plannedFor(t, "buy grocery"); !recalled(in, fact) {
		t.Fatalf("remembered = %+v, want the tag-recalled Memory", in.Remembered)
	}
}

// One capture can be both records: the reminder interrupts, the fact is kept for later.
// Both are committed in one step, the Memory carries the features generated for it, and
// the user reads one sentence rather than two.
func TestPipelineOneCaptureCommitsBothKinds(t *testing.T) {
	server, script := newGatewayScript(t,
		`{"text":"Lunch at Ikea","memoryText":"I have an Ikea voucher to use by December","zone":"Asia/Singapore","date":"2026-10-30","time":"13:00","recurrence":"none","evidence":"the user said \"this Friday at 1pm\""}`,
	)
	// The tagger's answer as the gateway would give it: normalized on the way to the row.
	script.tags = "Voucher, ikea, expiry, voucher"
	p := newPump(t)
	p.agentWorker.Loop.Model = agent.NewGateway(config.Agent{URL: server.URL, Model: "local-model"})
	p.start()

	job := p.text(t, "lunch at Ikea this Friday at 1pm, and I have an Ikea voucher to use by December",
		"Asia/Singapore")
	done := p.settle(t, job.JobID, status(store.JobComplete), "the commit of both kinds")

	var text, local, emit string
	if err := p.handle.Read.QueryRow(
		`SELECT Text, LocalDateTime, EmitAtUtc FROM Reminder`).Scan(&text, &local, &emit); err != nil {
		t.Fatalf("read reminder: %v", err)
	}
	if text != "Lunch at Ikea" || local != "2026-10-30T13:00" || emit != "2026-10-30T05:00:00Z" {
		t.Fatalf("reminder = %q %q %q, want the resolved local time and its instant", text, local, emit)
	}
	// The features land behind the commit, so the flow waits for the write it is asserting.
	const fact = "I have an Ikea voucher to use by December"
	if tags := p.tagged(t, fact); tags != "voucher,ikea,expiry" {
		t.Fatalf("tags = %q, want the features the tagger answered", tags)
	}
	var source int64
	if err := p.handle.Read.QueryRow(
		`SELECT SourceJobId FROM Memory`).Scan(&source); err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if source != job.JobID {
		t.Fatalf("memory source = %d, want the Job that committed it", source)
	}

	// One reply for the turn — the Reminder's, because a fact does not interrupt — and no
	// push: a commit has nothing to tell the user the thread does not already say.
	if last := done.Conversation[len(done.Conversation)-1]; last.Role != "assistant" || last.Text != "Reminder set." {
		t.Fatalf("reply = %+v, want the commit sentence", last)
	}
	if calls, _, _ := p.pusher.sent(); calls != 0 {
		t.Fatalf("pushes = %d, want a commit to push nothing", calls)
	}
	// One run, one Trace row, both counts, and the evidence beside them.
	trace := p.lastTrace(t, job.JobID)
	if traceCount(trace, "reminders") != 1 || traceCount(trace, "memories") != 1 {
		t.Fatalf("trace = %+v, want one of each kind recorded", trace)
	}
	if got := traceField(trace, "evidence"); got != `the user said "this Friday at 1pm"` {
		t.Fatalf("evidence = %q, want the model's own words for the time", got)
	}
}

// A repeat is committed as a rule rather than as a one-off with a hidden series: the
// rule is what every later instant is derived from, and its calendar part comes from the
// first occurrence the model resolved. Two patterns, two ingresses, both record tables.
func TestPipelineRecurringCaptureCommitsARule(t *testing.T) {
	server, script := newGatewayScript(t,
		`{"text":"Take pills","memoryText":"","zone":"Asia/Singapore","date":"2026-11-05","time":"09:00","recurrence":"monthly","evidence":"the user said \"every month on the 5th at 9am\""}`,
		`{"text":"Renew passport","memoryText":"","zone":"Asia/Singapore","date":"2027-03-03","time":"08:00","recurrence":"yearly","evidence":"the user said \"every year on March 3rd at 8am\""}`,
	)
	p := newPump(t)
	p.agentWorker.Loop.Model = agent.NewGateway(config.Agent{URL: server.URL, Model: "local-model"})
	p.captureWorker.STT = scriptedTranscriber{text: "remind me to take pills every month on the 5th at 9am"}
	p.start()

	// The spoken repeat, through the transcription stage, then the typed one chained onto
	// the same running stack.
	p.settle(t, p.voice(t, "Asia/Singapore").JobID, status(store.JobComplete), "the monthly Reminder")
	p.settle(t, p.text(t, "remind me to renew my passport every year on March 3rd at 8am", "Asia/Singapore").JobID,
		status(store.JobComplete), "the yearly Reminder")

	// Each repeat is a rule row of its own, with no local time on the Reminder, and its
	// next instant derived from the rule and the zone: 09:00 in Singapore is 01:00 UTC.
	for _, want := range []struct {
		text string
		rule store.Rule
		next string
	}{
		{"Take pills", store.Rule{Frequency: store.FrequencyMonthly, Time: "09:00", Day: 5}, "2026-11-05T01:00:00Z"},
		{"Renew passport", store.Rule{Frequency: store.FrequencyYearly, Time: "08:00", Month: 3, Day: 3}, "2027-03-03T00:00:00Z"},
	} {
		var local, raw, next string
		if err := p.handle.Read.QueryRow(`
			SELECT COALESCE(r.LocalDateTime, ''), c.Rule, c.NextEmitAtUtc
			FROM Reminder r JOIN RecurringReminder c ON c.ReminderId = r.ReminderId
			WHERE r.Text = ?`, want.text).Scan(&local, &raw, &next); err != nil {
			t.Fatalf("read the rule for %q: %v", want.text, err)
		}
		var rule store.Rule
		if err := json.Unmarshal([]byte(raw), &rule); err != nil {
			t.Fatalf("decode the rule for %q: %v (%s)", want.text, err, raw)
		}
		if rule != want.rule || next != want.next {
			t.Fatalf("rule for %q = %+v %s, want %+v %s", want.text, rule, next, want.rule, want.next)
		}
		if local != "" {
			t.Fatalf("local date time for %q = %q, want a repeat to carry none", want.text, local)
		}
	}
	// And the repeat is context for what comes next: the second capture's prompt carries
	// the first one's next occurrence, which is what a correction needs to see.
	planned := script.call(t, "remind me to renew my passport every year on March 3rd at 8am")
	content := planned.Messages[len(planned.Messages)-1].Content
	if !strings.Contains(content, "- Take pills (2026-11-05T09:00 monthly Asia/Singapore)") {
		t.Fatalf("prompt = %q, want the repeat in the local zone with its pattern", content)
	}
	// The one-off column stays for one-offs: neither repeat left an instant there.
	if got := p.count(t, `SELECT count(*) FROM Reminder WHERE EmitAtUtc IS NOT NULL`); got != 0 {
		t.Fatalf("one-off reminders = %d, want the repeats to carry a rule instead", got)
	}
	if got := p.reminders(t); got != 2 {
		t.Fatalf("reminders = %d, want one per capture", got)
	}
}

// The question a capture earns is written for the fields the check could not accept,
// and those fields ride the request: the model is told which half of the record the
// user still owes instead of inferring it from the capture's own words.
func TestPipelineQuestionRequestNamesTheMissingFields(t *testing.T) {
	server, script := newGatewayScript(t,
		`{"text":"Buy glue","memoryText":"","zone":"Asia/Singapore","date":"2026-10-25","time":"","recurrence":"none","evidence":"the user said \"when I'm home\""}`,
	)
	p := newPump(t)
	p.agentWorker.Loop.Model = agent.NewGateway(config.Agent{URL: server.URL, Model: "local-model"})
	p.start()

	job := p.text(t, "buy glue when I'm home", "Asia/Singapore")
	asked := p.settle(t, job.JobID, status(store.JobClarifying), "the question about the time")

	// A date with no clock is one field short, and the prompt says which one.
	missing := script.questionCall(t, "buy glue when I'm home")
	content := missing.Messages[len(missing.Messages)-1].Content
	if !strings.Contains(content, "missing: time\n") {
		t.Fatalf("question prompt = %q, want the field the capture left open", content)
	}
	if strings.Contains(content, "missing: date") {
		t.Fatalf("question prompt = %q, want only the field that is actually missing", content)
	}

	// The durable half of the same ask: nothing committed, the question in the thread,
	// and the Trace naming the one field.
	if got := p.reminders(t); got != 0 {
		t.Fatalf("reminders = %d, want an ask to commit nothing", got)
	}
	if got := strings.Join(traceFields(p.lastTrace(t, job.JobID), "missing"), ","); got != "time" {
		t.Fatalf("missing = %s, want the one field the model left empty", got)
	}
	if last := asked.Conversation[len(asked.Conversation)-1]; last.Role != "assistant" || last.Text == "" {
		t.Fatalf("question turn = %+v, want the question the user answers", last)
	}
}

// A Job whose answers keep missing the mark stops spending calls on the question: after
// two written ones the third is the server's own sentence, so the user is asked again
// rather than charged for a model that would only repeat itself.
func TestPipelineThirdClarificationFallsBackWithoutACall(t *testing.T) {
	server, script := newGatewayScript(t,
		`{}`, `{}`, `{}`,
		`{"text":"Buy glue","memoryText":"","zone":"Asia/Singapore","date":"2026-10-25","time":"18:00","recurrence":"none","evidence":"the user said \"tomorrow at 6pm\""}`,
	)
	p := newPump(t)
	p.agentWorker.Loop.Model = agent.NewGateway(config.Agent{URL: server.URL, Model: "local-model"})
	p.start()

	job := p.text(t, "buy glue when I'm home", "Asia/Singapore")
	p.settle(t, job.JobID, status(store.JobClarifying), "the first question")

	// Two answers with nothing Blurt can act on, each earning another question.
	for round, answer := range []string{"sometime soon", "at some point"} {
		p.reply(t, job.JobID, store.Message{Text: answer, TZ: "Asia/Singapore"})
		p.settle(t, job.JobID, func(j store.Job) bool { return len(j.Conversation) == 4+2*round },
			"the question that answer earns")
	}
	// Two calls, three questions: the third is the server's sentence, and it is not the
	// one the model would have written.
	if got := script.asked(); got != 2 {
		t.Fatalf("question calls = %d, want two before the fallback", got)
	}
	asked := p.load(t, job.JobID)
	if len(asked.Conversation) != 6 {
		t.Fatalf("conversation = %+v, want three questions kept", asked.Conversation)
	}
	for _, at := range []int{1, 3, 5} {
		if turn := asked.Conversation[at]; turn.Role != "assistant" || !strings.HasSuffix(turn.Text, "?") {
			t.Fatalf("question %d = %+v, want the user asked", at/2+1, turn)
		}
	}
	if third, first := asked.Conversation[5].Text, asked.Conversation[1].Text; third == first {
		t.Fatalf("third question = %q, want the fallback rather than the model's sentence", third)
	}
	if got := p.reminders(t); got != 0 {
		t.Fatalf("reminders = %d, want three asks to commit nothing", got)
	}

	// The answer that finally resolves it commits on a fourth run.
	p.reply(t, job.JobID, store.Message{Text: "tomorrow at 6pm", TZ: "Asia/Singapore"})
	done := p.settle(t, job.JobID, status(store.JobComplete), "the answer that lands")
	if len(done.Conversation) != 8 {
		t.Fatalf("conversation = %+v, want the commit sentence last", done.Conversation)
	}
	if got := strings.Join(p.outcomes(t, job.JobID, store.AgentStage), ","); got !=
		"clarifying,reply,clarifying,reply,clarifying,reply,complete" {
		t.Fatalf("outcomes = %s, want three asks and then the commit", got)
	}
	if got := p.reminders(t); got != 1 {
		t.Fatalf("reminders = %d, want the one the fourth answer committed", got)
	}
}

// A Memory whose features cannot be generated is still committed: the tag call is best
// effort, so a capture the user already made never fails on it. The row lands untagged
// rather than not at all, and the Job closes on one clean run.
func TestPipelineTaggerFailureStillCommitsTheMemory(t *testing.T) {
	p := newPump(t).answer("remember milk is cheaper at store a", memoryTurn("milk is cheaper at store a"))
	p.model.tagErr = errors.New("agent: call model: connection refused")
	p.start()

	job := p.text(t, "remember milk is cheaper at store a", "Asia/Singapore")
	done := p.settle(t, job.JobID, status(store.JobComplete), "the Memory without its features")

	var fact, tags string
	if err := p.handle.Read.QueryRow(`SELECT Text, Tags FROM Memory`).Scan(&fact, &tags); err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if fact != "milk is cheaper at store a" || tags != "" {
		t.Fatalf("memory = %q %q, want the fact kept untagged", fact, tags)
	}
	// The reply is the Memory's own sentence, and the run is one clean step: no retry,
	// no failure, nothing left to hand a worker.
	if last := done.Conversation[len(done.Conversation)-1]; last.Text != "Noted." {
		t.Fatalf("reply = %+v, want the fact's own sentence", last)
	}
	if got := strings.Join(p.outcomes(t, job.JobID, store.AgentStage), ","); got != "complete" {
		t.Fatalf("outcomes = %s, want one committed run", got)
	}
	if done.RetryCount != 0 {
		t.Fatalf("retry count = %d, want the tag call not to cost a retry", done.RetryCount)
	}
	p.drain(t, p.agent, "agent queue")
}

// Blurt's two memory halves meet: what the questionnaire asked is context for a later
// capture. The answers land as questionnaire Memory over the real ingress, and the agent's
// retrieval finds one for a capture that shares only a word with it.
func TestPipelineQuestionnaireAnswersBecomeCaptureContext(t *testing.T) {
	const answer = "Where do you shop for groceries?\nStore A is cheapest for milk"
	p := newPump(t).answer("buy milk", reminderTurn("buy milk", "tomorrow at 6pm"))
	p.start()
	client, base := p.api(t, "lam@example.com")

	p.post(t, client, base+"/api/questionnaire", map[string]any{
		"version": 1,
		"answers": []map[string]string{
			{"question": "Where do you shop for groceries?", "answer": "Store A is cheapest for milk"},
		},
	}, http.StatusOK)
	if got := p.memories(t); got != 1 {
		t.Fatalf("memories = %d, want the answer stored", got)
	}

	// The capture is worded nothing like the answer except for the one noun.
	job := p.text(t, "buy milk", "Asia/Singapore")
	p.settle(t, job.JobID, status(store.JobComplete), "the capture that reads the answer")

	if in := p.plannedFor(t, "buy milk"); !recalled(in, answer) {
		t.Fatalf("remembered = %+v, want the questionnaire answer as context", in.Remembered)
	}
	// The capture wrote its own record and left the answer alone, still marked as the
	// questionnaire's.
	if got := p.memories(t); got != 1 {
		t.Fatalf("memories = %d, want the answer untouched", got)
	}
	if got := p.reminders(t); got != 1 {
		t.Fatalf("reminders = %d, want the capture's own Reminder", got)
	}
	var tags string
	if err := p.handle.Read.QueryRow(`SELECT Tags FROM Memory`).Scan(&tags); err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if tags != "questionnaire" {
		t.Fatalf("tags = %q, want the answer still the questionnaire's", tags)
	}
}

// An answer that still leaves the record short earns a second question rather than a
// guess: two ask exits in one Job, each its own Trace row and its own push, and the third
// answer is the one that commits.
func TestPipelineSecondClarificationThenCommit(t *testing.T) {
	p := newPump(t, agent.Turn{}, agent.Turn{}, reminderTurn("buy glue", "tomorrow at 6pm")).start()
	job := p.text(t, "buy glue when I'm home", "Asia/Singapore")

	first := p.settle(t, job.JobID, status(store.JobClarifying), "the first question")
	if len(first.Conversation) != 2 {
		t.Fatalf("conversation = %+v, want the capture and the question", first.Conversation)
	}
	// An ask commits nothing, and the push is the only thing that reaches the user away
	// from the thread, so there is one and it says what the thread says.
	if got := p.reminders(t); got != 0 {
		t.Fatalf("reminders = %d, want an ask to commit nothing", got)
	}
	p.soon(t, func() bool { calls, _, _ := p.pusher.sent(); return calls == 1 }, "the first push")
	if _, _, pushed := p.pusher.sent(); pushed != first.Conversation[1].Text {
		t.Fatalf("push = %q, want the question the thread carries", pushed)
	}

	// An answer with no clock in it: the Job asks again rather than inventing one.
	p.reply(t, job.JobID, store.Message{Text: "sometime soon", TZ: "Asia/Singapore"})
	asked := p.settle(t, job.JobID, func(j store.Job) bool { return len(j.Conversation) == 4 },
		"the second question")
	if got := p.reminders(t); got != 0 {
		t.Fatalf("reminders = %d, want the second ask to commit nothing either", got)
	}
	if got := strings.Join(traceFields(p.lastTrace(t, job.JobID), "missing"), ","); got != "event,zone,date,time" {
		t.Fatalf("missing = %s, want the fields the second capture left open", got)
	}
	p.soon(t, func() bool { calls, _, _ := p.pusher.sent(); return calls == 2 }, "the second push")
	if len(asked.Conversation) != 4 {
		t.Fatalf("conversation = %+v, want both questions kept", asked.Conversation)
	}

	p.reply(t, job.JobID, store.Message{Text: "tomorrow at 6pm", TZ: "Asia/Singapore"})
	done := p.settle(t, job.JobID, status(store.JobComplete), "the answer that lands")
	// Every move the Job made is in the trace, the user's own turns included: two asks
	// answered by two replies, and then the commit.
	if got := strings.Join(p.outcomes(t, job.JobID, store.AgentStage), ","); got != "clarifying,reply,clarifying,reply,complete" {
		t.Fatalf("outcomes = %s, want two asks and then the commit", got)
	}
	if len(done.Conversation) != 6 {
		t.Fatalf("conversation = %+v, want capture, question, answer, question, answer, reply", done.Conversation)
	}
	if got := p.reminders(t); got != 1 {
		t.Fatalf("reminders = %d, want the one the third answer committed", got)
	}
	if calls, _, _ := p.pusher.sent(); calls != 2 {
		t.Fatalf("pushes = %d, want the commit to push nothing", calls)
	}
}
