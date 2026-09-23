package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Gavrielsh/True/internal/repository"
	errs "github.com/Gavrielsh/True/pkg/errors"
)

// fakeKYC implements repository.KYCEngine with injectable behaviour.
type fakeKYC struct {
	record func(context.Context, repository.KYCDecisionRequest) (repository.KYCDecisionResult, error)

	lastReq repository.KYCDecisionRequest
}

func (f *fakeKYC) RecordDecision(ctx context.Context, req repository.KYCDecisionRequest) (repository.KYCDecisionResult, error) {
	f.lastReq = req
	return f.record(ctx, req)
}

// kycRouter mounts the KYC handler directly (no operator-injection stub
// needed: Decision doesn't read OperatorCodeFromContext).
func kycRouter(k repository.KYCEngine) *gin.Engine {
	r := gin.New()
	h := NewKYCHandlers(k)
	g := r.Group("/api/v1")
	g.POST("/kyc/decision", h.Decision)
	return r
}

func TestKYCDecisionHandler_Verified_HappyPath(t *testing.T) {
	t.Parallel()
	playerID := uuid.New()
	eng := &fakeKYC{record: func(_ context.Context, req repository.KYCDecisionRequest) (repository.KYCDecisionResult, error) {
		if req.ExternalID != "ext-1" || req.EventID != "evt-1" || req.Decision != "VERIFIED" {
			t.Errorf("unexpected request: %+v", req)
		}
		return repository.KYCDecisionResult{
			PlayerID: playerID, EventID: "evt-1", Decision: "VERIFIED", Applied: true,
		}, nil
	}}
	r := kycRouter(eng)

	body := `{"external_id":"ext-1","event_id":"evt-1","decision":"VERIFIED","decided_at":"2026-01-01T00:00:00Z"}`
	w := doJSON(r, http.MethodPost, "/api/v1/kyc/decision", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp kycDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Result.Applied || resp.Result.PlayerID != playerID {
		t.Errorf("resp: got %+v", resp)
	}
}

func TestKYCDecisionHandler_StaleDecision_NotApplied(t *testing.T) {
	t.Parallel()
	eng := &fakeKYC{record: func(_ context.Context, _ repository.KYCDecisionRequest) (repository.KYCDecisionResult, error) {
		return repository.KYCDecisionResult{EventID: "evt-2", Decision: "VERIFIED", Applied: false}, nil
	}}
	r := kycRouter(eng)

	body := `{"external_id":"ext-1","event_id":"evt-2","decision":"VERIFIED","decided_at":"2020-01-01T00:00:00Z"}`
	w := doJSON(r, http.MethodPost, "/api/v1/kyc/decision", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (a stale decision is still audited, not an error); body=%s", w.Code, w.Body.String())
	}
	var resp kycDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Result.Applied {
		t.Error("Applied: got true, want false for a stale decision")
	}
}

func TestKYCDecisionHandler_BadBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{"missing_fields", `{}`},
		{"bad_decided_at", `{"external_id":"x","event_id":"e","decision":"VERIFIED","decided_at":"not-a-date"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := &fakeKYC{record: func(_ context.Context, _ repository.KYCDecisionRequest) (repository.KYCDecisionResult, error) {
				t.Error("engine must not be called on invalid body")
				return repository.KYCDecisionResult{}, nil
			}}
			w := doJSON(kycRouter(eng), http.MethodPost, "/api/v1/kyc/decision", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status: got %d want 400", w.Code)
			}
		})
	}
}

func TestKYCDecisionHandler_PlayerNotFoundMaps404(t *testing.T) {
	t.Parallel()
	eng := &fakeKYC{record: func(_ context.Context, _ repository.KYCDecisionRequest) (repository.KYCDecisionResult, error) {
		return repository.KYCDecisionResult{}, errs.ErrPlayerNotFound
	}}
	body := `{"external_id":"ghost","event_id":"evt-3","decision":"VERIFIED","decided_at":"2026-01-01T00:00:00Z"}`
	w := doJSON(kycRouter(eng), http.MethodPost, "/api/v1/kyc/decision", body)
	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d want 404", w.Code)
	}
}
