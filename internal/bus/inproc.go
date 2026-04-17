package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"sync"
	"time"

	"github.com/atterpac/orca/pkg/orca"
)

const defaultBuffer = 64

type subscription struct {
	id     string
	filter Filter
	ch     chan orca.Message
}

type InProc struct {
	mu      sync.RWMutex
	subs    map[string]*subscription
	pending map[string]chan orca.Message
}

func NewInProc() *InProc {
	return &InProc{
		subs:    make(map[string]*subscription),
		pending: make(map[string]chan orca.Message),
	}
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (b *InProc) Publish(ctx context.Context, m orca.Message) error {
	if m.ID == "" {
		m.ID = newID()
	}
	if m.Timestamp.IsZero() {
		m.Timestamp = time.Now()
	}
	if m.TTL > 0 {
		m.TTL--
		if m.TTL == 0 {
			return ErrTTLExpired
		}
	}

	if m.Kind == orca.KindResponse && m.CorrelationID != "" {
		b.mu.Lock()
		ch, ok := b.pending[m.CorrelationID]
		if ok {
			delete(b.pending, m.CorrelationID)
		}
		b.mu.Unlock()
		if ok {
			select {
			case ch <- m:
			default:
			}
			return nil
		}
	}

	b.mu.RLock()
	targets := make([]chan orca.Message, 0)
	for _, s := range b.subs {
		if matches(s.filter, m) {
			targets = append(targets, s.ch)
		}
	}
	b.mu.RUnlock()

	for _, ch := range targets {
		select {
		case ch <- m:
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	return nil
}

func (b *InProc) Subscribe(ctx context.Context, f Filter) (<-chan orca.Message, Cancel) {
	s := &subscription{
		id:     newID(),
		filter: f,
		ch:     make(chan orca.Message, defaultBuffer),
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

func (b *InProc) Request(ctx context.Context, m orca.Message) (orca.Message, error) {
	if m.ID == "" {
		m.ID = newID()
	}
	if m.CorrelationID == "" {
		m.CorrelationID = m.ID
	}
	m.Kind = orca.KindRequest

	replyCh := make(chan orca.Message, 1)
	b.mu.Lock()
	b.pending[m.CorrelationID] = replyCh
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.pending, m.CorrelationID)
		b.mu.Unlock()
	}()

	if err := b.Publish(ctx, m); err != nil {
		return orca.Message{}, err
	}

	select {
	case reply := <-replyCh:
		return reply, nil
	case <-ctx.Done():
		return orca.Message{}, ctx.Err()
	}
}

func matches(f Filter, m orca.Message) bool {
	if f.AgentID != "" && m.To != f.AgentID {
		return false
	}
	if f.Topic != "" && m.Topic != f.Topic {
		return false
	}
	if len(f.Kinds) > 0 && !slices.Contains(f.Kinds, m.Kind) {
		return false
	}
	return true
}
