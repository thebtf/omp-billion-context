package main

import (
	"context"
	"errors"
	"sync"
)

var errPreviousTurnPending = errors.New("the previous CPA callback has not returned")

type sessionTurns struct {
	mu    sync.Mutex
	slots map[string]*turnSlot
}
type turnSlot struct {
	ctx  context.Context
	done chan struct{}
}

func (t *sessionTurns) lock(ctx context.Context, id string) (func(), error) {
	for {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		t.mu.Lock()
		if t.slots == nil {
			t.slots = make(map[string]*turnSlot)
		}
		previous := t.slots[id]
		if previous == nil {
			current := &turnSlot{ctx: ctx, done: make(chan struct{})}
			t.slots[id] = current
			t.mu.Unlock()
			return func() { t.mu.Lock(); delete(t.slots, id); close(current.done); t.mu.Unlock() }, nil
		}
		if previous.ctx.Err() != nil {
			t.mu.Unlock()
			return nil, errPreviousTurnPending
		}
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-previous.done:
		case <-previous.ctx.Done():
		}
	}
}
