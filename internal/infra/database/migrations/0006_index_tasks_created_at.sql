-- Supports ORDER BY created_at ASC, id ASC (FIFO). A DESC index is still
-- usable via a backward scan; the column pair is what matters for the keyset.
CREATE INDEX IF NOT EXISTS tasks_created_at_id_idx ON tasks (created_at DESC, id DESC);
