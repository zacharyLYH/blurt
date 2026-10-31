// Package l1 is the L1 eval: the agent loop scored against a real provider. One case per file
// under cases/, with the goldens and fixtures beside them. The settings come from .env, and
// -eval is what opts in, so nothing else in `go test ./...` spends a call.
//
//	go test ./internal/agent/L1 -run TestPlannerCases -eval -v
//
// Files, by what a reader is looking for:
//
//	L1_test.go        the flags, the arm, and the three tests
//	cases_test.go     the case model (JSON), loading, seeding, fixtures
//	runner_test.go    one case's world: a fresh database, the Job, the readbacks
//	score_test.go     the scorer: what the run left behind against the case's static truth
//	report_test.go    the scorecard, the transcripts, the printed summary
//	recorder_test.go  what the model was handed and what it answered
//	wire_test.go      the raw provider traffic (-wire)
//	doubles_test.go   the scripted model, the push recorder, the pinned clock
package l1

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"blurt/internal/agent"
	"blurt/internal/store"
)

// The L1 eval's flags. The default `go test ./...` gate touches none of this: the cases
// need a real provider, so they skip without one.
var (
	evalCase  = flag.String("case", "", "run only the case with this name")
	evalLayer = flag.String("layer", "", "run only this layer, e.g. L1")
	evalAudit = flag.Bool("audit", false, "print what each case saw, and write no report")
	evalWrite = flag.Bool("update", false, "write the missing or changed goldens")
	// evalRun is the opt-in. The key in .env supplies the settings; this flag is what keeps
	// `go test ./...` offline, since .env is present on every developer's machine.
	evalRun = flag.Bool("eval", false, "run the cases against the real provider")
	// evalPrompt picks the prompt arm: how much the model is handed versus asked to know.
	evalPrompt = flag.String("prompt", "full", "prompt arm: full (the production prompt) or a variant in cases/_prompts")
	// evalWire records every HTTP request and answer to the provider, which is the only way
	// to see what the model was actually sent and actually returned.
	evalWire = flag.Bool("wire", false, "record the provider's HTTP requests and answers")
	// evalRepeat is how many times each case runs. One run per case is how a suite lies to
	// itself: this case passed and failed on the same prompt, so a rate is the measurement.
	evalRepeat = flag.Int("repeat", 1, "run each case this many times, and report a pass rate")
)

// arm is one side of the prompt A/B: the model, the prompt under test, and the name both
// are reported under, so two arms land in two reports and diff cleanly.
type arm struct {
	model  agent.Model
	name   string
	prompt string
}

// evalArm builds the arm the flags asked for, and puts the wire in front of the provider
// when one was asked for. A cut of the production prompt and a whole prompt from promptDir
// are the same thing here, so a rewrite can be measured without touching the product.
func evalArm(t *testing.T, settings agent.Settings, requests *wire) arm {
	t.Helper()
	name := settings.Model + "-" + *evalPrompt
	prompt := promptArm(t, *evalPrompt)
	if requests != nil {
		server := httptest.NewServer(http.HandlerFunc(requests.handler))
		t.Cleanup(server.Close)
		t.Logf("wire: %s -> %s", server.URL, settings.URL)
		settings.URL = server.URL
	}
	client := agent.NewModelClient(settings)
	client.Prompt = prompt
	return arm{model: client, name: name, prompt: prompt}
}

// promptArm resolves one arm's system prompt: the production prompt, or a named variant
// under cases/_prompts. Production is the arm every case runs by default, so what the eval
// scores is the prompt the product ships; a variant file is for the next experiment.
func promptArm(t *testing.T, name string) string {
	t.Helper()
	if name == "full" {
		return agent.AgentPrompt
	}
	path := filepath.Join(promptDir, name+".txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read prompt arm %s: %v", path, err)
	}
	return strings.TrimSpace(string(raw))
}

const (
	// defaultEvalURL is the chat route the loop's free-form turn and its strict call need:
	// the one provider Blurt supports that is not the home gateway.
	defaultEvalURL = "https://openrouter.ai/api/v1/chat/completions"
	// evalTimeout and evalAttemptTimeout shorten the loop's own walls, so a case that
	// hangs is a failed case rather than a stuck test binary.
	evalTimeout        = 4 * time.Minute
	evalAttemptTimeout = 2 * time.Minute
	// reportDir lives outside the package: cases are inputs, reports are artifacts.
	reportDir = "../../../eval-out"
)

// envFile is the developer's .env, read for the eval's own three settings and nothing else.
// The app itself is configured by the process environment — docker compose passes this file
// in — so this is the one place a test parses it: running L1 by hand should not mean
// re-exporting a key.
const envFile = "../../../../.env"

// envValues is envFile, decoded once.
var envValues = sync.OnceValue(func() map[string]string {
	values := map[string]string{}
	raw, err := os.ReadFile(envFile)
	if err != nil {
		return values
	}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key == "" || strings.HasPrefix(key, "#") {
			continue
		}
		values[key] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return values
})

// envValue is one setting: the process environment wins over .env, so a run can be pointed at
// another model without editing the file. L1 and L2 read the same three-way fallback here.
func envValue(key string) string {
	if fromEnv := os.Getenv(key); fromEnv != "" {
		return fromEnv
	}
	return envValues()[key]
}

// evalSettings is what one hosted run needs: a provider, an endpoint, a key and a model,
// exactly the four the setup page stores on a User row. There is no default model on
// purpose — the id belongs to the deployment, and a wrong one would score every case as
// a model failure.
func evalSettings() (agent.Settings, string) {
	key := envValue("BLURT_EVAL_KEY")
	if key == "" {
		return agent.Settings{}, "set BLURT_EVAL_KEY and BLURT_EVAL_MODEL (in .env or the environment) to run the cases against a real model"
	}
	model := envValue("BLURT_EVAL_MODEL")
	if model == "" {
		return agent.Settings{}, "BLURT_EVAL_KEY is set but BLURT_EVAL_MODEL is not"
	}
	url := envValue("BLURT_EVAL_URL")
	if url == "" {
		url = defaultEvalURL
	}
	return agent.Settings{Provider: agent.ProviderOpenRouter, URL: url, APIKey: key, Model: model}, ""
}

// evalCases is the case set the flags asked for, narrowed to the lane this test runs: L1 and
// L2 share one directory, so each test takes only the cases it can run. A lane with nothing
// for this test is a skip; a -case that matched nothing stays a loud failure, because that is
// a name that does not exist rather than a layer this test does not serve.
func evalCases(t *testing.T, lane func(Case) bool) []Case {
	t.Helper()
	all, err := loadCases(caseDir)
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}
	picked := make([]Case, 0, len(all))
	for _, c := range all {
		if *evalLayer != "" && c.Layer != *evalLayer {
			continue
		}
		if *evalCase != "" && c.Name != *evalCase {
			continue
		}
		if !lane(c) {
			continue
		}
		picked = append(picked, c)
	}
	if len(picked) == 0 && *evalCase != "" {
		t.Fatalf("no case matched (%d loaded)", len(all))
	}
	return picked
}

// loopCase is L1's lane: the agent loop, seeded database and all. helperCase is L2's: the
// two stages that run one call each, with no loop and no database behind them.
func loopCase(c Case) bool   { return c.Stage == "" }
func helperCase(c Case) bool { return c.Stage != "" }

// TestPlannerCases is L1: one case, one fresh database, the real tools, the real lint retry
// and the loop called directly. The settings come from .env (BLURT_EVAL_*), and -eval is
// what opts in, so the default gate never spends a call.
//
//	go test ./internal/agent/L1 -run TestPlannerCases -eval -v -timeout 30m
func TestPlannerCases(t *testing.T) {
	if !*evalRun {
		t.Skip("pass -eval to run the cases against the real provider (settings come from .env)")
	}
	cases := evalCases(t, loopCase)
	if len(cases) == 0 {
		t.Skip("no L1 case matched: the helper stages run as -layer L2")
	}
	settings, skip := evalSettings()
	if skip != "" {
		t.Skip(skip)
	}
	var requests *wire
	if *evalWire {
		requests = &wire{
			target: settings.URL, dir: reportDir,
			run:    runName(settings.Model + "-" + *evalPrompt),
			client: &http.Client{Timeout: 5 * time.Minute},
		}
	}
	a := evalArm(t, settings, requests)
	lines := []reportLine{}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			lines = append(lines, repeat(*evalRepeat, func() reportLine {
				requests.forCase(c.Name)
				return runCase(t, c, a)
			}))
		})
	}
	report := reportPath(a.name)
	if *evalAudit || *evalCase != "" {
		// Nothing worth pointing at: -audit writes no report, and a single-case run must not
		// overwrite the whole layer's scorecard with one line.
		report = ""
	} else {
		writeReport(t, a.name, lines)
	}
	summarize(lines, a.name, report)
}

// TestCaseRunScoresOffline pins the harness itself, with no model at all: the scripted
// double answers one turn, the loop commits it through the real path — seed, resolve,
// insert, Trace — and the scorer reads the real rows back. A harness that has never been
// run is the one thing an eval cannot afford, and this is the only way to run it without
// spending a hosted call.
func TestCaseRunScoresOffline(t *testing.T) {
	c, err := loadCase(caseDir, "one-off-call-mum-tomorrow.json")
	if err != nil {
		t.Fatalf("load case: %v", err)
	}
	// The answer the case asks for, one free-form turn and the strict call behind it.
	line := runCase(t, c, arm{
		model:  &scripted{turns: []agent.Answer{answerWith(oneOff("Call mum", "2026-10-25", "19:00"))}},
		name:   "scripted",
		prompt: agent.AgentPrompt,
	})
	if !line.OK {
		t.Fatalf("the harness failed its own answer: %v", line.Failures)
	}
	if line.Outcome != store.JobComplete || line.Calls != 2 {
		t.Fatalf("outcome %s, calls %d, want a complete Job on one turn plus its finalize",
			line.Outcome, line.Calls)
	}
	// One clock off, and the scorer names the fields that missed and nothing else.
	misses := compare(c, outcome{
		status: store.JobComplete,
		reminders: []store.Reminder{{
			Text: "Call mum", Timezone: "Asia/Singapore",
			LocalDateTime: "2026-10-25T17:00", EmitAtUtc: "2026-10-25T09:00:00Z",
			Status: store.ReminderResolved,
		}},
	})
	fields := []string{}
	for _, miss := range misses {
		fields = append(fields, miss.Field)
	}
	if want := []string{"reminders[0].datetime", "reminders[0].utc"}; !slices.Equal(fields, want) {
		t.Fatalf("fields = %v, want %v", fields, want)
	}
	// The report lands where a later run looks for it, and its name survives a model id
	// with slashes and spaces in it.
	if got := reportName("openai/gpt-6:luna medium"); got != "all-openai-gpt-6-luna-medium.json" {
		t.Fatalf("report name = %q", got)
	}
	writeReport(t, "scripted", []reportLine{line})
	path := filepath.Join(reportDir, reportName("scripted"))
	t.Cleanup(func() {
		os.Remove(path)
		os.RemoveAll(filepath.Dir(line.Transcript))
	})
	// A lint fault and the retry it buys: the words the loop hands back are part of the
	// turn, so the transcript has to carry them or a retried run reads as a mystery.
	retried := &scripted{turns: []agent.Answer{
		answerWith(mutateRecord(func(r *agent.Record) { r.Time = "" })),
		answerWith(oneOff("Call mum", "2026-10-25", "19:00")),
	}}
	line = runCase(t, c, arm{model: retried, name: "scripted", prompt: agent.AgentPrompt})
	if !line.OK || len(line.Faults) != 1 {
		t.Fatalf("the retried case scored %v with %d faults, want a pass on one fault",
			line.OK, len(line.Faults))
	}
	if text, err := os.ReadFile(line.Transcript); err != nil || !strings.Contains(string(text), "server, to the model:") {
		t.Fatalf("the retry's own words are missing from %s: %v", line.Transcript, err)
	}
}

// TestPlannerGoldens pins what the model reads. It needs no model at all: the context block
// is the server's own render, so a retrieval change or a prompt change shows up as a diff
// here instead of as a suspicion in a report. Run with -update to write the goldens.
func TestPlannerGoldens(t *testing.T) {
	cases := evalCases(t, loopCase)
	if len(cases) == 0 {
		t.Skip("no L1 case matched: a helper case pins no context block")
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			got := newCaseRun(t, c, nil, "").block(t)
			path := filepath.Join(goldenDir, c.Name+".txt")
			if *evalWrite {
				if err := os.MkdirAll(goldenDir, 0o750); err != nil {
					t.Fatalf("create golden dir: %v", err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v (run with -update to write it)", path, err)
			}
			if got != string(want) {
				t.Errorf("the context block changed:\n--- want\n%s\n--- got\n%s", want, got)
			}
		})
	}
}
