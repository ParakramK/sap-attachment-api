package logging

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// collector is a slog.Handler that blocks on a release channel to control the
// async worker's progress deterministically.
type collector struct {
	mu        sync.Mutex
	received  int
	startOnce sync.Once
	started   chan struct{} // closed when the first record enters Handle
	release   chan struct{} // closed to let Handle return
}

func (c *collector) Handle(_ context.Context, _ slog.Record) error {
	c.startOnce.Do(func() { close(c.started) })
	c.mu.Lock()
	c.received++
	c.mu.Unlock()
	<-c.release
	return nil
}

func (c *collector) Enabled(context.Context, slog.Level) bool { return true }
func (c *collector) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *collector) WithGroup(string) slog.Handler            { return c }

func rec(msg string) slog.Record {
	return slog.Record{Time: time.Now(), Level: slog.LevelInfo, Message: msg}
}

func TestAsyncHandlerDeliversQueuedAndDropsOverflow(t *testing.T) {
	c := &collector{started: make(chan struct{}), release: make(chan struct{})}
	started := c.started
	a := newAsyncHandler(c, 2)

	_ = a.Handle(context.Background(), rec("one")) // worker picks up, blocks in Handle
	<-started

	_ = a.Handle(context.Background(), rec("two"))   // queued
	_ = a.Handle(context.Background(), rec("three")) // queued
	_ = a.Handle(context.Background(), rec("four"))  // dropped

	if got := a.core.dropped.Load(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}

	close(c.release)
	a.Close(context.Background())

	c.mu.Lock()
	got := c.received
	c.mu.Unlock()
	if got != 3 {
		t.Fatalf("received = %d, want 3", got)
	}
}

func TestAsyncHandlerCloseFlushesAndIsIdempotent(t *testing.T) {
	c := &collector{started: make(chan struct{}), release: make(chan struct{})}
	close(c.release) // Handle returns immediately

	a := newAsyncHandler(c, 16)
	_ = a.Handle(context.Background(), rec("flushed"))

	done := make(chan struct{})
	go func() {
		a.Close(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}

	// Second Close must not block or panic.
	a.Close(context.Background())
}
