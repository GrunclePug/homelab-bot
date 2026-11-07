-- The plex.tv share an approval created, so it can be removed when the
-- member leaves, and where that removal stands: '' (nothing to do),
-- 'asked' (approvers were asked), 'kept', 'removing' or 'removed'.
ALTER TABLE access_request ADD COLUMN plex_share_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE access_request ADD COLUMN share_state TEXT NOT NULL DEFAULT '';
