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
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	slogbetterstack "github.com/BetterStackHQ/slog-betterstack"
	"github.com/samber/slog-multi"

	"github.com/parakram/sap-attachment-api/internal/config"
)

// asyncQueueSize bounds remote-log buffering. Overflow drops records —
// remote logging must never backpressure the service.
const asyncQueueSize = 4096

// New returns the application logger and a Close func that flushes the
// remote sink (idempotent, safe to defer and to call on shutdown).
func New(cfg *config.Config) (*slog.Logger, func(context.Context)) {
	sink := io.Writer(os.Stdout)
	var rotator *dailyRotator
	logsDir, err := executableLogsDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "file logging disabled:", err)
	} else if rotator, err = newDailyRotator(logsDir); err != nil {
		fmt.Fprintln(os.Stderr, "file logging disabled:", err)
		rotator = nil
	}
	if rotator != nil {
		// One handler, two writers: stdout and the daily file get identical JSON.
		sink = io.MultiWriter(os.Stdout, rotator)
	}
	stdout := slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: cfg.Level()})

	var remote slog.Handler
	flush := func(context.Context) {}
	if cfg.LogtailToken != "" {
		opts := slogbetterstack.Option{
			Level:           cfg.RemoteLevel(),
			Token:           cfg.LogtailToken,
			ShutdownTimeout: cfg.ShutdownTimeout,
		}
		if cfg.LogtailHost != "" {
			// Guide: Endpoint = https://$INGESTING_HOST/
			opts.Endpoint = "https://" + strings.TrimSuffix(strings.TrimPrefix(cfg.LogtailHost, "https://"), "/") + "/"
		}
		a := newAsyncHandler(opts.NewBetterstackHandler(), asyncQueueSize)
		remote = a
		flush = a.Close
	}

	var handler slog.Handler = stdout
	if remote != nil {
		handler = slogmulti.Fanout(stdout, remote)
	}

	slog.SetDefault(slog.New(handler))
	closeAll := func(ctx context.Context) {
		flush(ctx)
		if rotator != nil {
			_ = rotator.Close()
		}
	}
	return slog.Default(), closeAll
}

// asyncCore owns the queue, the worker, and lifecycle state shared across
// WithAttrs/WithGroup clones.
type asyncCore struct {
	ch        chan slog.Record
	dropped   atomic.Int64
	done      chan struct{}
	closeOnce sync.Once
	onClose   func() // flushes the underlying handler's batched records
}

// asyncHandler decouples a slow remote handler from callers: Handle enqueues,
// a single worker goroutine delivers. Full queue => drop + count.
type asyncHandler struct {
	core *asyncCore
	next slog.Handler
}

func newAsyncHandler(next slog.Handler, queue int) *asyncHandler {
	core := &asyncCore{ch: make(chan slog.Record, queue), done: make(chan struct{})}
	if closer, ok := next.(interface{ Close() error }); ok {
		core.onClose = func() { _ = closer.Close() }
	}
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
		if a.core.onClose != nil {
			a.core.onClose()
		}
	})
}
