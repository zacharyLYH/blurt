package httpapi_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Trace is debug-only and droppable. This scan keeps the contract from drifting: no
// production file may send or read a notification's Trace id, and no page may read a
// Trace Detail. The product's own words come from the Reminder.
func TestProductionCodeDoesNotDependOnTrace(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	targets := []string{
		filepath.Join(root, "server"),
		filepath.Join(root, "web", "src"),
		filepath.Join(root, "web", "public"),
	}
	forbidden := []string{"notificationTraceId", "notificationTraceID", "Trace.Detail"}

	scanned := 0
	for _, target := range targets {
		err := filepath.WalkDir(target, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				switch entry.Name() {
				case "node_modules", "dist", "test-results", "eval", "cases":
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			switch filepath.Ext(path) {
			case ".go", ".ts", ".tsx", ".js":
			default:
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			for _, marker := range forbidden {
				if strings.Contains(string(raw), marker) {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s mentions %q: Trace is debug-only and droppable", rel, marker)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", target, err)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no production files")
	}
}
