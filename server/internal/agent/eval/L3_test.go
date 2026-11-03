package eval

// The L3 lane: the chain. A case seeds the database, sends text or recorded captures
// through the two workers production boots, and asserts the rows that land. Nothing here
// stubs a model: the planner and the two cheap stages are the deployment's, so a failure
// is the chain's, not one call's.
//
// This is the only lane that can ask the two questions a single turn cannot:
//
//   - the feedback loop — a capture becomes a question, the answer becomes the row;
//   - many schedules in one database — a trip, then something planned inside it.
//
// It is coarse on purpose. A row's prose and a Memory's tags belong to a model, so a case
// pins a substring and a tag count, and leaves "which field missed" to L1 and L2.
//
//	go test ./internal/agent/eval -run TestChainCases -eval -layer L3 -v -timeout 40m

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"blurt/internal/agent"
	"blurt/internal/auth"
	"blurt/internal/backend"
	"blurt/internal/capture"
	"blurt/internal/config"
	"blurt/internal/db"
	"blurt/internal/queue"
	"blurt/internal/store"
	"blurt/internal/stt"
)

const (
	// chainWait is how long one step may take: the loop's own job wall plus a worker's
	// poll, because a hosted planner answers in tens of seconds and may buy a retry.
	chainWait = 6 * time.Minute
	// tagWait bounds the tagger, which runs behind the commit rather than inside it.
	tagWait = 2 * time.Minute
	// chainPoll is how often the test asks the thread whether a Job has settled; the
	// workers (not the poll) are what moves it.
	chainPoll = 20 * time.Millisecond
	// audioFixtures is where an L3 case's clips live, relative to the package.
	audioFixtures = caseDir + "/_audio"
)

// chainArm is one chain run's three models: the planner the user configured (built by the
// L1 arm, prompt arm and all) and the deployment's two cheap stages.
type chainArm struct {
	name      string
	planner   agent.Model
	prescreen agent.Stage
	tags      agent.Stage
}

// chainArmFor builds the arm from .env the way the deployment wires it. All three are
// required: a chain that fell back to the planner for a cheap stage would not be the chain
// the user runs, and a missing id would score the wrong stage as the failure.
func chainArmFor(t *testing.T) (chainArm, string) {
	t.Helper()
	settings, skip := evalSettings()
	if skip != "" {
		return chainArm{}, skip
	}
	helpers, skip := helperArmFor(t, *evalPrompt, nil)
	if skip != "" {
		return chainArm{}, skip
	}
	planner := evalArm(t, settings, nil)
	ids := []string{settings.Model}
	for _, stage := range []string{StagePrescreen, StageTags} {
		ids = append(ids, helpers.stages[stage].id)
	}
	return chainArm{
		name:      strings.Join(distinct(ids), "+") + "-" + *evalPrompt,
		planner:   planner.model,
		prescreen: agent.Stage{Model: helpers.stages[StagePrescreen].model, Name: helpers.stages[StagePrescreen].id},
		tags:      agent.Stage{Model: helpers.stages[StageTags].model, Name: helpers.stages[StageTags].id},
	}, ""
}

// chainRun is one case's world: the backend main boots, seeded with the case's rows on a
// fresh database and serving the HTTP surface a browser talks to. Its only test-specific
// parts are the models and the pinned clock.
type chainRun struct {
	c      Case
	handle *db.DB
	app    *backend.Backend
	server *httptest.Server
	client *http.Client
	// opened is every Job the chain opened, in order, as the API reported it: a reply goes
	// to the newest one still waiting on the user, and nothing here reads the store to find
	// out which that is.
	opened []store.Job
	stop   context.CancelFunc
}

// newChainRun boots the stack and signs a user in through the dev PIN, so a case starts
// where a real client does: at the ingress. Only the two stage workers run — no claimer,
// sweeper or emitter may move a Job behind an assertion.
func newChainRun(t *testing.T, c Case, a chainArm) *chainRun {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	handle, err := db.Open(dir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	audio, err := capture.NewStore(dir)
	if err != nil {
		t.Fatalf("open audio store: %v", err)
	}
	users := store.NewUserStore(handle.Write, handle.Read)
	user, err := users.EnsureActive(ctx, evalName, evalEmail)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	if err := seedCase(ctx, handle, user.UserID, c); err != nil {
		t.Fatalf("seed %s: %v", c.Name, err)
	}
	now, _, err := c.anchor()
	if err != nil {
		t.Fatalf("anchor %s: %v", c.Name, err)
	}
	jobs := store.NewJobStore(handle.Write, handle.Read)
	reminders := store.NewReminderStore(handle.Write, handle.Read)
	queues := &queue.Queues{TranscriptionQueue: queue.NewMock(), AgentQueue: queue.NewMock()}
	pusher := &recordedPush{}
	app := backend.New(backend.Deps{
		DB: handle, Users: users, Jobs: jobs,
		Sessions:      store.NewSessionStore(handle.Write, handle.Read),
		Reminders:     reminders,
		Notifications: store.NewNotificationStore(handle.Write, handle.Read),
		Audio:         audio, Queues: queues,
		Loop: &agent.AgentLoop{
			Store: jobs,
			Recall: agent.Retrieval{
				Reminders: reminders,
				Memories:  store.NewMemoryStore(handle.Read),
			},
			Planner:        agent.Stage{Model: a.planner, Name: a.name},
			Prescreen:      a.prescreen,
			Tags:           a.tags,
			Pusher:         pusher,
			AgentQueue:     queues.AgentQueue,
			Timeout:        evalTimeout,
			AttemptTimeout: evalAttemptTimeout,
			Now:            func() time.Time { return now },
		},
		STT: sttTranscriber(), STTModel: sttModel(),
		Pins:   auth.NewPinStore(time.Minute, auth.DevPIN),
		Pusher: pusher, Poll: chainPoll,
		AllowedOrigins: []string{evalOrigin},
	})
	runCtx, stop := context.WithCancel(context.Background())
	app.Start(runCtx)
	server := httptest.NewServer(app.Handler())
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	run := &chainRun{
		c: c, handle: handle, app: app, server: server, client: &http.Client{Jar: jar},
		stop: stop,
	}
	t.Cleanup(func() {
		server.Close()
		run.stop()
		run.app.Wait()
		handle.Close()
	})
	run.post(t, "/api/auth/request-pin", map[string]string{"email": evalEmail}, http.StatusOK, nil)
	run.post(t, "/api/auth/verify-pin",
		map[string]any{"email": evalEmail, "pin": auth.DevPIN}, http.StatusOK, nil)
	return run
}

// The signed-in account every case runs as, and the origin the CSRF guard accepts. A turn
// without one is refused before a handler reads it.
const (
	evalName   = "Eval"
	evalEmail  = "eval@example.com"
	evalOrigin = "http://blurt.test"
)

// sttTranscriber is the gateway Blurt ships against: the same pair the helpers ride, because
// phonon is served from it.
func sttTranscriber() stt.Transcriber {
	return stt.NewHTTP(config.STT{
		URL:    envValue("BLURT_STT_URL"),
		APIKey: envValue("BLURT_STT_API_KEY"),
		Model:  sttModel(),
	})
}

// sttModel is the id the transcription worker reports; the gateway takes one.
func sttModel() string {
	if model := envValue("BLURT_STT_MODEL"); model != "" {
		return model
	}
	return "phonon"
}

// turn drives one step through the API and returns the Job it left: a capture posts to the
// ingress, a reply posts to the Job's own thread, and both wait for the workers to settle it.
func (r *chainRun) turn(t *testing.T, step Step) store.Job {
	t.Helper()
	index, path, body := -1, "/api/jobs", any(map[string]string{"text": step.Capture})
	shape, want := (func(*http.Request))(nil), http.StatusCreated
	switch {
	case step.Reply != "":
		index = r.waiting(t)
		path = "/api/jobs/" + strconv.FormatInt(r.opened[index].JobID, 10) + "/messages"
		body, want = map[string]string{"text": step.Reply}, http.StatusOK
	case step.Clip != "":
		audio, err := os.ReadFile(filepath.Join(audioFixtures, step.Clip))
		if err != nil {
			t.Fatalf("read clip %s: %v", step.Clip, err)
		}
		body = audio
		shape = func(req *http.Request) {
			req.Header.Set("Content-Type", "audio/wav")
			// The ingress wants the length the client recorded; the worker only ever
			// hands the blob to phonon, so the hint is not read again.
			req.Header.Set("X-Blurt-Duration-Ms", "800")
		}
	}
	job := r.settle(t, jobFrom(t, r.post(t, path, body, want, shape)).JobID)
	if index >= 0 {
		r.opened[index] = job
	} else {
		r.opened = append(r.opened, job)
	}
	return job
}

// post sends one turn the way a browser does: the device zone header, an Origin the CSRF
// guard accepts, and a JSON body or a raw clip. shape may add the upload headers.
func (r *chainRun) post(t *testing.T, path string, body any, want int, shape func(*http.Request)) []byte {
	t.Helper()
	raw, ok := body.([]byte)
	if !ok {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s body: %v", path, err)
		}
		raw = encoded
	}
	req, err := http.NewRequest(http.MethodPost, r.server.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", evalOrigin)
	req.Header.Set("X-Blurt-Timezone", r.c.CurrentTZ)
	if shape != nil {
		shape(req)
	}
	res, err := r.client.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	defer res.Body.Close()
	reply, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if res.StatusCode != want {
		t.Fatalf("post %s = %d, want %d: %s", path, res.StatusCode, want, reply)
	}
	return reply
}

// jobFrom reads the {"job": ...} envelope every turn answers with.
func jobFrom(t *testing.T, raw []byte) store.Job {
	t.Helper()
	var envelope struct {
		Job store.Job `json:"job"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	return envelope.Job
}

// waiting is the index of the newest Job still waiting on the user: the one a reply belongs
// to. Every Job's status came back from the API, so this is a read of memory, not the store.
func (r *chainRun) waiting(t *testing.T) int {
	t.Helper()
	for i := len(r.opened) - 1; i >= 0; i-- {
		if r.opened[i].Status == store.JobClarifying {
			return i
		}
	}
	t.Fatalf("this step replies, but no Job is waiting on the user")
	return -1
}

// settle polls the thread until the Job stops being open, which is the same state the app
// polls. The workers are the only thing moving it.
func (r *chainRun) settle(t *testing.T, jobID int64) store.Job {
	t.Helper()
	deadline := time.Now().Add(chainWait)
	for {
		res, err := r.client.Get(r.server.URL + "/api/jobs/" + strconv.FormatInt(jobID, 10))
		if err != nil {
			t.Fatalf("get job %d: %v", jobID, err)
		}
		raw, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("get job %d = %d: %s (%v)", jobID, res.StatusCode, raw, err)
		}
		job := jobFrom(t, raw)
		if job.Status != store.JobOpen {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for job %d; still %s after %d turns",
				jobID, job.Status, len(job.Conversation))
		}
		time.Sleep(chainPoll)
	}
}

// outcome reads one Job's own result: the rows it committed, the last agent Trace, and the
// sentence the user sees. Seeded rows carry no SourceJobId, so they never count.
func (r *chainRun) outcome(t *testing.T, job store.Job) outcome {
	t.Helper()
	ctx := context.Background()
	trace := lastAgentTrace(t, ctx, r.handle, job.JobID)
	return outcome{
		status: job.Status, reply: lastReply(job),
		reminders: readReminders(t, ctx, r.handle, job.JobID),
		updated:   readUpdated(t, ctx, r.handle, trace),
		others:    readEvery(t, ctx, r.handle),
		memories:  readMemories(t, ctx, r.handle, job.JobID),
		trace:     trace,
	}
}

// misses is one step's score: the L1 scorer over the step's own truth, plus the two things
// only a chain can check — the clip's transcript and the tags the tagger earned.
func (r *chainRun) misses(t *testing.T, step int, s Step, job store.Job) []miss {
	t.Helper()
	got := r.outcome(t, job)
	var found []miss
	add := func(m miss) {
		found = append(found, miss{fmt.Sprintf("step%d.%s", step, m.Field), m.Detail})
	}
	for _, m := range compare(Case{Expect: s.Expect}, got) {
		add(m)
	}
	if text := transcriptOf(job); len(s.TranscriptHas) > 0 {
		for _, part := range s.TranscriptHas {
			if !containsFold(text, part) {
				add(miss{"transcript", fmt.Sprintf("%q is not in the transcribed turn %q", part, text)})
				break
			}
		}
	}
	for _, memory := range s.Expect.Memories {
		if memory.TagsMin == 0 || got.status != store.JobComplete {
			continue
		}
		tags := r.waitTags(t, job.JobID, memory.Contains)
		if tags == "" {
			add(miss{"tags", "the tagger wrote nothing"})
			continue
		}
		if count := len(strings.Split(tags, ",")); count < memory.TagsMin {
			add(miss{"tags", fmt.Sprintf("%d tags, want at least %d: %s",
				count, memory.TagsMin, tags)})
		}
	}
	return found
}

// waitTags waits for the background tagger to leave its features on the Memory this step
// committed. The run closes on the commit, so the column is written after the Job settles.
// The row is found by the same substring the case asserts it with.
func (r *chainRun) waitTags(t *testing.T, jobID int64, contains []string) string {
	t.Helper()
	deadline := time.Now().Add(tagWait)
	for {
		rows, err := r.handle.Read.Query(
			`SELECT Text, Tags FROM Memory WHERE SourceJobId = ?`, jobID)
		if err != nil {
			t.Fatalf("read memory tags: %v", err)
		}
		var tags string
		for rows.Next() {
			var text, found string
			if err := rows.Scan(&text, &found); err != nil {
				rows.Close()
				t.Fatalf("scan memory tags: %v", err)
			}
			if len(contains) == 0 || containsFold(text, contains[0]) {
				tags = found
			}
		}
		rows.Close()
		if tags != "" {
			return tags
		}
		if time.Now().After(deadline) {
			t.Logf("job %d earned no tags in %s", jobID, tagWait)
			return ""
		}
		time.Sleep(chainPoll)
	}
}

// transcriptOf is the clip's own turn: the words the transcription worker wrote into the
// capture before the agent ever read it.
func transcriptOf(job store.Job) string {
	for _, turn := range job.Conversation {
		if turn.Role == "user" && turn.Text != "" {
			return turn.Text
		}
	}
	return ""
}

// describe is one step as a line: what the user did, not what the database should hold.
func describe(step Step) string {
	switch {
	case step.Reply != "":
		return "reply " + strconv.Quote(step.Reply)
	case step.Clip != "":
		return "clip " + step.Clip
	default:
		return "capture " + strconv.Quote(step.Capture)
	}
}

// runChain drives one case through the API and scores every step. The clock is pinned per
// case, so two schedules in one chain resolve against the same now.
func runChain(t *testing.T, c Case, a chainArm) reportLine {
	t.Helper()
	run := newChainRun(t, c, a)
	line := reportLine{Case: c.Name, Source: c.Source, Layer: c.Layer, Size: c.Size, Model: a.name}
	started := time.Now()
	var text strings.Builder
	for i, step := range c.Steps {
		job := run.turn(t, step)
		got := run.outcome(t, job)
		line.Calls += traceCount(got.trace, "turns")
		line.Faults = append(line.Faults, traceStrings(got.trace, "lint")...)
		line.Outcome = got.status
		for _, m := range run.misses(t, i+1, step, job) {
			if !slices.Contains(line.Fields, m.Field) {
				line.Fields = append(line.Fields, m.Field)
			}
			line.Failures = append(line.Failures, m.Field+": "+m.Detail)
		}
		fmt.Fprintf(&text, "=== step %d: %s ===\n", i+1, describe(step))
		for _, turn := range job.Conversation {
			fmt.Fprintf(&text, "%s: %s\n", turn.Role, turn.Text)
		}
		if detail, err := json.MarshalIndent(got.trace, "", "  "); err == nil {
			fmt.Fprintf(&text, "trace:\n%s\n", detail)
		}
	}
	line.LatencyMs = time.Since(started).Milliseconds()
	line.OK = len(line.Failures) == 0
	line.Runs, line.Passes = 1, btoi(line.OK)
	line.Transcript = writeTranscript(t, a.name, c, text.String())
	if *evalAudit {
		t.Logf("chain:\n%s", text.String())
	}
	for _, failure := range line.Failures {
		t.Errorf("%s", failure)
	}
	return line
}

// audit is the per-step read-out: the turn, the outcome, the rows and the words the linter
// refused. It is the lane for iterating a prompt over many schedules.
// TestChainCases is L3: one case, one fresh database, the real workers, the real provider
// behind every stage. -eval is the opt-in, so the default gate never spends a call.
func TestChainCases(t *testing.T) {
	if !*evalRun {
		t.Skip("pass -eval to run the chain against the real provider (settings come from .env)")
	}
	// L3 is its own report: running the lane without naming a layer would file it as
	// "all", which is what a whole-suite run is for.
	if *evalLayer == "" {
		defer func() { *evalLayer = "" }()
		*evalLayer = "L3"
	}
	if *evalWire {
		t.Log("wire: L3 records no provider traffic; -wire covers L1 and L2")
	}
	cases := evalCases(t, chainCase)
	if len(cases) == 0 {
		t.Skip("no L3 case matched: the other layers run as -layer L1 or L2")
	}
	a, skip := chainArmFor(t)
	if skip != "" {
		t.Skip(skip)
	}
	lines := []reportLine{}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			lines = append(lines, repeat(*evalRepeat, func() reportLine {
				return runChain(t, c, a)
			}))
		})
	}
	report := reportPath(a.name)
	if *evalAudit || *evalCase != "" {
		report = ""
	} else {
		writeReport(t, a.name, lines)
	}
	summarize(lines, a.name, report)
}

// TestChainScoresOffline pins the harness itself, with no provider at all: the scripted
// double asks once and then commits, and the chain's own path — worker, stream, AppendTurn,
// settle, row read — is what the assertion crosses. A harness that has never been run is the
// one thing an eval cannot afford.
func TestChainScoresOffline(t *testing.T) {
	c, err := loadCase(caseDir, "chain-question-asks-then-commits.json")
	if err != nil {
		t.Fatalf("load case: %v", err)
	}
	line := runChain(t, c, chainArm{
		name: "scripted",
		planner: &scripted{turns: []agent.Answer{
			{Kind: agent.AnswerQuestion, Question: "When should I remind you about buying glue?"},
			answerWith(oneOff("Buy glue", "2026-10-25", "18:00")),
		}},
		prescreen: agent.Stage{Model: sideStage{}},
		tags:      agent.Stage{Model: sideStage{}},
	})
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(line.Transcript)) })
	if !line.OK {
		t.Fatalf("the harness failed its own chain: %v", line.Failures)
	}
	if line.Outcome != store.JobComplete || line.Calls != 4 {
		t.Fatalf("outcome %s, calls %d, want a complete chain on two runs of two calls",
			line.Outcome, line.Calls)
	}
	if text, err := os.ReadFile(line.Transcript); err != nil || !strings.Contains(string(text), "Reminder set.") {
		t.Fatalf("the transcript is missing the commit sentence: %v", err)
	}
}
