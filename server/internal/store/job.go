package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Job lifecycle. Redis streams carry the stage a Job waits in; SQL only records
// whether it is still running, needs the user, or stopped.
const (
	JobOpen       = "open"
	JobClarifying = "clarifying"
	JobComplete   = "complete"
	JobFailed     = "failed"
)

const (
	TranscriptionStage = "transcription"
	AgentStage         = "agent"
	// NotificationStage is the emitter's two rows per firing: the attempt, written
	// with the state change, and the delivery outcome that points back at it.
	NotificationStage = "notification"
)

// MaxAttempts is the PRD's poison-pill limit for one processing stage.
const MaxAttempts = 3

// openJobsLimit caps the sidebar so a pile of Jobs cannot make Home unbounded.
const openJobsLimit = 50

// ErrJobClosed means the Job has already finished, so a reply has nothing to
// attach to.
var ErrJobClosed = errors.New("job is closed")

// Message is one Conversation turn. Audio is the temporary blob's filename,
// never a path: the storage directory stays server-side.
type Message struct {
	Role  string `json:"role"`
	Text  string `json:"text,omitempty"`
	Audio string `json:"audio,omitempty"`
	// TZ is the device's IANA zone at the moment of this turn: the anchor the
	// AgentLoop resolves a relative time against. It is per turn because a user
	// can blurt from another zone, and the newest turn is the one that counts.
	TZ string `json:"tz,omitempty"`
	At string `json:"at"`
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

// JobStore owns Job, Conversation, and the transcription Trace rows.
type JobStore struct {
	write *sql.DB
	read  *sql.DB
}

// OpenJob is one row of the open-conversation sidebar. It carries the preview
// the list renders instead of the whole Conversation.
type OpenJob struct {
	JobID     int64  `json:"jobId"`
	Status    string `json:"status"`
	Preview   string `json:"preview"`
	CreatedAt string `json:"createdAt"`
}

// AgentStep is the whole durable outcome of one AgentLoop step that commits.
type AgentStep struct {
	Reminders []Reminder
	Memories  []Memory
	Reply     Message
	Status    string
	Detail    map[string]any
}

// pendingAudio reports whether one turn is a clip the transcriber still owes a
// transcript for.
func pendingAudio(message Message) bool {
	return message.Role == "user" && message.Audio != "" && message.Text == ""
}

// PendingAudio returns the first user clip that has not been transcribed.
func (j Job) PendingAudio() (string, bool) {
	for _, message := range j.Conversation {
		if pendingAudio(message) {
			return message.Audio, true
		}
	}
	return "", false
}

func NewJobStore(write, read *sql.DB) *JobStore {
	return &JobStore{write: write, read: read}
}

// Create inserts an open Job. The caller publishes its first queue message.
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
		INSERT INTO Job (UserId, Status, Conversation, CreatedAt, queued_at)
		VALUES (?, ?, ?, ?, ?)
		RETURNING JobId, Status, Conversation, CreatedAt`,
		userID, JobOpen, string(raw), now(), now()).
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

// Get loads a Job without an owner filter, for queue consumers.
func (s *JobStore) Get(ctx context.Context, jobID int64) (Job, error) {
	return scanJob(s.read.QueryRowContext(ctx, `
		SELECT JobId, UserId, Status, RetryCount, Conversation, CreatedAt
		FROM Job WHERE JobId = ?`, jobID))
}

// OpenJobs backs the open-conversation sidebar: every Job the agent has not
// closed yet, newest first.
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

// StaleOpenJobs returns Jobs that committed to SQL but may not have reached
// Redis, for the queue sweeper.
func (s *JobStore) StaleOpenJobs(ctx context.Context, before string) ([]Job, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT JobId, UserId, Status, RetryCount, Conversation, CreatedAt
		FROM Job WHERE Status = ? AND queued_at < ?`, JobOpen, before)
	if err != nil {
		return nil, fmt.Errorf("list stale jobs: %w", err)
	}
	defer rows.Close()

	jobs := []Job{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stale jobs: %w", err)
	}
	return jobs, nil
}

// SaveTranscript writes the transcript onto the clip's own turn. The caller
// decides which stream the Job enters next.
func (s *JobStore) SaveTranscript(ctx context.Context, jobID int64, audio, text string, detail map[string]any) (Job, error) {
	return s.mutate(ctx, TranscriptionStage, jobID, detail, func(job *Job, _ *sql.Tx) error {
		for i := range job.Conversation {
			message := &job.Conversation[i]
			if message.Role == "user" && message.Audio == audio && message.Text == "" {
				message.Text, job.RetryCount = text, 0
				job.Status = JobOpen
				return nil
			}
		}
		return fmt.Errorf("audio message %q not found", audio)
	})
}

// Retry hands a Job back to its stage with the failed attempt recorded.
func (s *JobStore) Retry(ctx context.Context, stage string, jobID int64, attempt int, detail map[string]any) error {
	_, err := s.mutate(ctx, stage, jobID, detail, func(job *Job, _ *sql.Tx) error {
		job.RetryCount, job.Status = attempt, JobOpen
		return nil
	})
	return err
}

// Fail parks the Job, appends the user-facing fallback, and keeps it visible.
func (s *JobStore) Fail(ctx context.Context, stage string, jobID int64, attempt int, fallback string, detail map[string]any) error {
	_, err := s.mutate(ctx, stage, jobID, detail, func(job *Job, _ *sql.Tx) error {
		job.RetryCount, job.Status = attempt, JobFailed
		job.Conversation = append(job.Conversation, Message{Role: "assistant", Text: fallback, At: now()})
		return nil
	})
	return err
}

// mutate commits the Conversation, Status, RetryCount, any step records, and
// the Trace row in one transaction.
func (s *JobStore) mutate(ctx context.Context, stage string, jobID int64, detail map[string]any, change func(*Job, *sql.Tx) error) (Job, error) {
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
	if err := change(&job, tx); err != nil {
		return Job{}, err
	}
	raw, err := json.Marshal(job.Conversation)
	if err != nil {
		return Job{}, fmt.Errorf("encode conversation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE Job SET Status = ?, RetryCount = ?, Conversation = ?, queued_at = ? WHERE JobId = ?`,
		job.Status, job.RetryCount, string(raw), now(), jobID); err != nil {
		return Job{}, fmt.Errorf("update job: %w", err)
	}
	if _, err := recordTrace(ctx, tx, stage, jobID, 0, detail); err != nil {
		return Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, fmt.Errorf("commit job update: %w", err)
	}
	return job, nil
}

// recordTrace writes a Trace row and returns its id, inside the caller's
// transaction when there is one. ReminderId is set on a notification attempt, which
// is what ties an interruption back to the Reminder it was about; every other stage
// leaves it null. The id is returned because a notification's TraceId is the
// notification id the push payload carries.
func recordTrace(ctx context.Context, q execer, stage string, jobID, reminderID int64, detail map[string]any) (int64, error) {
	raw, err := json.Marshal(detail)
	if err != nil {
		return 0, fmt.Errorf("encode trace: %w", err)
	}
	var traceID int64
	if err := q.QueryRowContext(ctx,
		`INSERT INTO Trace (JobId, ReminderId, Timestamp, Stage, Detail) VALUES (?, ?, ?, ?, ?) RETURNING TraceId`,
		jobID, nullID(reminderID), now(), stage, string(raw)).Scan(&traceID); err != nil {
		return 0, fmt.Errorf("record %s trace: %w", stage, err)
	}
	return traceID, nil
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

// AppendTurn adds the user's own turn and hands the Job back to the agent
// stream with a fresh attempt budget.
func (s *JobStore) AppendTurn(ctx context.Context, userID, jobID int64, message Message, detail map[string]any) (Job, error) {
	return s.mutate(ctx, AgentStage, jobID, detail, func(job *Job, _ *sql.Tx) error {
		if job.UserID != userID {
			return ErrNotFound
		}
		switch job.Status {
		case JobOpen, JobClarifying, JobFailed:
		default:
			return fmt.Errorf("%w: job %d is %s", ErrJobClosed, jobID, job.Status)
		}
		message.Role, message.At = "user", now()
		kept := job.Conversation[:0]
		for _, turn := range job.Conversation {
			if !pendingAudio(turn) {
				kept = append(kept, turn)
			}
		}
		job.Conversation = append(kept, message)
		job.RetryCount, job.Status = 0, JobOpen
		return nil
	})
}

// TagMemory writes the search features the tagger generated for one committed Memory. It
// updates the row in place rather than inserting, so the record the user's capture already
// made is what becomes findable, and the schema's own trigger keeps the FTS index in step. A
// Job's Memory is unique by (SourceJobId, Text), which is how the row is found.
func (s *JobStore) TagMemory(ctx context.Context, jobID int64, text, tags string) error {
	if _, err := s.write.ExecContext(ctx,
		`UPDATE Memory SET Tags = ? WHERE SourceJobId = ? AND Text = ?`, tags, jobID, text); err != nil {
		return fmt.Errorf("tag memory: %w", err)
	}
	return nil
}

// CommitAgentStep writes one step atomically.
func (s *JobStore) CommitAgentStep(ctx context.Context, jobID int64, step AgentStep) (Job, error) {
	return s.mutate(ctx, AgentStage, jobID, step.Detail, func(job *Job, tx *sql.Tx) error {
		for _, reminder := range step.Reminders {
			reminder.UserID, reminder.SourceJobID = job.UserID, jobID
			if err := insertReminder(ctx, tx, reminder); err != nil {
				return err
			}
		}
		for _, memory := range step.Memories {
			memory.UserID, memory.SourceJobID = job.UserID, jobID
			if err := insertMemory(ctx, tx, memory); err != nil {
				return err
			}
		}
		if step.Reply.Text != "" || step.Reply.Audio != "" {
			reply := step.Reply
			reply.Role, reply.At = "assistant", now()
			job.Conversation = append(job.Conversation, reply)
		}
		job.Status = step.Status
		return nil
	})
}
