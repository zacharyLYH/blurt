package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"blurt/internal/store"
)

// The agent's whole vocabulary: two lookups, one pre-check and the call that ends the loop.
// Nothing here writes, and every read is scoped by the Job's own UserId, so the model never
// reaches a row the caller does not own. The answer is not a tool: submit_plan triggers the one
// strict call behind the loop, which is the loop's only exit.
const (
	toolFindReminders = "find_reminders"
	toolFindMemories  = "find_memories"
	toolCheckUpdate   = "check_update"
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
	tool(toolCheckUpdate, "Check one change before you submit it: whether that row can still be rewritten, and whether the new text, zone, day, clock and recurrence pass the server's checks. Name only what the capture moves: an empty field keeps the row's own, the way your answer will.", checkUpdateSchema),
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

	checkUpdateSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "reminder_id": {"type": "integer", "description": "the id of the row being changed, copied from a find_reminders result"},
    "text":       {"type": "string", "description": "the row's event phrase, changed when the capture renames it"},
    "zone":       {"type": "string", "description": "the IANA zone the row rings in"},
    "day":        {"type": "string", "description": "the day after the change, as the user's own word or a bare date YYYY-MM-DD; empty keeps the row's day"},
    "time":       {"type": "string", "description": "notify clock HH:MM 24-hour"},
    "recurrence": {"type": "string", "enum": ["none", "daily", "weekly", "monthly", "yearly"], "description": "none for a one-off"}
  },
  "required": ["reminder_id", "text", "zone", "day", "time", "recurrence"]
}`
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
		for _, reminder := range rows {
			// The ids this run may name later: the whitelist's whole memory is what the
			// model was shown.
			r.found = append(r.found, reminder.ReminderID)
		}
		return counted(result, left), false
	case toolFindMemories:
		rows, err := r.findMemories(ctx, call)
		if err != nil {
			return failed(name, err, left), false
		}
		result := memoryRows(rows)
		r.looked = append(r.looked, result)
		return counted(result, left), false
	case toolCheckUpdate:
		result, err := r.checkUpdate(ctx, call)
		if err != nil {
			return failed(name, err, left), false
		}
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

// named reports whether an id is one this run's own find_reminders printed. A small integer is a
// real row somebody owns, so an id the model never saw is refused rather than trusted.
func (r *jobRun) named(id int64) bool {
	return slices.Contains(r.found, id)
}

// checkUpdate is the change pre-check: whether that row is still the caller's to change, then the
// record the arguments describe run through the linter's own checks, so a place this accepts is a
// place lint accepts. It is advisory, the way recall is: a refusal is a tool result the model
// reads, never a fault, and lint still decides at submit.
func (r *jobRun) checkUpdate(ctx context.Context, call ToolCall) (string, error) {
	var args struct {
		ReminderID int64  `json:"reminder_id"`
		Text       string `json:"text"`
		Zone       string `json:"zone"`
		Day        string `json:"day"`
		Time       string `json:"time"`
		Recurrence string `json:"recurrence"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("arguments are not a change: %w", err)
	}
	if !r.named(args.ReminderID) {
		return fmt.Sprintf("cannot update id=%d: it is not a row this run looked up", args.ReminderID), nil
	}
	reminder, ok, err := r.loop.Recall.FindReminder(ctx, r.job.UserID, args.ReminderID)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("cannot update id=%d: no such reminder", args.ReminderID), nil
	}
	if reminder.Status != store.ReminderResolved {
		return fmt.Sprintf("cannot update id=%d: it is %s", args.ReminderID, reminderState(reminder)), nil
	}
	// The check answers for the row too: a change names only what it changes, so every field it
	// left unsaid is the row's — exactly as the commit will fill it.
	text := strings.TrimSpace(args.Text)
	if text == "" {
		text = reminder.Text
	}
	zone := strings.TrimSpace(args.Zone)
	if zone == "" {
		zone = reminder.Timezone
	}
	day := strings.TrimSpace(args.Day)
	date, clock := rowWhen(reminder)
	if day == "" {
		day = date
	} else if at, ok := resolveDay(day, inZone(r.started, zone)); ok {
		date = at.Format("2006-01-02")
	} else {
		date = ""
	}
	time := strings.TrimSpace(args.Time)
	if time == "" {
		time = clock
	}
	recurrence := strings.TrimSpace(args.Recurrence)
	if recurrence == "" {
		recurrence = rowRecurrence(reminder)
	}
	record := Record{Kind: KindReminder, Text: text, Zone: zone, Day: day, Date: date,
		Time: time, Recurrence: recurrence}
	if faults := lintRecord(record, r.started); len(faults) > 0 {
		return fmt.Sprintf("cannot update id=%d: %s", args.ReminderID, strings.Join(faults, "; ")), nil
	}
	return fmt.Sprintf("can update id=%d to %s on %s %s",
		args.ReminderID, record.Time, record.Date, record.Zone), nil
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
			reminder.ReminderID, reminder.Text, reminderWhen(reminder), reminderState(reminder)))
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
