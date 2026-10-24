// Package httpapi wires Blurt's HTTP surface. Files are split by the job
// they do: auth, user, questionnaire, and this shared skeleton.
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"blurt/internal/auth"
	"blurt/internal/capture"
	"blurt/internal/push"
	"blurt/internal/store"
)

// PINMailer delivers login PINs. An interface so tests can read the code
// without an SMTP server. Nil is valid: dev mode (BLURT_FAKE_PIN) issues a
// fixed PIN and deliberately sends nothing.
type PINMailer interface {
	SendPIN(to, pin string) error
}

// Pusher delivers one notification to every device a user is signed in on. An
// interface so the HTTP layer can be tested without a relay. Nil is valid: the
// send endpoint then reports that sending is not configured.
type Pusher interface {
	NotifyUser(ctx context.Context, userID int64, msg push.Message) (push.Result, error)
}

// Deps is everything the handlers need.
type Deps struct {
	DB       *sql.DB
	Users    *store.UserStore
	Sessions *store.SessionStore
	Pins     *auth.PinStore
	Mailer   PINMailer
	// Pusher sends notifications to the user's devices.
	Pusher Pusher
	// Jobs and Audio make up the capture path. Nil is valid for tests that do
	// not exercise it; the handlers answer 501 instead of panicking.
	Jobs  *store.JobStore
	Audio *capture.Store
	// VAPIDPublicKey is handed to browsers when they create a push
	// subscription. The private half stays inside the sender.
	VAPIDPublicKey string
	// AllowedOrigins guards unsafe requests against CSRF, alongside the
	// SameSite=Lax session cookie.
	AllowedOrigins []string
}

type Server struct {
	Deps
}

// New builds the handler for the whole API.
func New(deps Deps) http.Handler {
	s := &Server{Deps: deps}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /api/auth/request-pin", s.requestPin)
	mux.HandleFunc("POST /api/auth/verify-pin", s.verifyPin)
	mux.HandleFunc("POST /api/auth/logout", s.logout)
	mux.HandleFunc("GET /api/user", s.requireUser(s.getUser))
	mux.HandleFunc("POST /api/questionnaire", s.requireUser(s.submitQuestionnaire))
	mux.HandleFunc("GET /api/push/key", s.pushKey)
	mux.HandleFunc("POST /api/push/subscribe", s.requireUser(s.subscribePush))
	mux.HandleFunc("POST /api/push", s.requireUser(s.sendPush))
	mux.HandleFunc("POST /api/jobs", s.requireUser(s.createJob))
	mux.HandleFunc("GET /api/jobs", s.requireUser(s.listJobs))
	mux.HandleFunc("GET /api/jobs/{jobId}", s.requireUser(s.getJob))
	// Wrong method on a real Job path gets 405, not the generic 501 below.
	mux.HandleFunc("/api/jobs/{jobId}", methodNotAllowed(http.MethodGet))
	// Unbuilt endpoints stay explicit instead of 404 so the frontend can
	// tell "not yet" apart from "wrong URL".
	mux.HandleFunc("/api/", notImplemented)
	return s.csrf(mux)
}

// knownRoutes lets notImplemented answer 405 for a real path used with the
// wrong method, instead of pretending that endpoint is not built.
var knownRoutes = map[string]string{
	"/api/auth/request-pin": http.MethodPost,
	"/api/auth/verify-pin":  http.MethodPost,
	"/api/auth/logout":      http.MethodPost,
	"/api/user":             http.MethodGet,
	"/api/questionnaire":    http.MethodPost,
	"/api/push/key":         http.MethodGet,
	"/api/push/subscribe":   http.MethodPost,
	"/api/push":             http.MethodPost,
	"/api/jobs":             http.MethodGet + ", " + http.MethodPost,
}

// csrf requires an allowed Origin (or Referer) on unsafe methods. Cookies
// alone are never enough: SameSite=Lax plus this check covers CSRF.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		origin := requestOrigin(r)
		for _, allowed := range s.AllowedOrigins {
			if origin != "" && strings.EqualFold(normalizeOrigin(allowed), origin) {
				next.ServeHTTP(w, r)
				return
			}
		}
		writeError(w, http.StatusForbidden, "invalid_origin", "Request origin is not allowed.")
	})
}

// requestOrigin prefers Origin, falling back to Referer for older clients.
func requestOrigin(r *http.Request) string {
	if origin := r.Header.Get("Origin"); origin != "" {
		return normalizeOrigin(origin)
	}
	referer := r.Header.Get("Referer")
	if referer == "" {
		return ""
	}
	parsed, err := url.Parse(referer)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return normalizeOrigin(parsed.Scheme + "://" + parsed.Host)
}

func normalizeOrigin(origin string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(origin), "/"))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	status, code := "ok", http.StatusOK
	if err := s.DB.PingContext(r.Context()); err != nil {
		slog.Error("health: database unreachable", "err", err)
		status, code = "degraded", http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]string{
		"status": status,
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

// maxBodyBytes bounds JSON bodies so a large post cannot exhaust memory.
const maxBodyBytes = 1 << 20

// decodeJSON reads exactly one JSON value, writing a 4xx on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if raw := r.Header.Get("Content-Type"); raw != "" {
		mediaType, _, err := mime.ParseMediaType(raw)
		if err != nil || mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Body must be JSON.")
			return false
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Body must be JSON.")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_body", "Body must be one JSON value.")
		return false
	}
	return true
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "Something broke on our side.")
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	if allowed, ok := knownRoutes[r.URL.Path]; ok {
		w.Header().Set("Allow", allowed)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}
	writeError(w, http.StatusNotImplemented, "not_implemented", "Not implemented yet.")
}

// methodNotAllowed answers a real dynamic path used with the wrong method.
func methodNotAllowed(allowed string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", allowed)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
	}
}

func writeError(w http.ResponseWriter, code int, slug, message string) {
	writeJSON(w, code, map[string]string{"error": message, "code": slug})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("write json", "err", err)
	}
}
