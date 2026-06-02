package worker

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
)

// guard runs fn and converts any panic into a returned error, logging the
// recovered value and full stack trace at ERROR.
//
// Background workers run as top-level goroutines next to the HTTP server. A
// panic that escapes such a goroutine is fatal to the WHOLE process — it would
// take the Gin server (and every in-flight player transaction) down with it.
// Each worker therefore wraps its per-cycle unit of work in guard so a single
// poisoned cycle is logged and the loop survives to the next tick. The returned
// error flows through the loop's normal "cycle failed, retry next tick" path,
// so a panicking cycle is indistinguishable from a returned-error cycle to the
// caller — by design.
func guard(ctx context.Context, logger *slog.Logger, name string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s: panic recovered: %v", name, r)
			logger.ErrorContext(ctx, "worker panic recovered",
				slog.String("worker", name),
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
			)
		}
	}()
	return fn()
}
