package agent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"blurt/internal/store"
)

// questionText is what the user reads, chosen from the answer this run already holds rather
// than asked for in a second call: the model's own question, or the fixed sentence. Either
// way it passes through usableQuestion, which is what keeps a clock the user never named off
// the screen.
func (r *jobRun) questionText() string {
	if askedQuestions(r.job.Conversation) >= maxQuestions {
		// The Job has had its rounds; another ask would only repeat itself.
		return defaultQuestion
	}
	if r.usableQuestion(r.answer.Question) {
		return r.answer.Question
	}
	return defaultQuestion
}

// isQuestion reports whether a sentence is an ask: the model's question and the server's
// canned sentence both end in a question mark, while a commit sentence and a failure
// notice do not.
func isQuestion(text string) bool {
	return strings.HasSuffix(strings.TrimSpace(text), "?")
}

// askedQuestions is how many questions this Job has asked: the assistant turns that are
// asks. It is the Job's own round count, which is what the question cap is measured
// against; a commit sentence or a failure notice is not a round.
func askedQuestions(conversation []store.Message) int {
	questions := 0
	for _, turn := range conversation {
		if turn.Role == "assistant" && isQuestion(turn.Text) {
			questions++
		}
	}
	return questions
}

// usableQuestion reports whether a written question can be put to the user: one short
// sentence, ending in a question, quoting no clock the user did not name, and carrying no
// placeholder Blurt leaked into the prompt. A model handed the capture's own words gets the
// first of those wrong in exactly that way.
func (r *jobRun) usableQuestion(question string) bool {
	return question != "" && len(question) <= 200 && isQuestion(question) &&
		r.quotesOnlyTheUsersClocks(question) &&
		!strings.Contains(question, "<unknown>") && !strings.Contains(question, "<nothing>")
}

// quotesOnlyTheUsersClocks reports whether every clock a question names is one the user's own
// words or this run's lookups named.
//
// A clock the model supplied itself is the failure this guard exists for: the user reads it as
// their own, answers yes, and a time they never chose becomes a row they are reminded at.
// Quoting a clock the user did name is fine — asking which of the times they named comes first
// is a better question than asking them to say it all again — so this is a provenance check.
// Clocks only: a day word carries no equally cheap proof.
func (r *jobRun) quotesOnlyTheUsersClocks(question string) bool {
	named := clocksOf(r.said())
	for clock := range clocksOf(question) {
		if !named[clock] {
			return false
		}
	}
	return true
}

// said is everything the user has put into this Job, and what this run's own lookups
// answered: the two places a question may take a clock from.
func (r *jobRun) said() string {
	if len(r.looked) == 0 {
		return userText(r.job)
	}
	return userText(r.job) + "\n" + strings.Join(r.looked, "\n")
}

// storedClockRe is a clock that may sit against other characters, because the moment a
// looked-up row prints ("2026-10-27T10:00") has no word boundary in front of it. It is the
// guard's reader only: clock24Re stays the boundary-respecting one every other caller wants.
var storedClockRe = regexp.MustCompile(`([0-9]{1,2}):([0-9]{2})`)

// clocksOf is every clock a text names, normalized to HH:MM so that "5pm", "5 pm" and
// "17:00" are one clock to whoever compares the two sides.
func clocksOf(text string) map[string]bool {
	named := map[string]bool{}
	lowered := strings.ToLower(text)
	for _, match := range clockRe.FindAllStringSubmatch(lowered, -1) {
		hour, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		minute := 0
		if match[2] != "" {
			if minute, err = strconv.Atoi(match[2]); err != nil || minute > 59 {
				continue
			}
		}
		switch match[3] {
		case "pm":
			if hour < 12 {
				hour += 12
			}
		case "am":
			if hour == 12 {
				hour = 0
			}
		}
		if hour > 23 {
			continue
		}
		named[fmt.Sprintf("%02d:%02d", hour, minute)] = true
	}
	// The colon form reads only what the am/pm form did not claim, or "1:30pm" would also
	// be read as 01:30. It is boundary-free because a looked-up row prints its moment as a
	// stored timestamp — "2026-10-27T10:00" — and clock24Re cannot see a clock that sits
	// against the date.
	rest := clockRe.ReplaceAllString(lowered, " ")
	for _, match := range storedClockRe.FindAllStringSubmatch(rest, -1) {
		hour, hourErr := strconv.Atoi(match[1])
		minute, minuteErr := strconv.Atoi(match[2])
		if hourErr != nil || minuteErr != nil || hour > 23 || minute > 59 {
			continue
		}
		named[fmt.Sprintf("%02d:%02d", hour, minute)] = true
	}
	if strings.Contains(lowered, "noon") {
		named["12:00"] = true
	}
	if strings.Contains(lowered, "midnight") {
		named["00:00"] = true
	}
	return named
}
