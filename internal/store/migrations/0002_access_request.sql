CREATE TABLE access_request (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     TEXT NOT NULL,
    user_name   TEXT NOT NULL,
    plex        TEXT NOT NULL,
    note        TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,
    channel_id  TEXT NOT NULL DEFAULT '',
    message_id  TEXT NOT NULL DEFAULT '',
    decided_by  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    decided_at  TEXT NOT NULL DEFAULT ''
);

-- One open request per member.
CREATE UNIQUE INDEX access_request_open ON access_request (user_id)
    WHERE status IN ('pending', 'processing');
