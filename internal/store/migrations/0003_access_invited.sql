-- When the Plex invite went out, so a restart mid-approval knows whether
-- to finish the onboarding or ask the approver to press Approve again.
ALTER TABLE access_request ADD COLUMN invited_at TEXT NOT NULL DEFAULT '';
