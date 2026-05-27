package errors

import (
	"errors"
	"fmt"
	"testing"
)

func TestCodeFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want Code
	}{
		{"nil",                    nil,                                                CodeOK},
		{"insufficient_direct",    ErrInsufficientFunds,                               CodeInsufficientFunds},
		{"insufficient_wrapped",   fmt.Errorf("%w: %s", ErrInsufficientFunds, "ctx"),  CodeInsufficientFunds},
		{"invalid_amount",         ErrInvalidAmount,                                   CodeInvalidAmount},
		{"unsupported_currency",   ErrUnsupportedCurrency,                             CodeUnsupportedCurrency},
		{"player_not_found",       ErrPlayerNotFound,                                  CodePlayerNotFound},
		{"player_not_active",      ErrPlayerNotActive,                                 CodePlayerNotActive},
		{"transaction_pending",    ErrTransactionPending,                              CodeTransactionPending},
		{"transaction_conflict",   ErrTransactionConflict,                             CodeTransactionConflict},
		{"rollback_not_found",     ErrRollbackNotFound,                                CodeRollbackNotFound},
		{"rollback_already",       ErrRollbackAlready,                                 CodeRollbackAlready},
		{"unknown_falls_back",     errors.New("something exploded"),                   CodeInternal},
		{"double_wrap_preserves",  fmt.Errorf("layer2: %w", fmt.Errorf("layer1: %w", ErrInsufficientFunds)), CodeInsufficientFunds},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := CodeFor(tt.err); got != tt.want {
				t.Errorf("CodeFor(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
