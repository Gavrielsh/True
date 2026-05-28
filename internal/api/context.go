package api

import (
	"context"

	"github.com/gin-gonic/gin"
)

// Context keys for values stored on gin.Context by middlewares and read by
// handlers / loggers. Typed empty structs prevent string-key collisions.
type (
	traceIDKey      struct{}
	operatorCodeKey struct{}
)

const (
	// ginKeyTraceID / ginKeyOperator: the values are stored on gin.Context
	// (string-keyed) because gin's middleware-to-handler bridge uses string
	// keys. We mirror them onto the request context.Context via the typed
	// keys above so downstream Go code uses ctx.Value(traceIDKey{}).
	ginKeyTraceID  = "trace_id"
	ginKeyOperator = "operator_code"
)

// withTraceID and withOperator attach values to a request context.Context
// for propagation to the engine / repository layers.
func withTraceID(parent context.Context, v string) context.Context {
	return context.WithValue(parent, traceIDKey{}, v)
}
func withOperator(parent context.Context, v string) context.Context {
	return context.WithValue(parent, operatorCodeKey{}, v)
}

// TraceIDFromContext is used by structured loggers (cursor rule §9) to pull
// the trace_id out of any context.Context produced by this package.
func TraceIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey{}).(string); ok {
		return v
	}
	return ""
}

// OperatorCodeFromContext extracts the operator code that HMAC middleware
// verified for this request. Handlers use it to populate engine requests
// without trusting client-supplied operator fields.
func OperatorCodeFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(operatorCodeKey{}).(string); ok {
		return v
	}
	return ""
}

func traceIDFrom(c *gin.Context) string {
	if v, ok := c.Get(ginKeyTraceID); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
