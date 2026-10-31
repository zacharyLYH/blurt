package l1

// The L2 lane: the screen, the post-agent check and the tagger, one call each. These are the
// stages L1 stubs out (sideStage) and the ones a split deployment runs on the home gateway, so
// the arm is built from the same .env the deployment boots on: one model id per stage, over the
// gateway that serves phonon.
//
// One case is one call. That is what makes -repeat cheap enough to mean something here — a
// screen that refuses one good capture in ten is invisible in a single run — and it is what
// makes this the layer that tells an L3 failure apart: the transcript says which stage
// disagreed.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"blurt/internal/agent"
)

// helperTimeout bounds one helper call. The gateway answers in a second or two, so this is the
// wall that turns a hung stage into a failed case rather than a stuck binary.
const helperTimeout = 2 * time.Minute

// helperStage is one gateway stage as the deployment wires it: the model id it is asked under,
// and the model that asks it.
type helperStage struct {
	id    string
	model agent.Model
}

// helperArm is the two stages one L2 run asks, plus the name its report and transcripts land
// under. The prompt arm is already applied: an override where the arm has a file, the shipped
// prompt where it does not.
type helperArm struct {
	name   string
	url    string
	stages map[string]helperStage
}

// helperArmFor builds the arm from .env the way main.go wires the deployment: the gateway that
// serves phonon, one id per stage, and the URL and key falling back to the STT pair exactly as
// config.go does. A missing id or endpoint is a skip with the reason, not a failed run — the
// helper lane is opt-in and the ids belong to the deployment, not to the repo.
//
// When the run asked for the wire, every stage's client is pointed at the recording proxy
// instead: the envelope that reaches the gateway is the one thing a transcript cannot show.
func helperArmFor(t *testing.T, prompt string, requests *wire) (helperArm, string) {
	t.Helper()
	base := envValue("BLURT_AUX_URL")
	key := envValue("BLURT_AUX_API_KEY")
	if base == "" {
		base = envValue("BLURT_STT_URL")
	}
	if key == "" {
		key = envValue("BLURT_STT_API_KEY")
	}
	if base == "" || key == "" {
		return helperArm{}, "set BLURT_STT_URL and BLURT_STT_API_KEY (or the BLURT_AUX_* pair) to score the helper stages"
	}
	stages := []struct {
		name        string
		key         string
		temperature float64
	}{
		// A verdict is a decision, so it runs greedy; the tagger's answer is a guess at
		// words nobody said, and heat is what finds them.
		{StagePrescreen, "BLURT_PRESCREEN_MODEL", 0},
		{StageTags, "BLURT_TAGS_MODEL", 1},
	}
	ids := make([]string, 0, len(stages))
	for _, stage := range stages {
		id := envValue(stage.key)
		if id == "" {
			return helperArm{}, stage.key + " is not set: every helper stage names its own model"
		}
		ids = append(ids, id)
	}
	// Two stages that share one model should not read as two in a report name.
	arm := helperArm{
		name: strings.Join(distinct(ids), "+") + "-" + prompt,
		url:  base, stages: map[string]helperStage{},
	}
	if requests != nil {
		server := httptest.NewServer(http.HandlerFunc(requests.handler))
		t.Cleanup(server.Close)
		t.Logf("wire: %s -> %s", server.URL, base)
		requests.target, requests.run = base, runName(arm.name)
		arm.url = server.URL
	}
	for i, stage := range stages {
		client := agent.NewModelClient(agent.Settings{
			Provider: agent.ProviderGateway, URL: arm.url, APIKey: key, Model: ids[i],
			Temperature: stage.temperature,
		})
		switch override := helperPrompt(t, prompt, stage.name); stage.name {
		case StagePrescreen:
			client.PrescreenPrompt = override
		case StageTags:
			client.TagsPrompt = override
		}
		arm.stages[stage.name] = helperStage{id: ids[i], model: client}
	}
	return arm, ""
}

// distinct is the values once each, in order.
func distinct(values []string) []string {
	seen := map[string]bool{}
	kept := []string{}
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		kept = append(kept, value)
	}
	return kept
}

// helperPrompt resolves one stage's prompt for a named arm: cases/_prompts/<arm>-<stage>.txt,
// the L1 convention with the stage appended. Nothing in the repo ships such a file, so `full`
// is the only arm until an experiment adds one; a named arm without its file is a fatal, not a
// silent fallback to the shipped prompt. The full arm answers empty, which the client reads as
// the shipped prompt.
func helperPrompt(t *testing.T, arm, stage string) string {
	t.Helper()
	if arm == "full" {
		return ""
	}
	path := filepath.Join(promptDir, arm+"-"+stage+".txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read prompt arm %s: %v", path, err)
	}
	return strings.TrimSpace(string(raw))
}

// TestHelperCases is L2: the two helper stages scored one call at a time, against the models
// the deployment names. Nothing queues and no database is seeded, so a run is seconds and
// -repeat is worth its time.
//
//	go test ./internal/agent/L1 -run TestHelperCases -eval -layer L2 -repeat 10 -v
func TestHelperCases(t *testing.T) {
	if !*evalRun {
		t.Skip("pass -eval to run the cases against the real gateway (settings come from .env)")
	}
	cases := evalCases(t, helperCase)
	if len(cases) == 0 {
		t.Skip("no L2 case matched: the agent loop runs as -layer L1")
	}
	var requests *wire
	if *evalWire {
		requests = &wire{dir: reportDir, client: &http.Client{Timeout: 5 * time.Minute}}
	}
	arm, skip := helperArmFor(t, *evalPrompt, requests)
	if skip != "" {
		t.Skip(skip)
	}
	lines := []reportLine{}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			lines = append(lines, repeat(*evalRepeat, func() reportLine {
				requests.forCase(c.Name)
				return runHelperCase(t, c, arm)
			}))
		})
	}
	report := reportPath(arm.name)
	if *evalAudit || *evalCase != "" {
		// Nothing worth pointing at: -audit writes no report, and a single-case run must not
		// overwrite the whole layer's scorecard with one line.
		report = ""
	} else {
		writeReport(t, arm.name, lines)
	}
	summarize(lines, arm.name, report)
}

// helperOutcome is one helper answer as the scorer reads it: the verdict a screen gave, the
// words written beside it, and the features a tags call earned.
type helperOutcome struct {
	verdict string
	reason  string
	tags    []string
}

// label is the answer in one word, for the report's outcome column.
func (o helperOutcome) label() string {
	if o.verdict != "" {
		return o.verdict
	}
	return StageTags
}

// summary is the answer as the transcript reads it.
func (o helperOutcome) summary() string {
	var out strings.Builder
	if o.verdict != "" {
		fmt.Fprintf(&out, "verdict: %s\n", o.verdict)
	}
	if o.reason != "" {
		fmt.Fprintf(&out, "reason: %s\n", o.reason)
	}
	if o.tags != nil {
		fmt.Fprintf(&out, "tags: %s\n", strings.Join(o.tags, ","))
	}
	return out.String()
}

// runHelperCase asks one stage one question and scores what it answered. One case is one call,
// so the transcript is the whole run: what the stage read, and what it wrote back.
func runHelperCase(t *testing.T, c Case, arm helperArm) reportLine {
	t.Helper()
	stage, ok := arm.stages[c.Stage]
	if !ok {
		t.Fatalf("case %s: no model for stage %q", c.Name, c.Stage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), helperTimeout)
	defer cancel()

	// What the stage reads, rendered the way it was sent: a screen reads the words alone, the
	// tagger reads the fact.
	request := ""
	var ask func() (helperOutcome, error)
	switch c.Stage {
	case StagePrescreen:
		request = "capture: " + c.Capture
		ask = func() (helperOutcome, error) {
			verdict, err := stage.model.Prescreen(ctx, c.Capture)
			return helperOutcome{verdict: verdict.Outcome, reason: verdict.Reason}, err
		}
	case StageTags:
		request = "fact: " + c.Fact
		ask = func() (helperOutcome, error) {
			tags, err := stage.model.Tags(ctx, c.Fact)
			return helperOutcome{tags: tags}, err
		}
	default:
		t.Fatalf("case %s: unknown stage %q", c.Name, c.Stage)
	}

	started := time.Now()
	got, err := ask()
	line := reportLine{
		Case: c.Name, Source: c.Source, Layer: c.Layer, Size: c.Size,
		Calls: 1, LatencyMs: time.Since(started).Milliseconds(), Model: stage.id,
	}
	if err != nil {
		line.Failures = []string{err.Error()}
		line.Transcript = writeTranscript(t, arm.name, c, request+"\n\n=== answer ===\n"+err.Error()+"\n")
		t.Errorf("%s: %v", c.Stage, err)
		return line
	}
	line.Outcome = got.label()
	for _, miss := range compareHelper(c, got) {
		if !slices.Contains(line.Fields, miss.Field) {
			line.Fields = append(line.Fields, miss.Field)
		}
		line.Failures = append(line.Failures, miss.Field+": "+miss.Detail)
	}
	line.OK = len(line.Failures) == 0
	line.Transcript = writeTranscript(t, arm.name, c, request+"\n\n=== answer ===\n"+got.summary())
	if !line.OK {
		t.Errorf("%s scored %v", c.Stage, line.Failures)
	}
	return line
}

// compareHelper is L2's scorer: what the stage answered against the case's static truth. The
// verdict is exact, the words beside it are substring because prose belongs to the model, and
// the tags are read as one feature list a future capture has to be able to reach the fact by.
func compareHelper(c Case, got helperOutcome) []miss {
	want := c.Expect.Helper
	var misses []miss
	if want.Verdict != "" && got.verdict != want.Verdict {
		misses = append(misses, miss{"verdict", fmt.Sprintf("stage answered %q, want %q (%s)",
			got.verdict, want.Verdict, got.reason)})
	}
	if len(want.ReasonHas) > 0 && !containsAny(got.reason, want.ReasonHas) {
		misses = append(misses, miss{"reason", fmt.Sprintf("reason %q names none of %v",
			got.reason, want.ReasonHas)})
	}
	if len(got.tags) == 0 && (len(want.TagsHas) > 0 || len(want.TagsAny) > 0) {
		return append(misses, miss{"tags", "no tags were earned"})
	}
	joined := strings.Join(got.tags, ",")
	if len(got.tags) < want.TagsMin {
		misses = append(misses, miss{"tags_min", fmt.Sprintf("%d tags, want at least %d: %v",
			len(got.tags), want.TagsMin, got.tags)})
	}
	for _, part := range want.TagsHas {
		if !containsFold(joined, part) {
			misses = append(misses, miss{"tags_has", fmt.Sprintf("%q is not among %v", part, got.tags)})
		}
	}
	if len(want.TagsAny) > 0 && !containsAny(joined, want.TagsAny) {
		misses = append(misses, miss{"tags_any", fmt.Sprintf("none of %v is among %v",
			want.TagsAny, got.tags)})
	}
	for _, part := range want.TagsNot {
		if containsFold(joined, part) {
			misses = append(misses, miss{"tags_not", fmt.Sprintf("%q is among %v", part, got.tags)})
		}
	}
	limit := want.TagsMax
	if limit == 0 {
		limit = maxTagsPerFact
	}
	if len(got.tags) > limit {
		misses = append(misses, miss{"tags_max", fmt.Sprintf("%d tags, want at most %d: %v",
			len(got.tags), limit, got.tags)})
	}
	return misses
}

// maxTagsPerFact mirrors the product's own cap (agent.maxTags), so a case that says nothing
// about the count still catches a tagger that ignores it.
const maxTagsPerFact = 10

// helperDouble is the scripted helper stage: it answers whatever the test asks it to, which is
// how the scorer and the case set are pinned without a provider.
type helperDouble struct {
	verdict string
	reason  string
	tags    []string
}

func (d helperDouble) Prescreen(context.Context, string) (agent.Verdict, error) {
	return agent.Verdict{Outcome: d.verdict, Reason: d.reason}, nil
}

func (d helperDouble) Tags(context.Context, string) ([]string, error) { return d.tags, nil }

func (d helperDouble) Chat(context.Context, agent.PlannerInput, []agent.Message, []agent.Tool) (agent.Message, error) {
	return agent.Message{}, errors.New("eval: the planner belongs to L1")
}

func (d helperDouble) Finalize(context.Context, agent.PlannerInput, []agent.Message) (agent.Answer, error) {
	return agent.Answer{}, errors.New("eval: the planner belongs to L1")
}

var _ agent.Model = helperDouble{}

// TestHelperScoresOffline pins the L2 lane with no provider at all, because two things have to
// hold before a hosted run is worth its calls: every case loads and resolves, and every case
// written as a negative really is negative. An always-ok stage satisfies exactly the cases that
// ask for ok, so a case with the wrong polarity fails here rather than turning into a mystery
// in a report.
func TestHelperScoresOffline(t *testing.T) {
	cases := evalCases(t, helperCase)
	if len(cases) == 0 {
		t.Skip("no L2 case matched")
	}
	stages := map[string]bool{}
	always := helperOutcome{verdict: "ok", reason: "ok", tags: []string{"grocery", "dairy"}}
	for _, c := range cases {
		stages[c.Stage] = true
		t.Run(c.Name, func(t *testing.T) {
			wantPass := c.Stage != StageTags && c.Expect.Helper.Verdict == "ok" && len(c.Expect.Helper.ReasonHas) == 0
			if got := len(compareHelper(c, always)) == 0; got != wantPass {
				t.Fatalf("an always-ok %s pass = %v, want %v", c.Stage, got, wantPass)
			}
		})
	}
	for _, stage := range []string{StagePrescreen, StageTags} {
		if !stages[stage] {
			t.Errorf("no case covers the %s stage", stage)
		}
	}
}
