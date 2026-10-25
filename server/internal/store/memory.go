package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode"
)

// searchLimit caps retrieval. Phase 1 is BM25 only, so recall is best effort and
// the model decides which results matter.
const searchLimit = 10

// Memory is one fact worth keeping for later. It carries no time: a Memory is
// context for a future Reminder, not an interruption of its own. Metadata (trip
// dates and zone) arrives with the trip slice; nothing reads it yet, so it is
// left unmodelled rather than filled with an unused field.
type Memory struct {
	MemoryID    int64  `json:"memoryId"`
	UserID      int64  `json:"-"`
	Text        string `json:"text"`
	Tags        string `json:"tags,omitempty"`
	SourceJobID int64  `json:"sourceJobId,omitempty"`
	CreatedAt   string `json:"createdAt"`
}

// MemoryStore is retrieval only. Writes belong to whichever store owns the
// lifecycle — User for questionnaire answers, Job for the AgentLoop — so this
// one holds no write pool.
type MemoryStore struct {
	read *sql.DB
}

// insertMemory runs inside the Job's own transaction, for the same reason
// insertReminder does, and skips a text this Job already committed.
func insertMemory(ctx context.Context, tx execer, memory Memory) error {
	if memory.CreatedAt == "" {
		memory.CreatedAt = now()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO Memory (UserId, Text, Tags, SourceJobId, CreatedAt)
		SELECT ?, ?, ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM Memory WHERE SourceJobId = ? AND Text = ?)`,
		memory.UserID, memory.Text, memory.Tags, nullID(memory.SourceJobID), memory.CreatedAt,
		nullID(memory.SourceJobID), memory.Text); err != nil {
		return fmt.Errorf("insert memory: %w", err)
	}
	return nil
}

func NewMemoryStore(read *sql.DB) *MemoryStore {
	return &MemoryStore{read: read}
}

// Search is the PRD's BM25 query. Results are ordered by relevance, an empty
// query reads nothing rather than matching everything, and the index is stemmed
// (schema.sql), so an inflected query reaches the row's base word without any
// stemming here.
func (s *MemoryStore) Search(ctx context.Context, userID int64, query string, limit int) ([]Memory, error) {
	match := matchQuery(query)
	if match == "" {
		return nil, nil
	}
	if limit < 1 || limit > searchLimit {
		limit = searchLimit
	}
	// bm25(fts_memory, text_weight, tags_weight): a hit in the generated tags
	// counts three times one in the text, because the tags are the features a
	// Memory was indexed under rather than words that happen to appear.
	rows, err := s.read.QueryContext(ctx, `
		SELECT m.MemoryId, m.UserId, m.Text, m.Tags, m.CreatedAt
		FROM fts_memory JOIN Memory m ON m.MemoryId = fts_memory.rowid
		WHERE fts_memory MATCH ? AND m.UserId = ?
		ORDER BY bm25(fts_memory, 1.0, 3.0) LIMIT ?`, match, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("memory search: %w", err)
	}
	defer rows.Close()

	memories := []Memory{}
	for rows.Next() {
		var memory Memory
		if err := rows.Scan(&memory.MemoryID, &memory.UserID, &memory.Text, &memory.Tags, &memory.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		memories = append(memories, memory)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory search: %w", err)
	}
	return memories, nil
}

// matchQuery turns a capture into an FTS5 MATCH expression. Raw text cannot go
// in as-is: FTS5 parses a grammar of its own, so a stray apostrophe or the word
// AND is a syntax error that would fail the whole search. Every word is quoted
// and OR-ed, which also stops one odd token from hiding the rest — recall is
// what the model needs, and it does the filtering itself.
//
// Quoting does not defeat stemming: a one-token phrase is tokenized like any
// other term, so the table's porter tokenizer stems both sides and "groceries"
// still reaches "grocery". Do not add a stemmer here.
func matchQuery(query string) string {
	words := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	terms := make([]string, 0, len(words))
	for _, word := range words {
		terms = append(terms, `"`+strings.ReplaceAll(word, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " OR ")
}
