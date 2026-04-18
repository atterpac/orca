-- name: UpsertTask :exec
INSERT INTO tasks (
    id, phase, repo_root, worktree_path, artifact_dir,
    base_ref, branch, opened_at, closed_at, agents_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    phase         = excluded.phase,
    worktree_path = excluded.worktree_path,
    artifact_dir  = excluded.artifact_dir,
    base_ref      = excluded.base_ref,
    branch        = excluded.branch,
    closed_at     = excluded.closed_at,
    agents_json   = excluded.agents_json;

-- name: GetTask :one
SELECT * FROM tasks WHERE id = ? LIMIT 1;

-- name: ListOpenTasks :many
SELECT * FROM tasks WHERE phase != 'closed' ORDER BY opened_at DESC;

-- name: ListAllTasks :many
SELECT * FROM tasks ORDER BY opened_at DESC;

-- name: DeleteTask :exec
DELETE FROM tasks WHERE id = ?;
