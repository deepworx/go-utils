package errors

import (
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

func TestMapErrorInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		opts      []Option
		wantInfos []*errdetails.ErrorInfo
	}{
		{
			name: "rule with reason attaches ErrorInfo with reason and domain",
			opts: []Option{
				WithDomain("tasks.example.com"),
				WithRules(Is(errMissing, connect.CodeNotFound).WithReason("TASK_NOT_FOUND")),
			},
			wantInfos: []*errdetails.ErrorInfo{{Reason: "TASK_NOT_FOUND", Domain: "tasks.example.com"}},
		},
		{
			name:      "rule with reason and no domain leaves domain empty",
			opts:      []Option{WithRules(Is(errMissing, connect.CodeNotFound).WithReason("TASK_NOT_FOUND"))},
			wantInfos: []*errdetails.ErrorInfo{{Reason: "TASK_NOT_FOUND"}},
		},
		{
			name: "rule without reason attaches no details",
			opts: []Option{
				WithDomain("tasks.example.com"),
				WithRules(Is(errMissing, connect.CodeNotFound)),
			},
		},
		{
			name: "reason that is not valid UTF-8 attaches no details",
			opts: []Option{WithRules(Is(errMissing, connect.CodeNotFound).WithReason("\xff"))},
		},
		{
			name: "reason of a rule that does not match is not attached",
			opts: []Option{
				WithDomain("tasks.example.com"),
				WithRules(
					As[*invalidFieldError](connect.CodeInvalidArgument).WithReason("INVALID_FIELD"),
					Is(errMissing, connect.CodeNotFound),
				),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Map(errMissing, tt.opts...)

			if got.Code() != connect.CodeNotFound {
				t.Errorf("code = %v, want %v", got.Code(), connect.CodeNotFound)
			}
			details := got.Details()
			if len(details) != len(tt.wantInfos) {
				t.Fatalf("got %d details, want %d", len(details), len(tt.wantInfos))
			}
			for i, detail := range details {
				msg, err := connectproto.UnmarshalErrorDetail(detail)
				if err != nil {
					t.Fatalf("unmarshal detail %d: %v", i, err)
				}
				info, ok := msg.(*errdetails.ErrorInfo)
				if !ok {
					t.Fatalf("detail %d is %T, want *errdetails.ErrorInfo", i, msg)
				}
				want := tt.wantInfos[i]
				if info.GetReason() != want.GetReason() || info.GetDomain() != want.GetDomain() {
					t.Errorf("ErrorInfo = {reason %q, domain %q}, want {reason %q, domain %q}",
						info.GetReason(), info.GetDomain(), want.GetReason(), want.GetDomain())
				}
				if len(info.GetMetadata()) != 0 {
					t.Errorf("metadata = %v, want none", info.GetMetadata())
				}
			}
		})
	}
}
