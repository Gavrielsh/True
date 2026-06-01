package api

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Gavrielsh/TransactionMechanism/pkg/errors"
)

// Recovery converts an unexpected panic in any downstream handler into a
// clean HTTP 500, logged at ERROR with the trace_id.
//
// The codebase never calls panic() deliberately (cursor rule §7), but a
// nil-map write or out-of-range slice in third-party code must NOT take
// the process down or leak a stack trace to the operator. This is the
// outermost middleware so it wraps everything.
func Recovery(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorContext(c.Request.Context(), "panic recovered",
					slog.Any("panic", r),
					slog.String("path", c.Request.URL.Path),
					slog.String("trace_id", traceIDFrom(c)),
				)
				// AbortWithStatusJSON is a no-op if the response was already
				// (partially) written, which is the best we can do mid-stream.
				if !c.Writer.Written() {
					respondErrorCode(c, http.StatusInternalServerError,
						errors.CodeInternal, "internal error")
				} else {
					c.Abort()
				}
			}
		}()
		c.Next()
	}
}
