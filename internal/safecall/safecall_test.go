package safecall_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/deepworx/go-utils/internal/safecall"
)

var errFailed = errors.New("failed")

func TestCall(t *testing.T) {
	t.Parallel()

	late := func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}
	tests := []struct {
		name        string
		in          safecall.Input
		fn          func(ctx context.Context) error
		wantErr     error
		wantPanic   bool
		wantTimeout bool
	}{
		{name: "success", fn: func(context.Context) error { return nil }},
		{name: "error passes through", fn: func(context.Context) error { return errFailed }, wantErr: errFailed},
		{name: "panic becomes PanicError", fn: func(context.Context) error { panic("boom") }, wantPanic: true},
		{
			name:        "late success fails when strict",
			in:          safecall.Input{Timeout: time.Millisecond, LateSuccessFails: true},
			fn:          late,
			wantErr:     context.DeadlineExceeded,
			wantTimeout: true,
		},
		{name: "late success counts when lenient", in: safecall.Input{Timeout: time.Millisecond}, fn: late},
		{
			name: "no timeout leaves ctx open",
			fn: func(ctx context.Context) error {
				if _, ok := ctx.Deadline(); ok {
					return errFailed
				}
				return nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := safecall.Call(context.Background(), tt.in, tt.fn)
			var p *safecall.PanicError
			switch {
			case tt.wantPanic:
				if !errors.As(err, &p) || p.Value != "boom" || len(p.Stack) == 0 {
					t.Fatalf("Call = %v, want PanicError with value and stack", err)
				}
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Call = %v, want %v", err, tt.wantErr)
				}
			case err != nil:
				t.Fatalf("Call = %v, want nil", err)
			}
			if got := errors.Is(err, safecall.ErrTimeout); got != tt.wantTimeout {
				t.Errorf("errors.Is(err, ErrTimeout) = %v, want %v", got, tt.wantTimeout)
			}
		})
	}
}
