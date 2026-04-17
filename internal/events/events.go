package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"sync"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

type Filter struct {
	AgentID string
	Kinds   []orca.EventKind
}

type Cancel func()

type Sink interface {
	Emit(e orca.Event)
}

type Source interface {
	Subscribe(ctx context.Context, f Filter) (<-chan orca.Event, Cancel)
}

type Bus struct {
	mu     sync.RWMutex
	subs   map[string]*subscription
	buffer int
}

type subscription struct {
	id     string
	filter Filter
	ch     chan orca.Event
}

func NewBus(buffer int) *Bus {
	if buffer <= 0 {
		buffer = 256
	}
	return &Bus{subs: make(map[string]*subscription), buffer: buffer}
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (b *Bus) Emit(e orca.Event) {
	if e.V == 0 {
		e.V = 1
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.subs {
		if !matches(s.filter, e) {
			continue
		}
		select {
		case s.ch <- e:
		default:
			// drop on backpressure
		}
	}
}

func (b *Bus) Subscribe(ctx context.Context, f Filter) (<-chan orca.Event, Cancel) {
	s := &subscription{
		id:     newID(),
		filter: f,
		ch:     make(chan orca.Event, b.buffer),
	}
	b.mu.Lock()
	b.subs[s.id] = s
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		if existing, ok := b.subs[s.id]; ok {
			delete(b.subs, s.id)
			close(existing.ch)
		}
		b.mu.Unlock()
	}
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return s.ch, cancel
}

func matches(f Filter, e orca.Event) bool {
	if f.AgentID != "" && e.AgentID != f.AgentID {
		return false
	}
	if len(f.Kinds) > 0 {
		return slices.Contains(f.Kinds, e.Kind)
	}
	return true
}
