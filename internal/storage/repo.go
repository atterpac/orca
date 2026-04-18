// Package storage defines persistence interfaces for orca's runtime
// state. Implementations live in sub-packages (sqlite/, …). Callers
// depend on these interfaces so the backing store can be swapped
// without touching supervisor or daemon wiring.
//
// The umbrella Store constructs per-entity repositories. Each
// repository is responsible for a single aggregate (tasks, decisions,
// discussions, agents). Nil Store is a valid "no persistence" mode —
// supervisor checks for nil before every call.
package storage

import (
	"context"

	"github.com/atterpac/orca/pkg/orca"
)

// Store is the persistence umbrella. Close releases any resources
// held by the underlying driver (e.g. sqlite file handle).
type Store interface {
	Tasks() TaskRepo
	Close() error
}

// TaskRepo persists orca.Task records. Upsert is idempotent on ID so
// callers don't need a separate create/update distinction.
type TaskRepo interface {
	Upsert(ctx context.Context, t orca.Task) error
	Get(ctx context.Context, id string) (*orca.Task, bool, error)
	ListOpen(ctx context.Context) ([]orca.Task, error)
	ListAll(ctx context.Context) ([]orca.Task, error)
	Delete(ctx context.Context, id string) error
}
