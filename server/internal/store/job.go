package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Job statuses from the PRD. Transcription hands a Job back to queued so the
// agent poller can pick up the transcript; only failures park it in failed.
const (
	JobQueued     = "queued"
	JobProcessing = "processing"
	JobClarifying = "clarifying"
	JobComplete   = "complete"
	JobFailed     = "failed"
)

// transcriptionStage is the Trace stage the capture worker owns.
const transcriptionStage = "transcription"

// Message is one Conversation turn. Audio is the temporary blob's filename,
// never a path: the storage directory stays server-side.
type Message struct {
	Role  string `json:"role"`
	Text  string `json:"text,omitempty"`
	Audio string `json:"audio,omitempty"`
	At    string `json:"at"`
}

// Job is one capture conversation. RetryCount is a poison-pill guard, not UI.
type Job struct {
	JobID        int64     `json:"jobId"`
	UserID       int64     `json:"-"`
	Status       string    `json:"status"`
	RetryCount   int       `json:"-"`
	Conversation []Message `json:"conversation"`
	CreatedAt    string    `json:"createdAt"`
}

// PendingAudio returns the first user clip that has not been transcribed.
func (j Job) PendingAudio() (string, bool) {
	for _, message := range j.Conversation {
		if message.Role == "user" && message.Audio != "" && message.Text == "" {
			return message.Audio, true
		}
	}
	return "", false
}

// JobStore owns Job, Conversation, and the transcription Trace rows.
type JobStore struct {
	write *sql.DB
	read  *sql.DB
}

func NewJobStore(write, read *sql.DB) *JobStore {
	return &JobStore{write: write, read: read}
}

// Create inserts a queued Job. Text captures arrive complete; audio captures
// wait for the worker to append their transcript.
func (s *JobStore) Create(ctx context.Context, userID int64, messages ...Message) (Job, error) {
	for i := range messages {
		if messages[i].At == "" {
			messages[i].At = now()
		}
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		return Job{}, fmt.Errorf("encode conversation: %w", err)
	}

	var job Job
	var stored string
	err = s.write.QueryRowContext(ctx, `
		INSERT INTO Job (UserId, Status, Conversation, CreatedAt) VALUES (?, ?, ?, ?)
		RETURNING JobId, Status, Conversation, CreatedAt`,
		userID, JobQueued, string(raw), now()).
		Scan(&job.JobID, &job.Status, &stored, &job.CreatedAt)
	if err != nil {
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	job.UserID = userID
	if err := json.Unmarshal([]byte(stored), &job.Conversation); err != nil {
		return Job{}, fmt.Errorf("decode conversation: %w", err)
	}
	return job, nil
}

// ByID scopes the read to the caller so one user can never poll another's Job.
func (s *JobStore) ByID(ctx context.Context, userID, jobID int64) (Job, error) {
	return scanJob(s.read.QueryRowContext(ctx, `
		SELECT JobId, UserId, Status, RetryCount, Conversation, CreatedAt
		FROM Job WHERE JobId = ? AND UserId = ?`, jobID, userID))
}

// openJobsLimit caps the sidebar so a pile of Jobs cannot make Home unbounded.
const openJobsLimit = 50

// OpenJob is one row of the open-conversation sidebar. It carries the preview
// the list renders instead of the whole Conversation: the sidebar polls this,
// and a transcript can be two thousand characters long. An untranscribed clip
// has no words yet, so its preview is empty and the client supplies its own copy.
type OpenJob struct {
	JobID     int64  `json:"jobId"`
	Status    string `json:"status"`
	Preview   string `json:"preview"`
	CreatedAt string `json:"createdAt"`
}

// OpenJobs backs the open-conversation sidebar: every Job the agent has not
// closed yet, newest first. Failed Jobs stay in because the PRD keeps them
// visible for recovery, and a transcribed Job stays because nothing has read it
// yet. The slice is never nil so the reply is [] rather than null.
func (s *JobStore) OpenJobs(ctx context.Context, userID int64) ([]OpenJob, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT j.JobId, j.Status, j.CreatedAt,
		       COALESCE((
		         SELECT json_extract(m.value, '$.text')
		         FROM json_each(j.Conversation) m
		         WHERE json_extract(m.value, '$.role') = 'user'
		           AND COALESCE(json_extract(m.value, '$.text'), '') <> ''
		         ORDER BY m.key LIMIT 1
		       ), '') AS Preview
		FROM Job j
		WHERE j.UserId = ? AND j.Status <> ?
		ORDER BY j.CreatedAt DESC, j.JobId DESC LIMIT ?`, userID, JobComplete, openJobsLimit)
	if err != nil {
		return nil, fmt.Errorf("list open jobs: %w", err)
	}
	defer rows.Close()

	jobs := []OpenJob{}
	for rows.Next() {
		var job OpenJob
		if err := rows.Scan(&job.JobID, &job.Status, &job.CreatedAt, &job.Preview); err != nil {
			return nil, fmt.Errorf("scan open job: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list open jobs: %w", err)
	}
	return jobs, nil
}

// ClaimTranscription takes one queued Job whose user has no other Job in
// flight. The audio pending marker — a user message with an Audio reference and
// no Text yet — is the only condition. Saving a transcript clears the marker for
// that clip and leaves any later clip matching, so a second voice turn in the
// same conversation is claimable too.
func (s *JobStore) ClaimTranscription(ctx context.Context) (Job, bool, error) {
	job, err := scanJob(s.write.QueryRowContext(ctx, `
		UPDATE Job SET Status = ?
		WHERE JobId = (
			SELECT j.JobId FROM Job j
			WHERE j.Status = ?
			  AND NOT EXISTS (
				SELECT 1 FROM Job busy
				WHERE busy.UserId = j.UserId AND busy.Status = ? AND busy.JobId <> j.JobId)
			  AND EXISTS (
				SELECT 1 FROM json_each(j.Conversation) m
				WHERE json_extract(m.value, '$.role') = 'user'
				  AND json_extract(m.value, '$.audio') IS NOT NULL
				  AND json_extract(m.value, '$.audio') <> ''
				  AND COALESCE(json_extract(m.value, '$.text'), '') = '')
			ORDER BY j.CreatedAt LIMIT 1)
		RETURNING JobId, UserId, Status, RetryCount, Conversation, CreatedAt`,
		JobProcessing, JobQueued, JobProcessing))
	if errors.Is(err, ErrNotFound) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("claim transcription: %w", err)
	}
	return job, true, nil
}

// SaveTranscript fills the audio message's text and hands the Job back to the
// queue. Text and Trace commit together, so the claim marker — the very text the
// worker writes — and the transcript can never disagree.
func (s *JobStore) SaveTranscript(ctx context.Context, jobID int64, audio, text string, detail map[string]any) error {
	_, err := s.mutate(ctx, jobID, detail, func(job *Job) error {
		for i := range job.Conversation {
			message := &job.Conversation[i]
			if message.Role == "user" && message.Audio == audio && message.Text == "" {
				message.Text = text
				job.Status = JobQueued
				return nil
			}
		}
		return fmt.Errorf("audio message %q not found", audio)
	})
	return err
}

// RetryTranscription requeues the same Job; the audio blob stays on disk so the
// next attempt does not need a new upload.
func (s *JobStore) RetryTranscription(ctx context.Context, jobID int64, attempt int, detail map[string]any) error {
	_, err := s.mutate(ctx, jobID, detail, func(job *Job) error {
		job.RetryCount, job.Status = attempt, JobQueued
		return nil
	})
	return err
}

// RequeueInterrupted treats a crash mid-transcription as a failed attempt, as
// the PRD requires, so a Job left in processing is not invisible forever. The
// audio pending marker is the rule here too: a Job whose clip already has text
// was never left mid-flight, whatever traces it carries.
func (s *JobStore) RequeueInterrupted(ctx context.Context) (int64, error) {
	res, err := s.write.ExecContext(ctx, `
		UPDATE Job SET Status = ?, RetryCount = RetryCount + 1
		WHERE Status = ?
		  AND EXISTS (
			SELECT 1 FROM json_each(Job.Conversation) m
			WHERE json_extract(m.value, '$.role') = 'user'
			  AND json_extract(m.value, '$.audio') IS NOT NULL
			  AND json_extract(m.value, '$.audio') <> ''
			  AND COALESCE(json_extract(m.value, '$.text'), '') = '')`,
		JobQueued, JobProcessing)
	if err != nil {
		return 0, fmt.Errorf("requeue interrupted jobs: %w", err)
	}
	count, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("requeue interrupted jobs: %w", err)
	}
	return count, nil
}

// mutate is the whole durable outcome of one transcription attempt: the
// Conversation, Status, RetryCount, and Trace row land in one transaction, so a
// crash can never leave a transcribed Job that looks untranscribed and gets
// claimed again. The network round trip to phonon stays outside on purpose:
// SQLite has a single writer, and holding it for the length of a decode would
// block every other write in the app, captures included.
func (s *JobStore) mutate(ctx context.Context, jobID int64, detail map[string]any, change func(*Job) error) (Job, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, fmt.Errorf("begin job update: %w", err)
	}
	defer tx.Rollback()

	job, err := scanJob(tx.QueryRowContext(ctx, `
		SELECT JobId, UserId, Status, RetryCount, Conversation, CreatedAt
		FROM Job WHERE JobId = ?`, jobID))
	if err != nil {
		return Job{}, err
	}
	if err := change(&job); err != nil {
		return Job{}, err
	}
	raw, err := json.Marshal(job.Conversation)
	if err != nil {
		return Job{}, fmt.Errorf("encode conversation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE Job SET Status = ?, RetryCount = ?, Conversation = ? WHERE JobId = ?`,
		job.Status, job.RetryCount, string(raw), jobID); err != nil {
		return Job{}, fmt.Errorf("update job: %w", err)
	}
	if err := recordTrace(ctx, tx, jobID, detail); err != nil {
		return Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, fmt.Errorf("commit job update: %w", err)
	}
	return job, nil
}

// recordTrace writes within the caller's transaction, never on its own.
func recordTrace(ctx context.Context, tx *sql.Tx, jobID int64, detail map[string]any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encode trace: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO Trace (JobId, Timestamp, Stage, Detail) VALUES (?, ?, ?, ?)`,
		jobID, now(), transcriptionStage, string(raw)); err != nil {
		return fmt.Errorf("record transcription trace: %w", err)
	}
	return nil
}

func scanJob(row scanner) (Job, error) {
	var job Job
	var raw string
	err := row.Scan(&job.JobID, &job.UserID, &job.Status, &job.RetryCount, &raw, &job.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("scan job: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &job.Conversation); err != nil {
		return Job{}, fmt.Errorf("decode conversation: %w", err)
	}
	return job, nil
}

// FailTranscription parks the Job, appends the user-facing fallback, and keeps
// it visible for recovery. Callers remove the audio blob afterwards.
func (s *JobStore) FailTranscription(ctx context.Context, jobID int64, attempt int, fallback string, detail map[string]any) error {
	_, err := s.mutate(ctx, jobID, detail, func(job *Job) error {
		job.RetryCount, job.Status = attempt, JobFailed
		job.Conversation = append(job.Conversation, Message{Role: "assistant", Text: fallback, At: now()})
		return nil
	})
	return err
}
