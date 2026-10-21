-- Assignees are optional free-form labels. NULL means "unassigned"; the API
-- normalizes an empty request value to NULL so there is one representation.
-- The check mirrors the domain rule (trimmed, non-blank, at most 100
-- characters) so a direct SQL writer cannot store what the API rejects.
ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS assignee TEXT
    CHECK (assignee IS NULL OR (btrim(assignee) <> '' AND char_length(assignee) <= 100));
