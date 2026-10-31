package l1

import (
	"context"
	"errors"
	"fmt"
	"time"

	"blurt/internal/agent"
	"blurt/internal/push"
)

// The clock every case is pinned to, and the zone its files are written in.
var pinned = time.Date(2026, 10, 24, 18, 30, 0, 0, time.FixedZone("Asia/Singapore", 8*60*60))

const zone = "Asia/Singapore"

// verdictOK is the one screening answer that lets a turn reach the planner.
const verdictOK = "ok"

// localLayout is the case files' own grammar: an absolute now_local, and the clock on a seeded
// row. The rows themselves are stored through agent.ReminderFor, so the product's own format is
// what lands in the database.
const localLayout = "2006-01-02T15:04"

// clock is one case-file clock as hour and minute.
func clock(value string) (int, int, bool) {
	at, err := time.Parse("15:04", value)
	if err != nil {
		return 0, 0, false
	}
	return at.Hour(), at.Minute(), true
}

// scripted is the model that pins the harness itself: it submits at once and answers with the
// turns it was handed, in order. Running out of turns is an error rather than a zero answer, so
// a case that needs a third turn says so.
type scripted struct {
	turns []agent.Answer
	turn  int
}

func (s *scripted) Prescreen(context.Context, string) (agent.Verdict, error) {
	return agent.Verdict{Outcome: verdictOK}, nil
}

// Chat submits the plan: the wire name of the tool that ends the loop, which is the one name
// the harness has to know.
func (s *scripted) Chat(context.Context, agent.PlannerInput, []agent.Message, []agent.Tool) (agent.Message, error) {
	return agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
		ID: "submit", Type: "function",
		Function: agent.CallFunction{Name: "submit_plan", Arguments: "{}"},
	}}}, nil
}

func (s *scripted) Finalize(context.Context, agent.PlannerInput, []agent.Message) (agent.Answer, error) {
	if s.turn >= len(s.turns) {
		return agent.Answer{}, fmt.Errorf("scripted: no turn left after %d", s.turn)
	}
	answer := s.turns[s.turn]
	s.turn++
	return answer, nil
}

func (s *scripted) Tags(context.Context, string) ([]string, error) { return nil, nil }

var _ agent.Model = (*scripted)(nil)

// recordedPush is what the user was told, for the tests that assert the sentence.
type recordedPush struct {
	calls int
	title string
	body  string
}

func (p *recordedPush) NotifyUser(_ context.Context, _ int64, msg push.Message) (push.Result, error) {
	p.calls++
	p.title, p.body = msg.Title, msg.Body
	return push.Result{Sent: 1}, nil
}

// oneOff is a record the server can commit as a single local instant.
func oneOff(text, date, clock string) agent.Record {
	return agent.Record{Kind: agent.KindReminder, Text: text, Zone: zone, Date: date, Time: clock,
		Recurrence: agent.RecurrenceNone}
}

// mutateRecord is one committable one-off with one field changed, so a fault reads as the change
// rather than as a whole record.
func mutateRecord(mutate func(*agent.Record)) agent.Record {
	record := oneOff("Call mum", "2026-10-25", "19:00")
	mutate(&record)
	return record
}

// answerWith is the one record an answer carries.
func answerWith(record agent.Record) agent.Answer {
	return agent.Answer{Kind: agent.AnswerRecords, Records: []agent.Record{record}}
}

// sideStage stands in for the two stages L1 does not score: the screen passes every capture
// and the tagger answers nothing, because a Memory's search features come from the case's own
// seed, so a case can only fail on the agent turn, the strict call behind it and the lint
// retry. The two planner methods are the wiring's business, so calling one is a test bug worth a
// loud error rather than a zero value.
type sideStage struct{}

func (sideStage) Prescreen(context.Context, string) (agent.Verdict, error) {
	return agent.Verdict{Outcome: verdictOK}, nil
}

func (sideStage) Tags(context.Context, string) ([]string, error) { return nil, nil }

func (sideStage) Chat(context.Context, agent.PlannerInput, []agent.Message, []agent.Tool) (agent.Message, error) {
	return agent.Message{}, errors.New("eval: the planner methods belong to the arm's model")
}

func (sideStage) Finalize(context.Context, agent.PlannerInput, []agent.Message) (agent.Answer, error) {
	return agent.Answer{}, errors.New("eval: the planner methods belong to the arm's model")
}
