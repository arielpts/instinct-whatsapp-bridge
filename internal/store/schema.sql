CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Tokens are single use. redeemed_unix is the replay defence: a token that has
-- been spent cannot bind a second message (SEC-2, FR-9).
CREATE TABLE IF NOT EXISTS tokens (
    token         TEXT PRIMARY KEY,
    conversation  TEXT    NOT NULL,
    message       TEXT    NOT NULL,
    domain        TEXT    NOT NULL,
    issued_unix   INTEGER NOT NULL,
    redeemed_unix INTEGER
);
CREATE INDEX IF NOT EXISTS tokens_issued ON tokens (issued_unix);

-- What we forwarded. body is purged on the retention schedule; the row stays,
-- so a redelivered WhatsApp message is still recognised as a duplicate after
-- its content is gone (SEC-7).
CREATE TABLE IF NOT EXISTS forwards (
    message_id     TEXT PRIMARY KEY,
    conversation   TEXT    NOT NULL,
    sender         TEXT    NOT NULL,
    body           TEXT,
    received_unix  INTEGER NOT NULL,
    forwarded_unix INTEGER
);
CREATE INDEX IF NOT EXISTS forwards_received ON forwards (received_unix);

-- One row per reply email. The unique mail_message_id is what stops a
-- redelivered or retried email from producing a second WhatsApp message.
CREATE TABLE IF NOT EXISTS candidates (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    mail_message_id TEXT    NOT NULL UNIQUE,
    token           TEXT    NOT NULL,
    conversation    TEXT    NOT NULL,
    state           TEXT    NOT NULL,
    created_unix    INTEGER NOT NULL,
    resolved_unix   INTEGER,
    reason          TEXT
);
CREATE INDEX IF NOT EXISTS candidates_state ON candidates (state);

-- Per-bubble delivery state, written either side of each send so a crash
-- mid-delivery is recoverable and a partially delivered candidate is never
-- replayed wholesale (FR-12).
CREATE TABLE IF NOT EXISTS bubbles (
    candidate_id  INTEGER NOT NULL REFERENCES candidates (id),
    idx           INTEGER NOT NULL,
    body          TEXT    NOT NULL,
    sent_unix     INTEGER,
    wa_message_id TEXT,
    PRIMARY KEY (candidate_id, idx)
);

-- Quota accounting, one row per delivered bubble: quotas count messages that
-- actually reached someone, not candidates (SEC-6).
CREATE TABLE IF NOT EXISTS sends (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation TEXT    NOT NULL,
    sent_unix    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sends_time ON sends (sent_unix);
