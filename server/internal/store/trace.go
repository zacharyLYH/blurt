package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// traceLimit caps the debug sidebar, the way openJobsLimit caps the inbox.
const traceLimit = 100

// Trace is one append-only diagnostic row: what one stage did for one Job, with the
// Reminder it was about when the stage was a firing. Nothing the product shows
// depends on it, which is why the table can be dropped whole.
type Trace struct {
	TraceID   int64          `json:"traceId"`
	JobID     int64          `json:"jobId"`
	Stage     string         `json:"stage"`
	Timestamp string         `json:"timestamp"`
	Detail    map[string]any `json:"detail"`
}

// TracedReminder is one sidebar row: the caller's Reminder, and how much history it has.
type TracedReminder struct {
	ReminderID int64  `json:"reminderId"`
	Text       string `json:"text"`
	Timezone   string `json:"timezone"`
	Status     string `json:"status"`
	LastAt     string `json:"lastAt"`
	Traces     int    `json:"traces"`
}

// TraceStore is the debug page's read, and the one store that reads Trace. Every
// answer is scoped by the caller, so a page can only ever show its own Rows.
type TraceStore struct {
	read *sql.DB
}

func NewTraceStore(read *sql.DB) *TraceStore {
	return &TraceStore{read: read}
}

// Traces is one Reminder's whole history, oldest first, with the reminder itself.
//
// A reminder's history is every Trace that names it plus every Trace of the Job that
// created it: a firing carries ReminderId, and the agent row that planned the row
// carries only the Job. One join, one read, so the page shows the capture and the
// last firing in the same list.
func (s *TraceStore) Traces(ctx context.Context, userID, reminderID int64) (TracedReminder, []Trace, error) {
	var reminder TracedReminder
	err := s.read.QueryRowContext(ctx, `
		SELECT ReminderId, Text, Timezone, Status FROM Reminder
		WHERE ReminderId = ? AND UserId = ?`, reminderID, userID).
		Scan(&reminder.ReminderID, &reminder.Text, &reminder.Timezone, &reminder.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return TracedReminder{}, nil, ErrNotFound
	}
	if err != nil {
		return TracedReminder{}, nil, fmt.Errorf("load traced reminder: %w", err)
	}

	rows, err := s.read.QueryContext(ctx, `
		SELECT t.TraceId, t.JobId, t.Stage, t.Timestamp, t.Detail
		FROM Trace t JOIN Reminder r ON r.ReminderId = t.ReminderId OR t.JobId = r.SourceJobId
		WHERE r.ReminderId = ? AND r.UserId = ?
		ORDER BY t.Timestamp, t.TraceId`, reminderID, userID)
	if err != nil {
		return TracedReminder{}, nil, fmt.Errorf("read traces: %w", err)
	}
	defer rows.Close()

	traces := []Trace{}
	for rows.Next() {
		var trace Trace
		var detail string
		if err := rows.Scan(&trace.TraceID, &trace.JobID, &trace.Stage, &trace.Timestamp, &detail); err != nil {
			return TracedReminder{}, nil, fmt.Errorf("scan trace: %w", err)
		}
		trace.Detail = decodeDetail(detail)
		traces = append(traces, trace)
	}
	if err := rows.Err(); err != nil {
		return TracedReminder{}, nil, fmt.Errorf("read traces: %w", err)
	}
	reminder.Traces = len(traces)
	if len(traces) > 0 {
		reminder.LastAt = traces[len(traces)-1].Timestamp
	}
	return reminder, traces, nil
}

// Traced is the sidebar: the caller's Reminders that have any history at all, the
// most recently active first.
func (s *TraceStore) Traced(ctx context.Context, userID int64) ([]TracedReminder, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT r.ReminderId, r.Text, r.Timezone, r.Status, COUNT(t.TraceId), MAX(t.Timestamp)
		FROM Reminder r JOIN Trace t ON t.ReminderId = r.ReminderId OR t.JobId = r.SourceJobId
		WHERE r.UserId = ?
		GROUP BY r.ReminderId
		ORDER BY MAX(t.Timestamp) DESC LIMIT ?`, userID, traceLimit)
	if err != nil {
		return nil, fmt.Errorf("list traced reminders: %w", err)
	}
	defer rows.Close()

	traced := []TracedReminder{}
	for rows.Next() {
		var row TracedReminder
		if err := rows.Scan(&row.ReminderID, &row.Text, &row.Timezone, &row.Status,
			&row.Traces, &row.LastAt); err != nil {
			return nil, fmt.Errorf("scan traced reminder: %w", err)
		}
		traced = append(traced, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list traced reminders: %w", err)
	}
	return traced, nil
}

// decodeDetail is one Trace's payload. A row the debug page cannot read is shown as
// an empty object rather than as a missing stage: the stage and the time still say
// what happened.
func decodeDetail(raw string) map[string]any {
	detail := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		return map[string]any{}
	}
	return detail
}
