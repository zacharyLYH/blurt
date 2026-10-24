package httpapi

import (
	"net/http"
	"strings"

	"blurt/internal/store"
)

// maxAnswers keeps a malformed payload from writing unbounded Memory rows.
const maxAnswers = 50

// submitQuestionnaire stores answers as Memory and records the version.
func (s *Server) submitQuestionnaire(w http.ResponseWriter, r *http.Request, user store.User) {
	var body struct {
		Version int            `json:"version"`
		Answers []store.Answer `json:"answers"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Version < 1 {
		writeError(w, http.StatusBadRequest, "invalid_version", "Questionnaire version must be at least 1.")
		return
	}
	if len(body.Answers) == 0 || len(body.Answers) > maxAnswers {
		writeError(w, http.StatusBadRequest, "invalid_answers", "Send between 1 and 50 answers.")
		return
	}
	for i := range body.Answers {
		body.Answers[i].Question = strings.TrimSpace(body.Answers[i].Question)
		body.Answers[i].Answer = strings.TrimSpace(body.Answers[i].Answer)
		if body.Answers[i].Question == "" || body.Answers[i].Answer == "" {
			writeError(w, http.StatusBadRequest, "invalid_answers", "Every answer needs a question and a response.")
			return
		}
	}

	updated, err := s.Users.SaveQuestionnaire(r.Context(), user.UserID, body.Version, body.Answers)
	if err != nil {
		s.fail(w, "save questionnaire", err)
		return
	}
	writeJSON(w, http.StatusOK, userEnvelope{User: updated})
}
