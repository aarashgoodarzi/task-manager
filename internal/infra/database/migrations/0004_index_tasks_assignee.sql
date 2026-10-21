-- The list endpoint filters on assignee equality. The status filter is already
-- covered by tasks_status_idx from 0002; both indexes serve the count query and
-- narrow the rows that must be ordered by id for a page.
CREATE INDEX IF NOT EXISTS tasks_assignee_idx ON tasks (assignee);
