package capture

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const audioDir = "audio"

// namePattern is the only filename shape Open and Remove accept, so a value
// from a Conversation row can never escape the audio directory.
var namePattern = regexp.MustCompile(`^[0-9a-f]{16}\.wav$`)

// Store keeps uploaded blobs beside the SQLite file. They are temporary: the
// worker removes one as soon as the Job stops needing it.
type Store struct {
	dir string
}

func NewStore(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, audioDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create audio dir: %w", err)
	}
	// A crash mid-upload can leave a dotfile behind; it was never referenced by
	// a Job, so boot is the right time to sweep it.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".upload-") {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
	return &Store{dir: dir}, nil
}

// NewName mints a filename before the Job exists, so the upload can be written
// before the row is inserted; a failed insert then just removes the file.
func NewName() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("mint audio name: %w", err)
	}
	return hex.EncodeToString(raw) + ".wav", nil
}

// Save streams to a dotfile and renames it into place, so a half-written blob
// is never visible to the worker.
func (s *Store) Save(name string, audio io.Reader) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid audio name %q", name)
	}
	tmp, err := os.CreateTemp(s.dir, ".upload-*")
	if err != nil {
		return fmt.Errorf("create audio temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, audio); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write audio: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close audio: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.dir, name)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("publish audio: %w", err)
	}
	return nil
}

func (s *Store) Open(name string) (*os.File, error) {
	if !namePattern.MatchString(name) {
		return nil, fmt.Errorf("invalid audio name %q", name)
	}
	return os.Open(filepath.Join(s.dir, name))
}

// Remove is idempotent: the worker may clean up after a restart that already
// deleted the file.
func (s *Store) Remove(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid audio name %q", name)
	}
	if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove audio: %w", err)
	}
	return nil
}
