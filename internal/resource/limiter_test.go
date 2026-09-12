package resource

import (
	"context"
	"testing"
	"time"
)

func TestBudgetsCancellationAndRelease(t *testing.T) {
	l := New(10, 20)
	a, err := l.Acquire(context.Background(), 8, 15)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err = l.Acquire(ctx, 3, 1); err == nil {
		t.Fatal("memory oversubscribed")
	}
	if _, err = l.Acquire(context.Background(), 11, 0); err == nil {
		t.Fatal("impossible request accepted")
	}
	a()
	a()
	if s := l.Stats(); s.Memory != 0 || s.Spool != 0 || s.Waiting != 0 || s.PeakMemory != 8 || s.PeakSpool != 15 {
		t.Fatal(s)
	}
	b, err := l.Acquire(context.Background(), 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	if _, err = l.Acquire(ctx2, 1, 1); err == nil {
		t.Fatal("disk oversubscribed")
	}
	b()
}
func TestQueuedWorkCanProceedAfterRelease(t *testing.T) {
	l := New(10, 10)
	release, _ := l.Acquire(context.Background(), 10, 10)
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		r, err := l.Acquire(ctx, 10, 10)
		if err == nil {
			r()
		}
		close(done)
	}()
	release()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("waiter not woken")
	}
	if s := l.Stats(); s.Memory != 0 || s.Spool != 0 {
		t.Fatal(s)
	}
}
