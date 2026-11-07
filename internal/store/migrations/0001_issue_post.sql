CREATE TABLE issue_post (
    issue_id   INTEGER PRIMARY KEY,
    channel_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    created_at TEXT NOT NULL
);
