-- Blurt schema, phase 1. Applied with CREATE ... IF NOT EXISTS on boot so a
-- fresh volume and an existing one converge without a migration tool yet.
-- Time model: local date/time + IANA zone are the truth; UTC columns are
-- derived and rebuildable.

CREATE TABLE IF NOT EXISTS User (
    UserId          INTEGER PRIMARY KEY,
    Name            TEXT    NOT NULL,
    Email           TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    ModelProvider   TEXT,
    ModelName       TEXT,
    ModelEndpoint   TEXT,
    ModelApiKey     TEXT,   -- encrypted at rest
    -- Last questionnaire version this user finished; 0 means not started.
    LastQuestionnaireVersion INTEGER NOT NULL DEFAULT 0,
    CreatedAt       TEXT    NOT NULL
);

-- Login PINs are not persisted: the auth service keeps hashed PINs in an
-- in-memory map with a five-minute TTL (single use, attempt capped). A
-- restart simply invalidates outstanding PINs.

-- One row per logged-in device. Opaque HttpOnly cookie; delete to revoke.
CREATE TABLE IF NOT EXISTS Session (
    SessionId        INTEGER PRIMARY KEY,
    UserId           INTEGER NOT NULL REFERENCES User(UserId) ON DELETE CASCADE,
    TokenHash        BLOB    NOT NULL UNIQUE,
    ExpiresAt        TEXT    NOT NULL,
    PushSubscription TEXT,   -- device-specific subscription JSON
    CreatedAt        TEXT    NOT NULL,
    LastSeenAt       TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_session_user ON Session(UserId);

-- One conversation from capture through clarification.
CREATE TABLE IF NOT EXISTS Job (
    JobId        INTEGER PRIMARY KEY,
    UserId       INTEGER NOT NULL REFERENCES User(UserId) ON DELETE CASCADE,
    Status       TEXT    NOT NULL CHECK (Status IN ('queued','processing','clarifying','complete','failed')),
    RetryCount   INTEGER NOT NULL DEFAULT 0,
    Conversation TEXT    NOT NULL DEFAULT '[]',
    CreatedAt    TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_job_queue ON Job(Status, CreatedAt);

CREATE TABLE IF NOT EXISTS Reminder (
    ReminderId           INTEGER PRIMARY KEY,
    UserId               INTEGER NOT NULL REFERENCES User(UserId) ON DELETE CASCADE,
    Text                 TEXT    NOT NULL,
    Timezone             TEXT    NOT NULL,            -- IANA zone the user meant
    LocalDateTime        TEXT,                        -- one-off only, no offset
    EmitAtUtc            TEXT,                        -- one-off derived instant
    Status               TEXT    NOT NULL CHECK (Status IN ('resolved','fired','cancelled')),
    SourceJobId          INTEGER REFERENCES Job(JobId) ON DELETE SET NULL,
    RelativeToReminderId INTEGER REFERENCES Reminder(ReminderId) ON DELETE SET NULL,
    CreatedAt            TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reminder_window ON Reminder(UserId, EmitAtUtc);
CREATE INDEX IF NOT EXISTS idx_reminder_status ON Reminder(Status);

-- One-to-one extension for recurring reminders. Rule is JSON:
-- daily {time}, weekly {weekday,time}, monthly {day,time},
-- yearly {month,day,time}; optional start date.
CREATE TABLE IF NOT EXISTS RecurringReminder (
    ReminderId   INTEGER PRIMARY KEY REFERENCES Reminder(ReminderId) ON DELETE CASCADE,
    Rule         TEXT    NOT NULL,
    NextEmitAtUtc TEXT   NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_recurring_next ON RecurringReminder(NextEmitAtUtc);

CREATE TABLE IF NOT EXISTS Memory (
    MemoryId    INTEGER PRIMARY KEY,
    UserId      INTEGER NOT NULL REFERENCES User(UserId) ON DELETE CASCADE,
    Text        TEXT    NOT NULL,
    Tags        TEXT    NOT NULL DEFAULT '',
    Metadata    TEXT,   -- nullable JSON, e.g. trip dates and zone
    SourceJobId INTEGER REFERENCES Job(JobId) ON DELETE SET NULL,
    CreatedAt   TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memory_user ON Memory(UserId);

-- BM25 retrieval for phase 1, kept in sync by the triggers below.
CREATE VIRTUAL TABLE IF NOT EXISTS fts_memory USING fts5(
    text,
    tags,
    content='memory',
    content_rowid='MemoryId',
    tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER IF NOT EXISTS memory_ai AFTER INSERT ON Memory BEGIN
    INSERT INTO fts_memory(rowid, text, tags) VALUES (new.MemoryId, new.Text, new.Tags);
END;

CREATE TRIGGER IF NOT EXISTS memory_ad AFTER DELETE ON Memory BEGIN
    INSERT INTO fts_memory(fts_memory, rowid, text, tags) VALUES ('delete', old.MemoryId, old.Text, old.Tags);
END;

CREATE TRIGGER IF NOT EXISTS memory_au AFTER UPDATE ON Memory BEGIN
    INSERT INTO fts_memory(fts_memory, rowid, text, tags) VALUES ('delete', old.MemoryId, old.Text, old.Tags);
    INSERT INTO fts_memory(rowid, text, tags) VALUES (new.MemoryId, new.Text, new.Tags);
END;

-- Append-only diagnostics, always tied to a Job. Notification attempts carry
-- their ReminderId; that TraceId is the notification id in the push link.
CREATE TABLE IF NOT EXISTS Trace (
    TraceId    INTEGER PRIMARY KEY,
    JobId      INTEGER NOT NULL REFERENCES Job(JobId) ON DELETE CASCADE,
    ReminderId INTEGER REFERENCES Reminder(ReminderId) ON DELETE SET NULL,
    Timestamp  TEXT    NOT NULL,
    Stage      TEXT    NOT NULL,
    Detail     TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_trace_job ON Trace(JobId, Timestamp);

-- One report per notification. The linked Trace identifies Reminder and user.
CREATE TABLE IF NOT EXISTS NotificationFeedback (
    NotificationTraceId INTEGER PRIMARY KEY REFERENCES Trace(TraceId) ON DELETE CASCADE,
    FailureMode         TEXT    NOT NULL,
    Comment             TEXT,
    CreatedAt           TEXT    NOT NULL
);
