-- The "Remove Plex access?" card, so it can be retired when it no longer
-- applies and posted again if someone deletes it, and whether the account
-- already had access when the request was approved.
ALTER TABLE access_request ADD COLUMN share_channel_id TEXT NOT NULL DEFAULT '';
ALTER TABLE access_request ADD COLUMN share_message_id TEXT NOT NULL DEFAULT '';
ALTER TABLE access_request ADD COLUMN plex_already INTEGER NOT NULL DEFAULT 0;
