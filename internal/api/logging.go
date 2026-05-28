package api

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Telemetry assigns a trace_id to every request and emits one structured log
// line per request — but only at WARN (4xx) and ERROR (5xx).
//
// Per cursor rule §9 we do NOT log INFO/DEBUG on the hot path: a successful
// /bet at 10k TPS would otherwise generate 10k log lines/sec and bottleneck
// on disk I/O. Success is observed via metrics (out of scope here), not logs.
//
// The trace_id is taken from an inbound X-Trace-Id header when present (so an
// aggregator's correlation id flows through), otherwise generated. It is
// attached to the request context.Context so the engine / repository layers
// can include it in their own structured logs.
func Telemetry(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		traceID := c.GetHeader("X-Trace-Id")
		if traceID == "" {
			traceID = uuid.NewString()
		}
		c.Set(ginKeyTraceID, traceID)
		c.Request = c.Request.WithContext(withTraceID(c.Request.Context(), traceID))
		c.Header("X-Trace-Id", traceID)

		c.Next()

		status := c.Writer.Status()
		if status < 400 {
			return // hot path: no INFO/DEBUG logging
		}

		attrs := []any{
			slog.String("trace_id", traceID),
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.Path),
			slog.Int("status", status),
			slog.Duration("latency", time.Since(start)),
		}
		// NB: we never log headers (X-Signature) or the body (PII) — §9.
		if status >= 500 {
			logger.ErrorContext(c.Request.Context(), "request failed", attrs...)
		} else {
			logger.WarnContext(c.Request.Context(), "request rejected", attrs...)
		}
	}
}
