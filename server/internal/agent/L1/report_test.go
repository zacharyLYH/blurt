package l1

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// reportLine is one case's result: the spec's columns in one file, so two models compare by
// diff rather than by two logs read side by side.
type reportLine struct {
	Case     string   `json:"case"`
	Source   string   `json:"source"`
	Layer    string   `json:"layer"`
	Size     string   `json:"size"`
	OK       bool     `json:"ok"`
	Outcome  string   `json:"outcome"`
	Fields   []string `json:"fields,omitempty"`
	Failures []string `json:"failures,omitempty"`
	Tools    []string `json:"tools,omitempty"`
	Calls    int      `json:"calls"`
	// Runs and Passes are the rate: a case run once is a bit, a case run n times is a
	// measurement, and only the second one can tell a prompt change from a coin flip.
	Runs      int      `json:"runs"`
	Passes    int      `json:"passes"`
	Faults    []string `json:"faults,omitempty"`
	LatencyMs int64    `json:"latencyMs"`
	Model     string   `json:"model"`
	// Transcript is the case's whole turn on disk: a score says what failed, the transcript
	// says why.
	Transcript string `json:"transcript"`
}

// repeat runs one case n times and folds the runs into one line: a pass rate, plus the
// first run that failed, because that is the one worth reading. A rate is what a prompt
// change can be measured against — this suite has watched one case pass and fail on the
// same prompt, so a single bit says nothing.
func repeat(t *testing.T, c Case, a arm, n int, requests *wire) reportLine {
	t.Helper()
	var total reportLine
	for run := range n {
		requests.forCase(c.Name)
		one := runCase(t, c, a)
		if run == 0 {
			total = one
			total.Fields, total.Failures, total.Faults, total.Tools = nil, nil, nil, nil
			total.Calls, total.LatencyMs = 0, 0
			total.Runs, total.Passes = 0, 0
		}
		total.Runs++
		total.Passes += btoi(one.OK)
		total.Calls = max(total.Calls, one.Calls)
		total.LatencyMs = max(total.LatencyMs, one.LatencyMs)
		if !one.OK && len(total.Failures) == 0 {
			total.Fields, total.Failures, total.Faults, total.Tools = one.Fields, one.Failures, one.Faults, one.Tools
		}
	}
	total.OK = total.Passes == total.Runs
	return total
}

// btoi is one run's verdict as a count.
func btoi(ok bool) int {
	if ok {
		return 1
	}
	return 0
}

// runCase drives one case through the loop and scores what it left behind. The loop is
// called directly: no worker, no queue and no pump, because L1 is the loop's own behaviour
// and nothing else's.
func runCase(t *testing.T, c Case, a arm) reportLine {
	t.Helper()
	rec := &recorder{Model: a.model}
	run := newCaseRun(t, c, rec, a.name)
	started := time.Now()
	run.loop.Run(context.Background(), run.job, 1)
	elapsed := time.Since(started)

	got := run.outcome(t, rec)
	line := reportLine{
		Case: c.Name, Source: c.Source, Layer: c.Layer, Size: c.Size,
		Outcome: got.status, Tools: got.tools,
		// turns already counts every model call, the strict call behind submit_plan included.
		Calls:      traceCount(got.trace, "turns"),
		Faults:     traceStrings(got.trace, "lint"),
		LatencyMs:  elapsed.Milliseconds(),
		Model:      a.name,
		Transcript: writeTranscript(t, a.name, c, rec.transcript(a.prompt)),
	}
	for _, miss := range compare(c, got) {
		if !slices.Contains(line.Fields, miss.Field) {
			line.Fields = append(line.Fields, miss.Field)
		}
		line.Failures = append(line.Failures, miss.Field+": "+miss.Detail)
	}
	line.OK = len(line.Failures) == 0
	line.Runs, line.Passes = 1, btoi(line.OK)

	run.audit(t, rec, got, line)
	for _, failure := range line.Failures {
		t.Errorf("%s", failure)
	}
	return line
}

// audit is the per-case read-out: what the seed left, what the model read, what it called,
// what it answered and what the server made of it. It is the lane for iterating a prompt.
func (r *caseRun) audit(t *testing.T, rec *recorder, got outcome, line reportLine) {
	if !*evalAudit {
		return
	}
	t.Logf("source:   %s (%s, %s)", r.c.Source, r.c.Layer, r.c.Size)
	t.Logf("clock:    %s %s, seed %d reminders %d memories", r.c.NowLocal, r.c.CurrentTZ,
		len(r.c.Seed.Reminders), len(r.c.Seed.Memories))
	t.Logf("capture:  %s", r.c.Capture)
	t.Logf("context:\n%s", r.block(t))
	t.Logf("tools:    %v", got.tools)
	t.Logf("calls:    %d, faults %d", line.Calls, len(line.Faults))
	for _, fault := range traceStrings(got.trace, "lint") {
		t.Logf("lint:     %s", fault)
	}
	t.Logf("answer:\n%s", rec.written())
	t.Logf("outcome:  %s -> %s", line.Outcome, got.reply)
	if summary := reminderSummary(got.reminders); summary != "" {
		t.Logf("rows:     %s", summary)
	}
	for _, memory := range got.memories {
		t.Logf("memory:   %s", memory)
	}
	for _, failure := range line.Failures {
		t.Logf("MISS:     %s", failure)
	}
	t.Logf("transcript: %s", line.Transcript)
	t.Logf("ok:       %v (%d/%d)", line.OK, line.Passes, line.Runs)
}

// writeReport lands one run's lines: the scorecard a later run is read against.
func writeReport(t *testing.T, model string, lines []reportLine) {
	t.Helper()
	if err := os.MkdirAll(reportDir, 0o750); err != nil {
		t.Fatalf("create report dir: %v", err)
	}
	raw, err := json.MarshalIndent(lines, "", "  ")
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	if err := os.WriteFile(reportPath(model), append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
}

// summarize prints the run's scorecard: `go test` answers only "ok", and the scorecard itself
// lives in a file. One row per case, the totals, then where the report and the transcripts
// landed, so a run is readable without opening anything. report is empty when none was written.
func summarize(lines []reportLine, armName, report string) {
	passed, runs, calls, faults, slowest := 0, 0, 0, 0, int64(0)
	for _, line := range lines {
		passed, runs, calls = passed+line.Passes, runs+line.Runs, calls+line.Calls
		faults += len(line.Faults)
		slowest = max(slowest, line.LatencyMs)
	}
	fmt.Printf("\nL1: %d/%d passed · arm %s · %d model calls · slowest %s · %d lint faults\n",
		passed, runs, armName, calls, time.Duration(slowest)*time.Millisecond, faults)
	fmt.Printf("%-42s %6s %6s %7s %8s\n", "case", "pass", "calls", "faults", "ms")
	for _, line := range lines {
		fmt.Printf("%-42s %6s %6d %7d %8d\n", line.Case,
			fmt.Sprintf("%d/%d", line.Passes, line.Runs), line.Calls, len(line.Faults), line.LatencyMs)
	}
	fmt.Printf("%-42s %6s %6d %7d %8d\n", "total",
		fmt.Sprintf("%d/%d", passed, runs), calls, faults, slowest)
	for _, line := range lines {
		if !line.OK {
			fmt.Printf("FAIL %s: %s\n", line.Case, strings.Join(line.Failures, "; "))
		}
	}
	if report != "" {
		fmt.Printf("report: %s\ntranscripts: %s\n", report, filepath.Join(reportDir, runName(armName)))
	}
}

// writeTranscript lands one case's turn beside the report, so what the model read and called
// outlives the terminal.
func writeTranscript(t *testing.T, model string, c Case, text string) string {
	t.Helper()
	dir := filepath.Join(reportDir, runName(model))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create transcript dir: %v", err)
	}
	path := filepath.Join(dir, c.Name+".txt")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

// runName is stable per layer and model: the report and the transcripts of one pair land
// together, so re-running it is one diff.
func runName(model string) string {
	layer := *evalLayer
	if layer == "" {
		layer = "all"
	}
	return layer + "-" + modelSlug(model)
}

// reportName is the run's scorecard.
func reportName(model string) string { return runName(model) + ".json" }

// reportPath is where one run's scorecard lands.
func reportPath(model string) string { return filepath.Join(reportDir, reportName(model)) }

// modelSlugRe folds every run of characters a file name cannot take into one dash.
var modelSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// modelSlug turns a model id into a file name: "openai/gpt-6:luna" is "openai-gpt-6-luna".
func modelSlug(model string) string {
	return strings.Trim(modelSlugRe.ReplaceAllString(strings.ToLower(model), "-"), "-")
}
