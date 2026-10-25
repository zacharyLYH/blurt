package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"blurt/internal/decision"
	"blurt/internal/store"
)

// stubDecider answers the fallback's one choice question with whatever the test scripted, so the fallback
// reader's policy is tested without a gateway.
type stubDecider struct {
	choice        string
	probabilities map[string]float64
	err           error
	calls         int
	state         string
}

func (s *stubDecider) Decide(_ context.Context, state string, _ decision.Questions) (decision.Decisions, error) {
	s.calls++
	s.state = state
	if s.err != nil {
		return nil, s.err
	}
	return decision.Decisions{fallbackQuestion: decision.Answer{
		Kind:          decision.Choice,
		Choice:        s.choice,
		Probabilities: s.probabilities,
	}}, nil
}

// A reading is acted on only when it is decisive: a classifier splitting its probability between the
// criteria is abstaining, and an abstention leaves the lint's own refusal standing.
func TestLayaActsOnlyOnADecisiveAnswer(t *testing.T) {
	cases := []struct {
		name          string
		choice        string
		probabilities map[string]float64
		want          Verdict
	}{
		{"a confident contradiction", "contradicted",
			map[string]float64{"contradicted": 0.92, "supported": 0.05, "unknown": 0.03}, VerdictContradicted},
		{"a confident support", "supported",
			map[string]float64{"supported": 0.86, "unknown": 0.09, "contradicted": 0.05}, VerdictSupported},
		{"under the floor", "contradicted",
			map[string]float64{"contradicted": 0.62, "supported": 0.38}, VerdictUnknown},
		{"split", "contradicted",
			map[string]float64{"contradicted": 0.52, "supported": 0.47}, VerdictUnknown},
		{"flat", "supported",
			map[string]float64{"supported": 0.34, "contradicted": 0.33, "unknown": 0.33}, VerdictUnknown},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			stub := &stubDecider{choice: test.choice, probabilities: test.probabilities}
			laya := Laya{Decider: stub}
			reading, err := laya.Resolve(context.Background(), "buy glue", Write{Text: "Call mum"})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if reading.Verdict != test.want {
				t.Fatalf("verdict = %q, want %q", reading.Verdict, test.want)
			}
			// The state is the user's words and the record, never the model's own explanation of them.
			if !strings.Contains(stub.state, "the user said: buy glue") ||
				!strings.Contains(stub.state, "the record: Call mum") {
				t.Fatalf("state = %q", stub.state)
			}
		})
	}
}

// The reader is asked only where a lint abstained: a record built from the user's own words costs no
// call at all, which is what keeps the fallback off the happy path.
func TestWorkerAsksTheFallbackOnlyWhereTheLintAbstains(t *testing.T) {
	f := newFixture(t, Turn{Say: "Reminder set.", Writes: []Write{reminderWrite("Buy glue", "tomorrow at 6pm")}})
	laya := &stubDecider{choice: string(VerdictSupported), probabilities: map[string]float64{
		string(VerdictSupported): 0.9, string(VerdictUnknown): 0.06, string(VerdictContradicted): 0.04,
	}}
	f.worker.Loop.Fallback = Laya{Decider: laya}
	job := f.textJob(t, "buy glue at 6pm tomorrow")

	f.runOnce(t)

	if laya.calls != 0 {
		t.Fatalf("fallback calls = %d, want none: the lint proved the event", laya.calls)
	}
	if stored := f.load(t, job.JobID); stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
}

// A paraphrase is what the reader is for: the lint abstains, the reader says the record is what the user
// asked for, and the record commits.
func TestWorkerCommitsAParaphraseTheFallbackSupports(t *testing.T) {
	f := newFixture(t, Turn{Say: "Reminder set.", Writes: []Write{reminderWrite("Call mother", "tomorrow at 6pm")}})
	laya := &stubDecider{choice: string(VerdictSupported), probabilities: map[string]float64{
		string(VerdictSupported): 0.88, string(VerdictUnknown): 0.08, string(VerdictContradicted): 0.04,
	}}
	f.worker.Loop.Fallback = Laya{Decider: laya}
	job := f.textJob(t, "remind me to call mum at 6pm tomorrow")

	f.runOnce(t)

	if laya.calls != 1 {
		t.Fatalf("fallback calls = %d, want 1", laya.calls)
	}
	if stored := f.load(t, job.JobID); stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	if f.count(t, `SELECT count(*) FROM Reminder`) != 1 {
		t.Fatal("the paraphrase did not commit")
	}
	// What the reader said, and how sure it was, is in the Trace: without it a classifier that keeps
	// abstaining stays anecdotal.
	if read, ok := f.agentTrace(t, job.JobID)["fallback"].([]any); !ok || len(read) != 1 ||
		read[0] != "supported:0.88" {
		t.Fatalf("trace fallback = %v", f.agentTrace(t, job.JobID)["fallback"])
	}
}

// The phrase is provenance-checked too: a written time the user never said is
// refused, and the fallback can still let a paraphrase through. The reader sees
// both the text and the phrase.
func TestWorkerCommitsATimePhraseTheFallbackSupports(t *testing.T) {
	f := newFixture(t, Turn{Say: "Reminder set.",
		Writes: []Write{reminderWrite("Buy glue", "tomorrow at 18:00")}})
	laya := &stubDecider{choice: string(VerdictSupported), probabilities: map[string]float64{
		string(VerdictSupported): 0.9, string(VerdictUnknown): 0.06, string(VerdictContradicted): 0.04,
	}}
	f.worker.Loop.Fallback = Laya{Decider: laya}
	job := f.textJob(t, "buy glue at 6pm tomorrow")

	f.runOnce(t)

	if laya.calls != 1 {
		t.Fatalf("fallback calls = %d, want 1: the phrase's words are not the capture's", laya.calls)
	}
	if stored := f.load(t, job.JobID); stored.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete", stored.Status)
	}
	if !strings.Contains(laya.state, "the record: Buy glue (tomorrow at 18:00)") {
		t.Fatalf("state = %q, want the phrase in the record", laya.state)
	}
	var local string
	if err := f.handle.Read.QueryRow(`SELECT LocalDateTime FROM Reminder`).Scan(&local); err != nil {
		t.Fatalf("read reminder: %v", err)
	}
	if local != "2026-10-25T18:00" {
		t.Fatalf("local date time = %q, want the supported paraphrase", local)
	}
}

// Without a reader, or with one that is down, the phrase lint's own refusal stands.
func TestWorkerRefusesAnUnprovenTimePhrase(t *testing.T) {
	cases := []struct {
		name  string
		laya  decision.Decider
		calls int
	}{
		{"without a fallback", nil, 0},
		{"when the fallback fails", &stubDecider{err: errors.New("decision: call laya: connection refused")}, 1},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, Turn{Say: "Reminder set.",
				Writes: []Write{reminderWrite("Buy glue", "tomorrow at 18:00")}})
			if test.laya != nil {
				f.worker.Loop.Fallback = Laya{Decider: test.laya}
			}
			job := f.textJob(t, "buy glue at 6pm tomorrow")

			f.runOnce(t)

			if stored := f.load(t, job.JobID); stored.Status != store.JobClarifying {
				t.Fatalf("status = %q, want clarifying", stored.Status)
			}
			if stub, ok := test.laya.(*stubDecider); ok && stub.calls != test.calls {
				t.Fatalf("fallback calls = %d, want %d", stub.calls, test.calls)
			}
			if codes := f.refusalCodes(t, job.JobID); len(codes) != 1 || codes[0] != CheckTimeUnsupported {
				t.Fatalf("refused = %v, want %s", codes, CheckTimeUnsupported)
			}
		})
	}
}

// A gateway that is down abstains, so nothing is ever committed on a failing reader's nod.
func TestLayaAbstainsWhenTheGatewayFails(t *testing.T) {
	laya := Laya{Decider: &stubDecider{err: errors.New("decision: call laya: connection refused")}}
	reading, err := laya.Resolve(context.Background(), "buy glue", Write{Text: "Call mum"})
	if err == nil {
		t.Fatal("Resolve with a failing gateway: want an error")
	}
	if reading.Verdict != VerdictUnknown {
		t.Fatalf("verdict = %q, want %q", reading.Verdict, VerdictUnknown)
	}
}

// A contradiction about either half is the one refusal no further model turn can
// settle, so the Job asks at once rather than spending the budget on it.
func TestWorkerAsksAtOnceWhenTheFallbackContradicts(t *testing.T) {
	cases := []struct {
		name  string
		event string
		time  string
	}{
		{"the time phrase", "Buy glue", "tomorrow at 18:00"},
		{"the event", "Call mother", "tomorrow at 6pm"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, Turn{Say: "Reminder set.",
				Writes: []Write{reminderWrite(test.event, test.time)}})
			laya := &stubDecider{choice: string(VerdictContradicted), probabilities: map[string]float64{
				string(VerdictContradicted): 0.91, string(VerdictSupported): 0.05, string(VerdictUnknown): 0.04,
			}}
			f.worker.Loop.Fallback = Laya{Decider: laya}
			job := f.textJob(t, "buy glue at 6pm tomorrow")

			f.runOnce(t)

			stored := f.load(t, job.JobID)
			if stored.Status != store.JobClarifying {
				t.Fatalf("status = %q, want clarifying", stored.Status)
			}
			if len(f.model.inputs) != 1 {
				t.Fatalf("model calls = %d, want 1: the reader's verdict is not the model's to revise",
					len(f.model.inputs))
			}
			if f.count(t, `SELECT count(*) FROM Reminder`) != 0 {
				t.Fatal("a contradicted record committed a Reminder")
			}
			if codes := f.refusalCodes(t, job.JobID); len(codes) != 1 || codes[0] != CheckContradicted {
				t.Fatalf("refused = %v, want %s", codes, CheckContradicted)
			}
			if last := stored.Conversation[len(stored.Conversation)-1]; last.Text != defaultQuestion {
				t.Fatalf("question = %q, want %q", last.Text, defaultQuestion)
			}
		})
	}
}
