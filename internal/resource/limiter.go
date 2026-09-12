// Package resource bounds active work before any queue lease or SMTP DATA.
package resource

import (
	"context"
	"errors"
	"sync"
)

const (
	ReceiveMemory int64 = 1 << 20
	SendMemory    int64 = 8 << 20
	DefaultMemory int64 = 64 << 20
	DefaultSpool  int64 = 256 << 20
)

// Reservations bound concurrent working sets, not total RSS or filesystem cache.
// Runtime/TLS/SQL overhead and idle connections need a separate process margin.
type reservation struct{ memory, spool int64 }
type Limiter struct {
	queue                                                         []*reservation
	mu                                                            sync.Mutex
	changed                                                       chan struct{}
	memoryLimit, spoolLimit, memory, spool, peakMemory, peakSpool int64
	waiting                                                       int
}
type Stats struct {
	MemoryLimit, SpoolLimit, Memory, Spool, PeakMemory, PeakSpool int64
	Waiting                                                       int
}

func New(memory, spool int64) *Limiter {
	return &Limiter{changed: make(chan struct{}), memoryLimit: memory, spoolLimit: spool}
}

var Default = New(DefaultMemory, DefaultSpool)

func (l *Limiter) Acquire(ctx context.Context, memory, spool int64) (func(), error) {
	if l == nil {
		l = Default
	}
	l.mu.Lock()
	if memory < 0 || spool < 0 || memory > l.memoryLimit || spool > l.spoolLimit {
		l.mu.Unlock()
		return nil, errors.New("work exceeds resource budget")
	}
	l.waiting++
	request := &reservation{memory, spool}
	l.queue = append(l.queue, request)
	for {
		if err := ctx.Err(); err != nil {
			l.waiting--
			for i, q := range l.queue {
				if q == request {
					l.queue = append(l.queue[:i], l.queue[i+1:]...)
					break
				}
			}
			close(l.changed)
			l.changed = make(chan struct{})
			l.mu.Unlock()
			return nil, err
		}
		if l.queue[0] == request && memory <= l.memoryLimit-l.memory && spool <= l.spoolLimit-l.spool {
			break
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		l.mu.Lock()
	}
	l.waiting--
	l.queue[0] = nil
	l.queue = l.queue[1:]
	close(l.changed)
	l.changed = make(chan struct{})
	l.memory += memory
	l.spool += spool
	if l.memory > l.peakMemory {
		l.peakMemory = l.memory
	}
	if l.spool > l.peakSpool {
		l.peakSpool = l.spool
	}
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.memory -= memory
			l.spool -= spool
			close(l.changed)
			l.changed = make(chan struct{})
		})
	}, nil
}
func (l *Limiter) Stats() Stats {
	if l == nil {
		l = Default
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return Stats{l.memoryLimit, l.spoolLimit, l.memory, l.spool, l.peakMemory, l.peakSpool, l.waiting}
}
