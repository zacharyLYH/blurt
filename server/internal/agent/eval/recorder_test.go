package eval

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"blurt/internal/agent"
)

// recorder forwards every stage to the real model and keeps the whole turn: the input the
// loop built, the conversation it ended with, and the answer the strict call wrote. A tool
// call and the rows it got back are visible nowhere else, and reading them is the only way
// to tell a bad prompt from a bad plan.
type recorder struct {
	agent.Model
	mu        sync.Mutex
	in        agent.PlannerInput
	sent      []agent.Message
	offered   []agent.Tool
	lastReply agent.Message
	answer    agent.Answer
	replied   bool
}

func (r *recorder) Chat(ctx context.Context, in agent.PlannerInput, sent []agent.Message, tools []agent.Tool) (agent.Message, error) {
	reply, err := r.Model.Chat(ctx, in, sent, tools)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastReply, r.offered = reply, tools
	r.keep(in, sent)
	return reply, err
}

func (r *recorder) Finalize(ctx context.Context, in agent.PlannerInput, sent []agent.Message) (agent.Answer, error) {
	answer, err := r.Model.Finalize(ctx, in, sent)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keep(in, sent)
	if err == nil {
		r.answer, r.replied = answer, true
	}
	return answer, err
}

// keep remembers the longest conversation the loop has handed over: the last one it sent is
// the whole turn, tool calls and tool results included. The caller holds the lock.
func (r *recorder) keep(in agent.PlannerInput, sent []agent.Message) {
	if len(sent) >= len(r.sent) {
		r.in, r.sent = in, append([]agent.Message(nil), sent...)
	}
}

// turn is the conversation the run actually had. A loop that spent its budget never reaches
// the strict call, so its last reply is not in any conversation the loop sent and has to be
// added here — and that is the transcript most worth reading.
func (r *recorder) turn() []agent.Message {
	sent := append([]agent.Message(nil), r.sent...)
	if !r.replied && r.lastReply.Role != "" {
		sent = append(sent, r.lastReply)
	}
	return sent
}

// tools is every tool the model called, in order, named the way the model named it.
func (r *recorder) tools() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := []string{}
	for _, message := range r.turn() {
		for _, call := range message.ToolCalls {
			names = append(names, call.Function.Name)
		}
	}
	return names
}

// transcript renders the whole closed loop: the request the loop built, every tool call with
// the rows it got back, and the answer. It is what a prompt or a tool is judged on, so it is
// written beside the report rather than printed.
func (r *recorder) transcript(prompt string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return renderTranscript(prompt, r.in, r.turn(), r.offered, r.answer, r.replied)
}

// written is the answer's own fields, for the audit read-out: what the linter and the commit
// read, before any of it became rows.
func (r *recorder) written() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.replied {
		return "none: the loop never reached the strict call"
	}
	return answerText(r.answer)
}

// renderTranscript is the whole turn as text: the request exactly as the loop assembled it,
// each tool call with the result it was answered with, and the answer. Nothing is prettified
// — the point is to read what the model read.
func renderTranscript(prompt string, in agent.PlannerInput, sent []agent.Message, tools []agent.Tool, answer agent.Answer, replied bool) string {
	names := map[string]string{}
	for _, message := range sent {
		for _, call := range message.ToolCalls {
			names[call.ID] = call.Function.Name
		}
	}
	var out strings.Builder
	out.WriteString("=== request ===\n\n")
	for _, message := range agent.AgentMessages(prompt, in) {
		fmt.Fprintf(&out, "--- %s ---\n%s\n\n", message.Role, message.Content)
	}
	out.WriteString("--- tools offered ---\n")
	for _, tool := range tools {
		fmt.Fprintf(&out, "%s: %s\n  arguments: %s\n",
			tool.Function.Name, tool.Function.Description, tool.Function.Parameters)
	}
	out.WriteString("\n=== turn ===\n\n")
	for _, message := range sent {
		switch message.Role {
		case "assistant":
			if message.Content != "" {
				fmt.Fprintf(&out, "assistant:\n%s\n\n", message.Content)
			}
		case "user":
			// The loop's own words: a lint fault handed back for the retry. It is the most
			// load-bearing thing in a retried turn, so it is never skipped.
			fmt.Fprintf(&out, "server, to the model:\n%s\n\n", message.Content)
		}
		for _, call := range message.ToolCalls {
			fmt.Fprintf(&out, "assistant calls %s %s\n", call.Function.Name, call.Function.Arguments)
		}
		if message.Role == "tool" {
			fmt.Fprintf(&out, "result of %s:\n%s\n\n", names[message.ToolCallID], message.Content)
		}
	}
	out.WriteString("=== answer ===\n")
	if !replied {
		out.WriteString("none: the loop never reached the strict call\n")
		return out.String()
	}
	out.WriteString(answerText(answer))
	return out.String()
}

// answerText is the answer as the fields it was read into, so a transcript can be checked
// against the rows it produced.
func answerText(answer agent.Answer) string {
	var out strings.Builder
	fmt.Fprintf(&out, "kind: %s\n", answer.Kind)
	if answer.Question != "" {
		fmt.Fprintf(&out, "question: %s\n", answer.Question)
	}
	for _, op := range answer.Ops {
		record := op.Record
		fmt.Fprintf(&out, "op %s %d: %q | zone %s | date %s | day %s | time %s | recurrence %s\n",
			op.Intent, op.ReminderID, record.Text, record.Zone, record.Date, record.Day, record.Time,
			record.Recurrence)
	}
	for _, text := range answer.Memories {
		fmt.Fprintf(&out, "memory: %q\n", text)
	}
	if answer.Evidence != "" {
		fmt.Fprintf(&out, "evidence: %s\n", answer.Evidence)
	}
	return out.String()
}
