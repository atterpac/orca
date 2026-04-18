package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/atterpac/orca/internal/storage/sqlite/gen"
	"github.com/atterpac/orca/pkg/orca"
)

// taskRepo adapts the sqlc-generated Queries to the storage.TaskRepo
// interface. The agents list is persisted as a JSON string because
// SQLite has no native array type and the list is always read and
// written whole.
type taskRepo struct {
	q *gen.Queries
}

func (r *taskRepo) Upsert(ctx context.Context, t orca.Task) error {
	agents := t.Agents
	if agents == nil {
		agents = []string{}
	}
	agentsJSON, err := json.Marshal(agents)
	if err != nil {
		return fmt.Errorf("marshal agents: %w", err)
	}
	return r.q.UpsertTask(ctx, gen.UpsertTaskParams{
		ID:           t.ID,
		Phase:        t.Phase,
		RepoRoot:     t.RepoRoot,
		WorktreePath: t.WorktreePath,
		ArtifactDir:  t.ArtifactDir,
		BaseRef:      t.BaseRef,
		Branch:       t.Branch,
		OpenedAt:     t.OpenedAt,
		ClosedAt:     t.ClosedAt,
		AgentsJson:   string(agentsJSON),
	})
}

func (r *taskRepo) Get(ctx context.Context, id string) (*orca.Task, bool, error) {
	row, err := r.q.GetTask(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	t, err := rowToTask(row)
	if err != nil {
		return nil, false, err
	}
	return &t, true, nil
}

func (r *taskRepo) ListOpen(ctx context.Context) ([]orca.Task, error) {
	rows, err := r.q.ListOpenTasks(ctx)
	if err != nil {
		return nil, err
	}
	return rowsToTasks(rows)
}

func (r *taskRepo) ListAll(ctx context.Context) ([]orca.Task, error) {
	rows, err := r.q.ListAllTasks(ctx)
	if err != nil {
		return nil, err
	}
	return rowsToTasks(rows)
}

func (r *taskRepo) Delete(ctx context.Context, id string) error {
	return r.q.DeleteTask(ctx, id)
}

func rowToTask(row gen.Task) (orca.Task, error) {
	var agents []string
	if row.AgentsJson != "" {
		if err := json.Unmarshal([]byte(row.AgentsJson), &agents); err != nil {
			return orca.Task{}, fmt.Errorf("unmarshal agents for task %s: %w", row.ID, err)
		}
	}
	return orca.Task{
		ID:           row.ID,
		Phase:        row.Phase,
		RepoRoot:     row.RepoRoot,
		WorktreePath: row.WorktreePath,
		ArtifactDir:  row.ArtifactDir,
		BaseRef:      row.BaseRef,
		Branch:       row.Branch,
		OpenedAt:     row.OpenedAt,
		ClosedAt:     row.ClosedAt,
		Agents:       agents,
	}, nil
}

func rowsToTasks(rows []gen.Task) ([]orca.Task, error) {
	out := make([]orca.Task, 0, len(rows))
	for _, r := range rows {
		t, err := rowToTask(r)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}
