package api

import (
	"context"
	"log/slog"
)

// ContextHandler decorates a slog.Handler, injecting request-scoped fields
// (trace_id, operator_code) pulled from the context.Context into EVERY log
// record. This satisfies cursor rule §9 ("every log entry MUST dynamically
// pull and include ... trace_id from the context.Context") without each call
// site having to thread the fields through manually — the engine and
// repository just use slog.XxxContext(ctx, ...) and the fields appear.
//
// Wire it at the composition root:
//
//	base := slog.NewJSONHandler(os.Stdout, opts)
//	slog.SetDefault(slog.New(api.NewContextHandler(base)))
type ContextHandler struct {
	inner slog.Handler
}

// NewContextHandler wraps inner so log records gain trace_id / operator_code
// from their context.
func NewContextHandler(inner slog.Handler) *ContextHandler {
	return &ContextHandler{inner: inner}
}

func (h *ContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if tid := TraceIDFromContext(ctx); tid != "" {
		r.AddAttrs(slog.String("trace_id", tid))
	}
	if op := OperatorCodeFromContext(ctx); op != "" {
		r.AddAttrs(slog.String("operator_code", op))
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs / WithGroup must re-wrap so the decorator survives logger.With()
// and logger.WithGroup() calls (otherwise the next .With() unwraps us).
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{inner: h.inner.WithGroup(name)}
}
