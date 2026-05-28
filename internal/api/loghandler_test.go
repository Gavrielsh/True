package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestContextHandler_InjectsTraceAndOperator(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil)))

	ctx := withTraceID(context.Background(), "trace-xyz")
	ctx = withOperator(ctx, "PRAGMATIC")
	logger.InfoContext(ctx, "hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line not valid JSON: %v (%s)", err, buf.String())
	}
	if rec["trace_id"] != "trace-xyz" {
		t.Errorf("trace_id: got %v want trace-xyz", rec["trace_id"])
	}
	if rec["operator_code"] != "PRAGMATIC" {
		t.Errorf("operator_code: got %v want PRAGMATIC", rec["operator_code"])
	}
	if rec["msg"] != "hello" {
		t.Errorf("msg: got %v", rec["msg"])
	}
}

func TestContextHandler_NoContextValues_NoEmptyFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil)))

	logger.InfoContext(context.Background(), "no-ctx")

	out := buf.String()
	if strings.Contains(out, "trace_id") {
		t.Errorf("should not emit trace_id when absent: %s", out)
	}
	if strings.Contains(out, "operator_code") {
		t.Errorf("should not emit operator_code when absent: %s", out)
	}
}

func TestContextHandler_SurvivesWith(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	// .With() must not unwrap the decorator — trace_id should still appear.
	logger := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil))).
		With(slog.String("component", "test"))

	logger.InfoContext(withTraceID(context.Background(), "tid"), "x")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("bad json: %v (%s)", err, buf.String())
	}
	if rec["trace_id"] != "tid" {
		t.Errorf("trace_id lost after .With(): %s", buf.String())
	}
	if rec["component"] != "test" {
		t.Errorf("component attr missing: %s", buf.String())
	}
}
