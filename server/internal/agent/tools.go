package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"blurt/internal/store"
)

// The agent's whole vocabulary: two lookups and the call that ends the loop. Nothing here
// writes, and every lookup is scoped by the Job's own UserId, so the model never sees an id
// it can pass and never supplies one. The answer is not a tool: submit_plan triggers the one
// strict call behind the loop, which is the loop's only exit.
const (
	toolFindReminders = "find_reminders"
	toolFindMemories  = "find_memories"
	toolSubmitPlan    = "submit_plan"
)

// toolRows caps one lookup. Five rows is a page the model can read; the id a capture is
// about is in them.
const toolRows = 5

// agentTools is every tool a turn offers, in the OpenAI shape. The list is one value
// because every turn sends the same one.
var agentTools = []Tool{
	tool(toolFindReminders, "Find the user's own reminders by words in their text. Use it when the capture refers to something already scheduled, or when a repeat or a correction needs the row it is about. An empty result is an answer.", findRemindersSchema),
	tool(toolFindMemories, "Find the user's own remembered facts by words. Use it when the capture leans on something the user told Blurt earlier.", findMemoriesSchema),
	tool(toolSubmitPlan, "End the loop and write the final answer. Call it once the capture has everything it needs.", noArgumentsSchema),
}

// Each tool's arguments, as the schema the model writes them against.
const (
	findRemindersSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "query": {"type": "string", "description": "words to find the reminder by, e.g. its event or the place it happens"}
  },
  "required": ["query"]
}`

	findMemoriesSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "query": {"type": "string", "description": "words to find the fact by"}
  },
  "required": ["query"]
}`

	noArgumentsSchema = `{"type": "object", "additionalProperties": false, "properties": {}}`
)

// runTool answers one tool call: the rows a lookup found, or the end of the loop. A lookup
// that fails is answered rather than failed as a turn, the way recall is advisory: a capture
// that cannot be looked up can still be placed, and the model is the one that decides whether
// it can.
func (r *jobRun) runTool(ctx context.Context, call ToolCall, left int) (string, bool) {
	name := call.Function.Name
	switch name {
	case toolFindReminders:
		rows, err := r.findReminders(ctx, call)
		if err != nil {
			return failed(name, err, left), false
		}
		result := reminderRows(rows)
		r.looked = append(r.looked, result)
		return counted(result, left), false
	case toolFindMemories:
		rows, err := r.findMemories(ctx, call)
		if err != nil {
			return failed(name, err, left), false
		}
		result := memoryRows(rows)
		r.looked = append(r.looked, result)
		return counted(result, left), false
	case toolSubmitPlan:
		return counted("submitted", left), true
	}
	return counted(name+": no such tool", left), false
}

// findReminders is the find_reminders lookup: the user's own rows, scoped here rather than by
// anything the model could name.
func (r *jobRun) findReminders(ctx context.Context, call ToolCall) ([]store.Reminder, error) {
	query, err := queryOf(call)
	if err != nil {
		return nil, err
	}
	return r.loop.Recall.FindReminders(ctx, r.job.UserID, query, toolRows)
}

// findMemories is the find_memories lookup, over the same index the context block reads.
func (r *jobRun) findMemories(ctx context.Context, call ToolCall) ([]store.Memory, error) {
	query, err := queryOf(call)
	if err != nil {
		return nil, err
	}
	return r.loop.Recall.FindMemories(ctx, r.job.UserID, query, toolRows)
}

// queryOf is the search phrase one tool call carries. The arguments are the model's own JSON, so
// a call that does not fit the schema is the model's to correct — answered in the tool result
// rather than failed as a turn.
func queryOf(call ToolCall) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("arguments are not a query: %w", err)
	}
	if query := strings.TrimSpace(args.Query); query != "" {
		return query, nil
	}
	return "", errors.New("a query is required")
}

// counted appends the calls left, which is the cap the prompt states and what tells the
// model to stop looking things up.
func counted(result string, left int) string {
	return fmt.Sprintf("%s\nloop calls left: %d", result, left)
}

// failed is a tool that could not answer. The run goes on: the model is told, and decides what
// to do without it, the way recall is advisory.
func failed(name string, err error, left int) string {
	slog.Warn("agent tool", "tool", name, "err", err)
	return counted(fmt.Sprintf("%s failed (%v), so plan without it", name, err), left)
}

// reminderRows renders each row the way the tool promises: the id it can name beside the
// text, when it rings in its own zone, and its status.
func reminderRows(reminders []store.Reminder) string {
	if len(reminders) == 0 {
		return "no reminders match"
	}
	lines := make([]string, 0, len(reminders))
	for _, reminder := range reminders {
		lines = append(lines, fmt.Sprintf("id=%d %s | %s | %s",
			reminder.ReminderID, reminder.Text, reminderWhen(reminder), reminder.Status))
	}
	return strings.Join(lines, "\n")
}

// memoryRows renders the facts a lookup found: the text and the features it is indexed
// under, exactly as the context block does.
func memoryRows(memories []store.Memory) string {
	if len(memories) == 0 {
		return "no memories match"
	}
	lines := make([]string, 0, len(memories))
	for _, memory := range memories {
		lines = append(lines, "- "+memoryLine(memory))
	}
	return strings.Join(lines, "\n")
}

// memoryLine is one Memory as both the context block and the tool render it.
func memoryLine(memory store.Memory) string {
	if strings.TrimSpace(memory.Tags) == "" {
		return memory.Text
	}
	return memory.Text + " [" + memory.Tags + "]"
}

// toolResult is one tool's answer as the conversation carries it: the result paired with
// the call it answers.
func toolResult(call ToolCall, result string) Message {
	return Message{Role: "tool", ToolCallID: call.ID, Content: result}
}
