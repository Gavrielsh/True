// Package telemetry wires OpenTelemetry distributed tracing for the engine.
// Spans cover the critical money path (HTTP handler → Redis idempotency →
// Postgres transaction) per cursor rule §9.
//
// DATA PRIVACY (§9): span attributes carry identifiers only — player_id
// (UUID, not PII), operator_code, operator_transaction_id, currency family.
// NEVER raw amounts, emails, tokens, or signatures.
package telemetry

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// tracerName scopes every span the engine emits.
const tracerName = "github.com/Gavrielsh/True"

// shutdownTimeout bounds the final span flush at process exit.
const shutdownTimeout = 5 * time.Second

// Init configures the global tracer provider from the environment.
//
//	OTEL_EXPORTER_OTLP_ENDPOINT unset  → default "localhost:4317"
//	OTEL_EXPORTER_OTLP_ENDPOINT == ""  → no-op tracer (dev mode, never crash)
//
// The returned shutdown flushes buffered spans; callers defer it in main.
// The OTLP gRPC exporter dials lazily and retries in the background, so an
// absent collector degrades silently — it never blocks or fails the boot.
func Init(ctx context.Context, serviceName string) (shutdown func(), err error) {
	endpoint, set := os.LookupEnv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if !set {
		endpoint = "localhost:4317"
	}
	if endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func() {}, nil
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(), // cluster-internal collector (dev compose / sidecar)
	)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		"",
		semconv.ServiceName(serviceName),
	))
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return func() {
		// Detached, bounded context: the flush must run even though the
		// process-level ctx is already cancelled at shutdown.
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		_ = tp.Shutdown(flushCtx)
	}, nil
}

// StartSpan opens an internal span on the engine's tracer, inheriting the
// caller's trace from ctx. Always pair with EndSpan (or span.End).
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(ctx, name, trace.WithAttributes(attrs...))
}

// EndSpan records err (if any) on the span, sets the span status, and ends
// it. Designed for the defer-with-named-error pattern:
//
//	defer func() { telemetry.EndSpan(span, err) }()
func EndSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}
