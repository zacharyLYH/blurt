package l1

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"blurt/internal/agent"
	"blurt/internal/db"
	"blurt/internal/store"
)

// The eval's layout: one case, one file, with shared seeds, rendered context blocks and
// prompt arms beside them. Add is a file, delete is rm, and a seed is shared by naming it.
const (
	caseDir   = "cases"
	goldenDir = caseDir + "/_golden"
	// promptDir holds whole prompt variants, so an experiment is a file rather than a second
	// prompt living in the product. A variant that wins the eval gets promoted into prompt.go.
	promptDir = caseDir + "/_prompts"
	fixtures  = "_fixtures"
)

// Case is one eval case: the database it starts from, the turns the model reads, and the
// records a correct run leaves behind. One case, one file.
type Case struct {
	// Name is the file's own stem: the golden and the report both key on it, and two
	// names for one case is how a golden drifts from what it pins.
	Name   string `json:"name"`
	Source string `json:"source"`
	Layer  string `json:"layer"`
	Size   string `json:"size"`
	// Stage names the L2 helper this case asks, one call: prescreen or tags. Empty is the
	// agent loop, which is L1.
	Stage string `json:"stage,omitempty"`
	// NowLocal and CurrentTZ are the pinned clock: now_local in current_tz. Every date
	// the case asserts is derived from it, so the case means the same thing next year
	// and the expected instant is arithmetic rather than a reading of an answer.
	NowLocal  string `json:"now_local"`
	CurrentTZ string `json:"current_tz"`
	// Use names a fixture whose rows are seeded before the case's own.
	Use     string `json:"use,omitempty"`
	Seed    Seed   `json:"seed"`
	History []Turn `json:"history"`
	Capture string `json:"capture"`
	// Fact is an L2 tags case's whole input: the one remembered fact whose search
	// features are asked for.
	Fact string `json:"fact,omitempty"`
	// Steps is an L3 case's chain: captures and replies in order, each with what the
	// database must hold afterwards. L1 and L2 are one turn, so they use Capture.
	Steps  []Step `json:"steps,omitempty"`
	Expect Expect `json:"expect"`
}

// Step is one turn of an L3 chain: a capture that opens a Job, or a reply to the Job still
// waiting on the user. Exactly one of the three turn fields is set.
type Step struct {
	// Capture opens a new Job with the user's words.
	Capture string `json:"capture,omitempty"`
	// Clip opens a new Job with a recorded clip, which the transcription worker has to turn
	// into a turn before the agent sees it.
	Clip string `json:"clip,omitempty"`
	// Reply answers the newest Job still waiting on the user.
	Reply string `json:"reply,omitempty"`
	// TranscriptHas is what the clip's own turn must say once it is transcribed. The whole
	// sentence belongs to phonon, so a case pins the words that carry the meaning.
	TranscriptHas []string `json:"transcript_has,omitempty"`
	Expect        Expect   `json:"expect"`
}

// Turn is one earlier Conversation turn, which is how a continuation case is written.
type Turn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// The two L2 helpers, one call each. A case names one and carries that stage's input.
const (
	StagePrescreen = "prescreen"
	StageTags      = "tags"
)

// Seed is the rows an earlier capture would have left behind, in the store's own shape.
// A time may be relative to now_local, so a case still means the same thing next year.
type Seed struct {
	Reminders []SeedReminder `json:"reminders"`
	Memories  []SeedMemory   `json:"memories"`
}

// SeedReminder is one existing Reminder. Zone falls back to the case's current_tz, and
// Status to resolved, so a case writes only what it is about.
type SeedReminder struct {
	Text       string `json:"text"`
	Local      string `json:"local"`
	Zone       string `json:"zone,omitempty"`
	Recurrence string `json:"recurrence,omitempty"`
	Status     string `json:"status,omitempty"`
}

// SeedMemory is one fact Blurt already kept. Tags are the search features a real capture
// would have had generated, so a case can be about retrieval through its tags.
type SeedMemory struct {
	Text string `json:"text"`
	Tags string `json:"tags,omitempty"`
}

// Expect is a case's static truth: what a correct run leaves behind. It is written by
// hand from the capture and never regenerated from a run — a run says the model is
// wrong, never what the right answer is.
//
// An empty key is not asserted; a key that is set must match exactly. The derived fields
// are exact because the server computed them from the pinned clock, and only a record's
// text is matched by substring, because prose is the model's and the event is not.
type Expect struct {
	// Kind is records or question: the two exclusive shapes of an answer.
	Kind string `json:"kind"`
	// Reminders and Memories are exact: the count is asserted, not only the contents.
	Reminders []ExpectReminder `json:"reminders"`
	Memories  []ExpectMemory   `json:"memories"`
	// AsksFor is what a question has to be about, by substring: one of these counts, so
	// "when" and "what time" are the same ask.
	AsksFor []string `json:"asks_for,omitempty"`
	// Tools must each have been called. It is the one field about the path rather than
	// the answer, and it is here because a lookup is invisible in the final rows.
	Tools []string `json:"tools,omitempty"`
	// Helper is an L2 case's truth: what the stage under test has to answer.
	Helper *HelperExpect `json:"helper,omitempty"`
}

// HelperExpect is an L2 case's static truth: the verdict a helper must answer, the words its
// reason has to carry, or the features one fact has to earn.
//
// The verdict is the stage's own enum — a screen answers ok, abuse or incoherent. TagsHas is
// what a future capture must be able to reach the fact by; TagsAny is the weaker form, for
// words outside the fact where any sensible one counts as an answer.
type HelperExpect struct {
	Verdict   string   `json:"verdict,omitempty"`
	ReasonHas []string `json:"reason_contains,omitempty"`
	TagsHas   []string `json:"tags_has,omitempty"`
	TagsAny   []string `json:"tags_any,omitempty"`
	TagsNot   []string `json:"tags_not,omitempty"`
	TagsMin   int      `json:"tags_min,omitempty"`
	TagsMax   int      `json:"tags_max,omitempty"`
}

// ExpectReminder is one row the commit must leave. DateTime is the local time as stored
// (2026-10-25T18:00), UTC is the row's instant — a one-off's derived EmitAtUtc, a
// repeat's NextEmitAtUtc — and Rule is read only when it is present.
type ExpectReminder struct {
	TextContains []string    `json:"text_contains"`
	DateTime     string      `json:"datetime,omitempty"`
	UTC          string      `json:"utc,omitempty"`
	Zone         string      `json:"zone,omitempty"`
	Recurrence   string      `json:"recurrence,omitempty"`
	Status       string      `json:"status,omitempty"`
	Rule         *ExpectRule `json:"rule,omitempty"`
}

// ExpectRule is a repeat's stored pattern. A zero field is not asserted.
type ExpectRule struct {
	Frequency string `json:"frequency"`
	Time      string `json:"time"`
	Weekday   int    `json:"weekday,omitempty"`
	Day       int    `json:"day,omitempty"`
	Month     int    `json:"month,omitempty"`
}

// ExpectMemory is one fact the commit must leave, matched by substring. TagsMin is the
// L3-only count: the tagger's own answer is not deterministic, but a fact that earned
// fewer than a handful of features is a fact a later capture cannot reach.
type ExpectMemory struct {
	Contains []string `json:"contains"`
	TagsMin  int      `json:"tags_min,omitempty"`
}

// kindFailed is a chain step's third outcome: the prescreen closed the Job.
const kindFailed = "failed"

// loadCases reads every case file under dir, in name order so a report is stable.
func loadCases(dir string) ([]Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read cases: %w", err)
	}
	cases := []Case{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		c, err := loadCase(dir, entry.Name())
		if err != nil {
			return nil, err
		}
		cases = append(cases, c)
	}
	return cases, nil
}

// loadCase reads one case file, checks it against its own name, and pulls its fixture in.
func loadCase(dir, file string) (Case, error) {
	raw, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return Case{}, fmt.Errorf("read case %s: %w", file, err)
	}
	var c Case
	if err := json.Unmarshal(raw, &c); err != nil {
		return Case{}, fmt.Errorf("decode case %s: %w", file, err)
	}
	if want := strings.TrimSuffix(file, ".json"); c.Name != want {
		return Case{}, fmt.Errorf("case %s names itself %q", file, c.Name)
	}
	if err := c.check(); err != nil {
		return Case{}, fmt.Errorf("case %s: %w", c.Name, err)
	}
	if _, _, err := c.anchor(); err != nil {
		return Case{}, fmt.Errorf("case %s: %w", c.Name, err)
	}
	if err := c.pullFixture(dir); err != nil {
		return Case{}, fmt.Errorf("case %s: %w", c.Name, err)
	}
	return c, nil
}

// check refuses a case whose layer and inputs disagree, at load rather than at run: an L2
// case names one helper and carries that helper's input, and a typo in the stage would
// otherwise spend a call to answer nothing. An L3 case is its steps and nothing else.
func (c Case) check() error {
	switch c.Layer {
	case "L1", "L2", "L3":
	default:
		return fmt.Errorf("unknown layer %q", c.Layer)
	}
	if c.Layer == "L3" {
		return c.checkChain()
	}
	if len(c.Steps) > 0 {
		return errors.New("steps are an L3 field; name layer L3")
	}
	if c.Stage == "" {
		if c.Expect.Helper != nil {
			return errors.New("helper is an L2 field; name a stage")
		}
		return nil
	}
	if c.Expect.Helper == nil {
		return fmt.Errorf("stage %s needs a helper expectation", c.Stage)
	}
	switch c.Stage {
	case StagePrescreen:
		if c.Capture == "" {
			return errors.New("a prescreen case needs a capture")
		}
	case StageTags:
		if c.Fact == "" {
			return errors.New("a tags case needs a fact")
		}
	default:
		return fmt.Errorf("unknown stage %q", c.Stage)
	}
	return nil
}

// checkChain refuses a chain that cannot be run: an L3 case is a list of steps, one turn
// each, and every step has to say what the database must hold. A reply that opens the case
// would answer nothing, so it fails at load rather than as an empty Conversation.
func (c Case) checkChain() error {
	if c.Stage != "" || c.Capture != "" || c.Fact != "" || len(c.History) > 0 {
		return errors.New("an L3 case is its steps, not a single capture, history or a stage")
	}
	if len(c.Steps) == 0 {
		return errors.New("an L3 case needs steps")
	}
	for i, step := range c.Steps {
		turns := 0
		for _, turn := range []string{step.Capture, step.Clip, step.Reply} {
			if turn != "" {
				turns++
			}
		}
		if turns != 1 {
			return fmt.Errorf("step %d sets %d of capture, clip and reply", i+1, turns)
		}
		if i == 0 && step.Reply != "" {
			return errors.New("step 1 replies, but no Job is waiting on the user yet")
		}
		switch step.Expect.Kind {
		case agent.AnswerRecords, agent.AnswerQuestion, kindFailed:
		default:
			return fmt.Errorf("step %d has kind %q", i+1, step.Expect.Kind)
		}
	}
	return nil
}

// can lean on a shared starting state without repeating it.
func (c *Case) pullFixture(dir string) error {
	if c.Use == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, fixtures, c.Use+".json"))
	if err != nil {
		return fmt.Errorf("read fixture %q: %w", c.Use, err)
	}
	var shared Seed
	if err := json.Unmarshal(raw, &shared); err != nil {
		return fmt.Errorf("decode fixture %q: %w", c.Use, err)
	}
	shared.Reminders = append(shared.Reminders, c.Seed.Reminders...)
	shared.Memories = append(shared.Memories, c.Seed.Memories...)
	c.Seed = shared
	return nil
}

// anchor is a case's pinned clock: now_local in current_tz.
func (c Case) anchor() (time.Time, *time.Location, error) {
	loc, err := time.LoadLocation(c.CurrentTZ)
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("load zone %q: %w", c.CurrentTZ, err)
	}
	at, err := time.ParseInLocation(localLayout, c.NowLocal, loc)
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("parse now_local %q: %w", c.NowLocal, err)
	}
	return at, loc, nil
}

// seedCase inserts one case's rows, so the case's database looks the way an earlier capture
// would have left it. The rows are derived by the same functions the commit path uses
// (agent.ReminderFor, ruleFor), so a seeded repeat is the row a real capture would have
// written, rule and instant included. A seeded row carries no SourceJobId: it belongs to
// an earlier Job, which is what makes it something the model has to look up.
func seedCase(ctx context.Context, handle *db.DB, userID int64, c Case) error {
	now, _, err := c.anchor()
	if err != nil {
		return err
	}
	tx, err := handle.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin seed: %w", err)
	}
	defer tx.Rollback()
	for _, row := range c.Seed.Reminders {
		if err := seedReminder(ctx, tx, userID, c, row, now); err != nil {
			return err
		}
	}
	for _, row := range c.Seed.Memories {
		if err := seedMemory(ctx, tx, userID, row, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit seed: %w", err)
	}
	return nil
}

// seedReminder writes one seeded Reminder through the derivation the commit path uses:
// the local time and the derived instant for a one-off, the rule and the next instant
// for a repeat. The two INSERTs are its own because store's helpers key their dedupe and
// their rule lookup on SourceJobId, and a seeded row has none: going through them would
// write the Reminder and silently leave out its rule.
func seedReminder(ctx context.Context, tx *sql.Tx, userID int64, c Case, row SeedReminder, now time.Time) error {
	zone := row.Zone
	if zone == "" {
		zone = c.CurrentTZ
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return fmt.Errorf("seed reminder %q: load zone %q: %w", row.Text, zone, err)
	}
	at, err := seedLocal(row.Local, now, loc)
	if err != nil {
		return fmt.Errorf("seed reminder %q: %w", row.Text, err)
	}
	frequency := row.Recurrence
	if frequency == "" {
		frequency = agent.RecurrenceNone
	}
	record := agent.Record{
		Kind: agent.KindReminder, Text: row.Text, Zone: zone,
		Date: at.Format("2006-01-02"), Time: at.Format("15:04"), Recurrence: frequency,
	}
	// now must already be in loc, the way resolve hands it over.
	reminder, ok := agent.ReminderFor(record, now.In(loc), loc)
	if !ok {
		return fmt.Errorf("seed reminder %q: %s %s has no next occurrence", row.Text, frequency, record.Time)
	}
	status := row.Status
	if status == "" {
		status = store.ReminderResolved
	}
	var reminderID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO Reminder (UserId, Text, Timezone, LocalDateTime, EmitAtUtc, Status, CreatedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		RETURNING ReminderId`,
		userID, reminder.Text, reminder.Timezone, nullText(reminder.LocalDateTime),
		nullText(reminder.EmitAtUtc), status, now.UTC().Format(time.RFC3339)).Scan(&reminderID)
	if err != nil {
		return fmt.Errorf("insert reminder: %w", err)
	}
	if reminder.Rule == nil {
		return nil
	}
	rule, err := json.Marshal(reminder.Rule)
	if err != nil {
		return fmt.Errorf("encode rule: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO RecurringReminder (ReminderId, Rule, NextEmitAtUtc) VALUES (?, ?, ?)`,
		reminderID, string(rule), reminder.NextEmitAtUtc); err != nil {
		return fmt.Errorf("insert recurring reminder: %w", err)
	}
	return nil
}

// nullText keeps an unset human time out of a nullable column, the way the store does: a
// repeat stores its rule, not a local date time.
func nullText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// seedMemory records one fact the way an earlier capture would have left it. The FTS
// triggers index it, so the lookup and the context block both find it.
func seedMemory(ctx context.Context, tx *sql.Tx, userID int64, row SeedMemory, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO Memory (UserId, Text, Tags, CreatedAt) VALUES (?, ?, ?, ?)`,
		userID, row.Text, row.Tags, now.UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("insert memory: %w", err)
	}
	return nil
}

// seedOffsetRe is a seeded time relative to the anchor: "+1d 17:00" is the day after now
// at 17:00, "-2h" is two hours before it, and no clock keeps the anchor's own clock.
var seedOffsetRe = regexp.MustCompile(`^([+-])([0-9]+)([dh])(?:\s+([0-9]{1,2}:[0-9]{2}))?$`)

// seedLocal resolves one seeded time against the case's pinned clock. An absolute
// "2026-10-25T17:00" is taken as written, so a case that is about a date can name it.
func seedLocal(local string, anchor time.Time, loc *time.Location) (time.Time, error) {
	if local == "" {
		return time.Time{}, errors.New("no local time")
	}
	if at, err := time.ParseInLocation(localLayout, local, loc); err == nil {
		return at, nil
	}
	match := seedOffsetRe.FindStringSubmatch(local)
	if match == nil {
		return time.Time{}, fmt.Errorf("%q is neither an offset (+1d 17:00, -2h) nor a time", local)
	}
	amount, err := strconv.Atoi(match[2])
	if err != nil {
		return time.Time{}, fmt.Errorf("read offset %q: %w", local, err)
	}
	if match[1] == "-" {
		amount = -amount
	}
	at := anchor.In(loc)
	switch match[3] {
	case "d":
		at = at.AddDate(0, 0, amount)
	case "h":
		at = at.Add(time.Duration(amount) * time.Hour)
	}
	if match[4] == "" {
		return at, nil
	}
	hour, minute, ok := clock(match[4])
	if !ok {
		return time.Time{}, fmt.Errorf("read clock %q", match[4])
	}
	return time.Date(at.Year(), at.Month(), at.Day(), hour, minute, 0, 0, loc), nil
}
