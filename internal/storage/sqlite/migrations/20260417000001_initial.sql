-- +goose Up
-- +goose StatementBegin
CREATE TABLE tasks (
    id              TEXT PRIMARY KEY,
    phase           TEXT NOT NULL,
    repo_root       TEXT NOT NULL,
    worktree_path   TEXT NOT NULL DEFAULT '',
    artifact_dir    TEXT NOT NULL DEFAULT '',
    base_ref        TEXT NOT NULL DEFAULT '',
    branch          TEXT NOT NULL DEFAULT '',
    opened_at       TIMESTAMP NOT NULL,
    closed_at       TIMESTAMP,
    agents_json     TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX idx_tasks_phase ON tasks(phase);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_tasks_phase;
DROP TABLE IF EXISTS tasks;
-- +goose StatementEnd
