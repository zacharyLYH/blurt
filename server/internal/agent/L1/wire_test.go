package l1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// wire is the provider as seen from outside: the eval points the client at it and it
// forwards the bytes to the real endpoint, keeping the request and the answer. A schema that
// is honoured on some endpoints and treated as a hint on others is a wire question, so the
// wire is what gets read — `go test -eval -wire`.
type wire struct {
	mu      sync.Mutex
	target  string
	dir     string
	run     string
	current string
	client  *http.Client
}

// forCase names the case the next exchange belongs to.
func (w *wire) forCase(name string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.current = name
}

func (w *wire) handler(rw http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}
	forwarded, err := http.NewRequestWithContext(req.Context(), req.Method, w.target, bytes.NewReader(body))
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}
	for name, values := range req.Header {
		for _, value := range values {
			forwarded.Header.Add(name, value)
		}
	}
	// Ask for an uncompressed answer: the recording is meant to be read.
	forwarded.Header.Del("Accept-Encoding")
	answer, err := w.client.Do(forwarded)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}
	defer answer.Body.Close()
	raw, err := io.ReadAll(answer.Body)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}
	w.record(req, body, answer.StatusCode, raw)
	for name, values := range answer.Header {
		for _, value := range values {
			rw.Header().Add(name, value)
		}
	}
	rw.WriteHeader(answer.StatusCode)
	rw.Write(raw)
}

// record appends one exchange to the arm's wire file, so what the model was sent and what it
// answered can be read side by side.
func (w *wire) record(req *http.Request, body []byte, status int, answer []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	dir := filepath.Join(w.dir, w.run)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return
	}
	file, err := os.OpenFile(filepath.Join(dir, "wire.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintf(file, "\n=== %s: %s %s -> %d ===\n--- request ---\n%s\n--- answer ---\n%s\n",
		w.current, req.Method, req.URL.Path, status, prettyJSON(body), prettyJSON(answer))
}

// prettyJSON re-indents a body so a recorded call is readable. Anything unreadable is kept
// exactly as it came.
func prettyJSON(raw []byte) string {
	var decoded json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return string(raw)
	}
	pretty, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(pretty)
}
