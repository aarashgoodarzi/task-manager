CREATE TABLE IF NOT EXISTS tasks (
    id          UUID PRIMARY KEY,
    title       TEXT NOT NULL CHECK (btrim(title) <> ''),
    description TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL CHECK (status IN ('todo', 'in_progress', 'done'))
);
