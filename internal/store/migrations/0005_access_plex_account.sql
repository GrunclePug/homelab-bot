-- A share's ID changes when a pending invite is accepted, so approvals
-- record who the share is for instead: "id:" and the Plex account ID, or
-- "email:" and a SHA-256 of an address that has no Plex account yet.
ALTER TABLE access_request DROP COLUMN plex_share_id;
ALTER TABLE access_request ADD COLUMN plex_account TEXT NOT NULL DEFAULT '';
