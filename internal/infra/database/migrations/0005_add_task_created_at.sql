-- List order is FIFO (oldest first). created_at is the primary sort key;
-- id is the tie-breaker so keyset pagination stays total-ordered when two
-- rows share a timestamp.
ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now();
