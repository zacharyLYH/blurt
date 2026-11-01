package flow

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"blurt/internal/agent"
	"blurt/internal/store"
)

// The split deployment's model ids. The planner is the deployment's model; the screen
// and the tagger are the home gateway's two cheap models.
const (
	screenModel  = "screen-model"
	tagModel     = "tag-model"
	plannerModel = "owner/planner-model"
)

// stageCall is one request as it reached the endpoint: which model id it named and
// which wire shape carried it.
type stageCall struct {
	Model    string
	Envelope string
}

// stageScript serves both shapes on one endpoint, records the model each request named,
// and answers each schema the way a working stage would. It is what makes a mis-wired
// stage fail here: a stage sent to the wrong client, model or envelope does not get an
// answer it can use, and the assertion reads the model id back out of the request.
type stageScript struct {
	mu    sync.Mutex
	calls map[string][]stageCall
}

func newStageScript(t *testing.T) (*httptest.Server, *stageScript) {
	t.Helper()
	script := &stageScript{calls: map[string][]stageCall{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		// The two envelopes put the schema and the tools in different places: the
		// gateway nests them under payload, an OpenAI-compatible route keeps them at
		// the top.
		type format struct {
			JSONSchema struct {
				Name string `json:"name"`
			} `json:"json_schema"`
		}
		var body struct {
			Model          string           `json:"model"`
			Tools          []map[string]any `json:"tools"`
			ResponseFormat format           `json:"response_format"`
			Payload        struct {
				Messages       []wireMessage    `json:"messages"`
				Tools          []map[string]any `json:"tools"`
				ResponseFormat format           `json:"response_format"`
			} `json:"payload"`
		}
		_ = json.Unmarshal(raw, &body)

		name, envelope, tools := body.ResponseFormat.JSONSchema.Name, "openai", len(body.Tools)
		if len(body.Payload.Messages) > 0 {
			name, envelope = body.Payload.ResponseFormat.JSONSchema.Name, "gateway"
			tools = len(body.Payload.Tools)
		}
		// The agent turn names no schema and offers tools: it answers with a call.
		content := `{"verdict":"ok","reason":"","question":""}`
		switch {
		case tools > 0:
			name, content = "agent", `{"tool":"submit_plan"}`
		case name == "planner":
			content = `{"kind":"records","text":"","memories":["milk is cheaper at store a"],"zone":"","day":"","time":"","recurrence":"","evidence":"","question":""}`
		case name == "tags":
			content = `{"tags":"grocery,dairy"}`
		}

		script.mu.Lock()
		script.calls[name] = append(script.calls[name], stageCall{Model: body.Model, Envelope: envelope})
		script.mu.Unlock()

		message := map[string]any{"finish_reason": "stop", "message": map[string]any{"content": content}}
		if tools > 0 {
			message = map[string]any{"finish_reason": "tool_calls", "message": map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{
					"id": "call-1", "type": "function",
					"function": map[string]any{"name": "submit_plan", "arguments": "{}"},
				}}}}
		}
		reply := map[string]any{"choices": []any{message}}
		if envelope == "gateway" {
			reply = map[string]any{"output": map[string]any{"choices": []any{message}}}
		}
		out, _ := json.Marshal(reply)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(server.Close)
	return server, script
}

// wantStage is the pin: exactly one call, naming this model, over this envelope.
func (s *stageScript) wantStage(t *testing.T, stage string, want stageCall) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	calls := s.calls[stage]
	if len(calls) != 1 {
		t.Fatalf("%s calls = %d, want exactly one", stage, len(calls))
	}
	if calls[0] != want {
		t.Fatalf("%s called %q over %s, want %q over %s",
			stage, calls[0].Model, calls[0].Envelope, want.Model, want.Envelope)
	}
}

// wantStages is wantWire's sibling for a split deployment: it asserts each stage's model
// and envelope, and satisfies the harness's declare-your-calls rule at the same layer.
func (p *pump) wantStages(t *testing.T, script *stageScript, want map[string]stageCall) {
	t.Helper()
	p.declared = true
	for stage, call := range want {
		script.wantStage(t, stage, call)
	}
}

// TestPipelineThreeStagesAskTheirOwnModel is the split-model pin. The planner rides the
// deployment's endpoint; the screen and the tagger ride the home gateway, each under its own
// model id. There is no single client to fall back on, which is the production shape — so a
// stage wired to the wrong client, model or wire envelope fails here rather than in a
// deployment where nobody is watching the gateway's logs.
func TestPipelineThreeStagesAskTheirOwnModel(t *testing.T) {
	server, script := newStageScript(t)
	p := newPump(t)
	loop := p.loop
	// No fallback model: every stage must be wired explicitly, as main.go wires it.
	loop.Planner = agent.Stage{Name: plannerModel, Model: agent.NewModelClient(agent.Settings{
		Provider: agent.ProviderOpenRouter, URL: server.URL, APIKey: "owner", Model: plannerModel})}
	gateway := func(name string) agent.Stage {
		return agent.Stage{Name: name, Model: agent.NewModelClient(agent.Settings{
			Provider: agent.ProviderGateway, URL: server.URL, APIKey: "helper", Model: name})}
	}
	loop.Prescreen, loop.Tags = gateway(screenModel), gateway(tagModel)
	p.start()

	job := p.text(t, "remember milk is cheaper at store a", "Asia/Singapore")
	p.settle(t, job.JobID, status(store.JobComplete), "the Memory committed by the planner's model")
	if tags := p.tagged(t, "milk is cheaper at store a"); tags != "grocery,dairy" {
		t.Fatalf("tags = %q, want the gateway tagger's features", tags)
	}

	p.wantStages(t, script, map[string]stageCall{
		"prescreen": {Model: screenModel, Envelope: "gateway"},
		"agent":     {Model: plannerModel, Envelope: "openai"},
		"planner":   {Model: plannerModel, Envelope: "openai"},
		"tags":      {Model: tagModel, Envelope: "gateway"},
	})
	// The planner named the run, and the three gateway models are in the Trace.
	trace := p.lastTrace(t, job.JobID)
	if got := traceField(trace, "model"); got != plannerModel {
		t.Fatalf("trace model = %q, want %q", got, plannerModel)
	}
}
