package cache

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Acquire must emit the redis.idempotency_acquire span carrying the
// idempotency key. NOT parallel: swaps the global tracer provider.
func TestAcquire_EmitsSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	r := NewRedis(client)
	status, _, err := r.Acquire(context.Background(), "PRAGMATIC:span-tx-1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if status != StatusAcquired {
		t.Fatalf("status: got %v want StatusAcquired", status)
	}

	var found bool
	for _, s := range rec.Ended() {
		if s.Name() != "redis.idempotency_acquire" {
			continue
		}
		found = true
		var key string
		for _, kv := range s.Attributes() {
			if kv.Key == attribute.Key("idempotency_key") {
				key = kv.Value.AsString()
			}
		}
		if key != "api:idempotency:PRAGMATIC:span-tx-1" {
			t.Errorf("idempotency_key attr: got %q", key)
		}
	}
	if !found {
		t.Error("redis.idempotency_acquire span not recorded")
	}
}
