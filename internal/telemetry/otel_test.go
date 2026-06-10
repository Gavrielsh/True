package telemetry

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// withRecorder swaps in an in-memory recording tracer provider for the test
// and restores the previous one afterwards. Tests using it must NOT be
// parallel with each other (global provider), so none of them call t.Parallel.
func withRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

func TestStartSpan_RecordsNameAndAttributes(t *testing.T) {
	rec := withRecorder(t)

	_, span := StartSpan(context.Background(), "db.bet_tx",
		attribute.String("player_id", "9a3a8e1c-0000-0000-0000-000000000001"))
	span.SetAttributes(attribute.String("ledger_transaction_id", "ltx-1"))
	EndSpan(span, nil)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans: got %d want 1", len(spans))
	}
	s := spans[0]
	if s.Name() != "db.bet_tx" {
		t.Errorf("name: got %q want db.bet_tx", s.Name())
	}
	attrs := map[attribute.Key]string{}
	for _, kv := range s.Attributes() {
		attrs[kv.Key] = kv.Value.AsString()
	}
	if attrs["player_id"] == "" {
		t.Error("player_id attribute missing")
	}
	if attrs["ledger_transaction_id"] != "ltx-1" {
		t.Errorf("ledger_transaction_id: got %q", attrs["ledger_transaction_id"])
	}
	if s.Status().Code != codes.Ok {
		t.Errorf("status: got %v want Ok", s.Status().Code)
	}
}

func TestEndSpan_RecordsError(t *testing.T) {
	rec := withRecorder(t)

	_, span := StartSpan(context.Background(), "http.bet")
	EndSpan(span, errors.New("insufficient funds"))

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans: got %d want 1", len(spans))
	}
	s := spans[0]
	if s.Status().Code != codes.Error {
		t.Errorf("status: got %v want Error", s.Status().Code)
	}
	if len(s.Events()) == 0 {
		t.Error("expected a recorded error event")
	}
}

// Child spans must join the parent trace — the ctx must flow, never be
// replaced with context.Background().
func TestStartSpan_PropagatesTrace(t *testing.T) {
	rec := withRecorder(t)

	ctx, parent := StartSpan(context.Background(), "http.bet")
	_, child := StartSpan(ctx, "db.bet_tx")
	EndSpan(child, nil)
	EndSpan(parent, nil)

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("spans: got %d want 2", len(spans))
	}
	// Ended() order: child first (ended first).
	if spans[0].SpanContext().TraceID() != spans[1].SpanContext().TraceID() {
		t.Error("child span did not join the parent trace")
	}
	if spans[0].Parent().SpanID() != spans[1].SpanContext().SpanID() {
		t.Error("child's parent is not the http.bet span")
	}
}

// An explicitly empty endpoint must install a no-op tracer and never error.
func TestInit_EmptyEndpointIsNoop(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	shutdown, err := Init(context.Background(), "true-engine-test")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown func must be non-nil")
	}
	shutdown() // must be safe to call

	_, span := StartSpan(context.Background(), "noop.check")
	if span.IsRecording() {
		t.Error("no-op tracer must not record spans")
	}
	span.End()
}
