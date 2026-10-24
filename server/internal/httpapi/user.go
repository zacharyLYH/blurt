package httpapi

import (
	"net/http"

	"blurt/internal/store"
)

// getUser returns the cookie identity plus questionnaire progress, which is
// what the app needs on boot.
func (s *Server) getUser(w http.ResponseWriter, r *http.Request, user store.User) {
	writeJSON(w, http.StatusOK, userEnvelope{User: user})
}
