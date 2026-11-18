// Package logging builds the application logger: structured JSON to stdout,
// with an optional asynchronous Better Stack (Logtail) sink.
//
// The remote sink never blocks application flow: records are queued to a
// bounded channel and dropped (with a counter) when Better Stack is slow or
// unreachable. Delivery is fire-and-forget; stdout always keeps the full
// record.
package logging

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	slogbetterstack "github.com/samber/slog-betterstack"
	"github.com/samber/slog-multi"

	"github.com/parakram/sap-attachment-api/internal/config"
)

// asyncQueueSize bounds remote-log buffering. Overflow drops records —
// remote logging must never backpressure the service.
const asyncQueueSize = 4096

// New returns the application logger and a Close func that flushes the
// remote sink (idempotent, safe to defer and to call on shutdown).
func New(cfg *config.Config) (*slog.Logger, func(context.Context)) {
	stdout := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.Level()})

	var remote slog.Handler
	flush := func(context.Context) {}
	if cfg.LogtailToken != "" {
		a := newAsyncHandler(slogbetterstack.Option{
			Level:   cfg.RemoteLevel(),
			Token:   cfg.LogtailToken,
			Timeout: 10 * time.Second,
		}.NewBetterstackHandler())
		remote = a
		flush = a.Close
	}

	var handler slog.Handler = stdout
	if remote != nil {
		handler = slogmulti.Fanout(stdout, remote)
	}

	slog.SetDefault(slog.New(handler))
	return slog.Default(), flush
}

// asyncCore owns the queue, the worker, and lifecycle state shared across
// WithAttrs/WithGroup clones.
type asyncCore struct {
	ch        chan slog.Record
	dropped   atomic.Int64
	done      chan struct{}
	closeOnce sync.Once
}

// asyncHandler decouples a slow remote handler from callers: Handle enqueues,
// a single worker goroutine delivers. Full queue => drop + count.
type asyncHandler struct {
	core *asyncCore
	next slog.Handler
}

func newAsyncHandler(next slog.Handler) *asyncHandler {
	core := &asyncCore{ch: make(chan slog.Record, asyncQueueSize), done: make(chan struct{})}
	go core.run(next)
	return &asyncHandler{core: core, next: next}
}

func (c *asyncCore) run(next slog.Handler) {
	defer close(c.done)
	for r := range c.ch {
		_ = next.Handle(context.Background(), r)
	}
}

func (a *asyncHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return a.next.Enabled(ctx, l)
}

func (a *asyncHandler) Handle(ctx context.Context, r slog.Record) error {
	select {
	case a.core.ch <- r:
	default:
		a.core.dropped.Add(1) // exported as a metric in the observability milestone
	}
	return nil
}

func (a *asyncHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &asyncHandler{core: a.core, next: a.next.WithAttrs(attrs)}
}

func (a *asyncHandler) WithGroup(name string) slog.Handler {
	return &asyncHandler{core: a.core, next: a.next.WithGroup(name)}
}

// Close drains the queue and stops the worker. Idempotent; waits up to ctx.
func (a *asyncHandler) Close(ctx context.Context) {
	a.core.closeOnce.Do(func() {
		close(a.core.ch)
		select {
		case <-a.core.done:
		case <-ctx.Done():
		}
	})
}
