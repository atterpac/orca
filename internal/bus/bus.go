package bus

import (
	"context"
	"errors"

	"github.com/atterpac/orca/pkg/orca"
)

var (
	ErrTimeout    = errors.New("bus: request timeout")
	ErrNoReceiver = errors.New("bus: no receiver for direct message")
	ErrTTLExpired = errors.New("bus: message ttl expired")
)

type Filter struct {
	AgentID string
	Topic   string
	Kinds   []orca.MessageKind
}

type Cancel func()

type Bus interface {
	Publish(ctx context.Context, m orca.Message) error
	Subscribe(ctx context.Context, f Filter) (<-chan orca.Message, Cancel)
	Request(ctx context.Context, m orca.Message) (orca.Message, error)
}
