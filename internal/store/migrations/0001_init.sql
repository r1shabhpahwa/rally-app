-- Initial schema. Money is stored in integer cents; timestamps are unix seconds
-- (UTC). Session date/time stay as local wall-clock strings because that is what
-- the organizer types and what the email has to show.

CREATE TABLE app_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE organizer (
    id            INTEGER PRIMARY KEY,
    name          TEXT NOT NULL,
    email         TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE TABLE player (
    id              INTEGER PRIMARY KEY,
    name            TEXT NOT NULL,
    email           TEXT NOT NULL COLLATE NOCASE,
    active          INTEGER NOT NULL DEFAULT 1,
    token           TEXT NOT NULL UNIQUE,
    unsubscribed_at INTEGER,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_player_email ON player (email COLLATE NOCASE);
CREATE INDEX idx_player_active ON player (active);

CREATE TABLE session (
    id                       INTEGER PRIMARY KEY,
    public_id                TEXT NOT NULL UNIQUE,
    date                     TEXT NOT NULL,
    start_time               TEXT NOT NULL,
    end_time                 TEXT NOT NULL,
    courts                   INTEGER NOT NULL,
    cost_per_court_hour_cents INTEGER NOT NULL,
    max_players              INTEGER NOT NULL,
    signup_deadline_at       INTEGER NOT NULL,
    status                   TEXT NOT NULL,
    notes                    TEXT NOT NULL DEFAULT '',
    invitation_sent_at       INTEGER,
    last_reminder_at         INTEGER,
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL
);
CREATE INDEX idx_session_date ON session (date);

CREATE TABLE court_deadline (
    id           INTEGER PRIMARY KEY,
    session_id   INTEGER NOT NULL REFERENCES session (id) ON DELETE CASCADE,
    court_number INTEGER NOT NULL,
    deadline_at  INTEGER NOT NULL,
    UNIQUE (session_id, court_number)
);

CREATE TABLE rsvp (
    id                INTEGER PRIMARY KEY,
    session_id        INTEGER NOT NULL REFERENCES session (id) ON DELETE CASCADE,
    player_id         INTEGER NOT NULL REFERENCES player (id) ON DELETE CASCADE,
    status            TEXT NOT NULL,
    guest_count       INTEGER NOT NULL DEFAULT 0,
    token             TEXT NOT NULL UNIQUE,
    waitlist_position INTEGER,
    source            TEXT NOT NULL DEFAULT 'invite',
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    UNIQUE (session_id, player_id)
);
CREATE INDEX idx_rsvp_session_status ON rsvp (session_id, status);

CREATE TABLE email_outbox (
    id              INTEGER PRIMARY KEY,
    kind            TEXT NOT NULL,
    session_id      INTEGER REFERENCES session (id) ON DELETE SET NULL,
    player_id       INTEGER REFERENCES player (id) ON DELETE SET NULL,
    to_email        TEXT NOT NULL,
    to_name         TEXT NOT NULL DEFAULT '',
    subject         TEXT NOT NULL,
    text_body       TEXT NOT NULL,
    html_body       TEXT NOT NULL,
    unsubscribe_url TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'pending',
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    sent_at         INTEGER
);
CREATE INDEX idx_outbox_due ON email_outbox (status, next_attempt_at);
CREATE INDEX idx_outbox_session ON email_outbox (session_id);

CREATE TABLE setting (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
