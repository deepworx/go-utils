package nats

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestPublish_NilConn(t *testing.T) {
	t.Parallel()

	err := Publish(context.Background(), nil, PublishInput{
		Subject: "test",
		Data:    []byte("data"),
	})

	if err == nil {
		t.Error("expected error with nil connection")
	}
}

func TestPublishInput_WithHeaders(t *testing.T) {
	t.Parallel()

	headers := make(nats.Header)
	headers.Set("custom", "value")

	in := PublishInput{
		Subject: "test.subject",
		Data:    []byte("test data"),
		Headers: headers,
	}

	if in.Headers.Get("custom") != "value" {
		t.Errorf("Headers.Get(custom) = %q, want %q", in.Headers.Get("custom"), "value")
	}
}

func TestJetStreamPublishInput_WithMsgID(t *testing.T) {
	t.Parallel()

	in := JetStreamPublishInput{
		Subject: "test.subject",
		Data:    []byte("test data"),
		MsgID:   "unique-id-123",
	}

	if in.MsgID != "unique-id-123" {
		t.Errorf("MsgID = %q, want %q", in.MsgID, "unique-id-123")
	}
}

func TestJetStreamPublish_NilJetStream(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic with nil JetStream")
		}
	}()

	_, _ = JetStreamPublish(context.Background(), nil, JetStreamPublishInput{
		Subject: "test",
		Data:    []byte("data"),
	})
}

func TestPullConsumeInput_Fields(t *testing.T) {
	t.Parallel()

	in := PullConsumeInput{
		Handler: func(_ context.Context, _ jetstream.Msg) error {
			return nil
		},
	}

	if in.Consumer != nil {
		t.Error("expected nil consumer")
	}

	if in.Handler == nil {
		t.Error("expected non-nil handler")
	}

	if len(in.Options) != 0 {
		t.Errorf("expected empty options, got %d", len(in.Options))
	}
}

func TestPushConsumeInput_Fields(t *testing.T) {
	t.Parallel()

	in := PushConsumeInput{
		Handler: func(_ context.Context, _ jetstream.Msg) error {
			return nil
		},
	}

	if in.Consumer != nil {
		t.Error("expected nil consumer")
	}

	if in.Handler == nil {
		t.Error("expected non-nil handler")
	}

	if len(in.Options) != 0 {
		t.Errorf("expected empty options, got %d", len(in.Options))
	}
}

func TestPullConsumeWithTracing_NilConsumer(t *testing.T) {
	t.Parallel()

	in := PullConsumeInput{
		Consumer: nil,
		Handler: func(_ context.Context, _ jetstream.Msg) error {
			return nil
		},
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic with nil consumer")
		}
	}()

	_, _ = PullConsumeWithTracing(context.Background(), in)
}

func TestPushConsumeWithTracing_NilConsumer(t *testing.T) {
	t.Parallel()

	in := PushConsumeInput{
		Consumer: nil,
		Handler: func(_ context.Context, _ jetstream.Msg) error {
			return nil
		},
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic with nil consumer")
		}
	}()

	_, _ = PushConsumeWithTracing(context.Background(), in)
}

func TestExtractTraceContext(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	extractedCtx := ExtractTraceContext(ctx, &mockJetStreamMsg{})

	if extractedCtx == nil {
		t.Error("ExtractTraceContext returned nil")
	}
}

func TestExtractTraceContext_WithHeaders(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	headers := make(nats.Header)
	headers.Set("traceparent", "00-abc123-def456-01")

	msg := &mockJetStreamMsg{headers: headers}
	extractedCtx := ExtractTraceContext(ctx, msg)

	if extractedCtx == nil {
		t.Error("ExtractTraceContext returned nil")
	}
}

// mockJetStreamMsg implements jetstream.Msg for testing.
type mockJetStreamMsg struct {
	headers nats.Header
}

func (m *mockJetStreamMsg) Subject() string { return "test.subject" }
func (m *mockJetStreamMsg) Headers() nats.Header {
	if m.headers == nil {
		return make(nats.Header)
	}
	return m.headers
}
func (m *mockJetStreamMsg) Data() []byte                       { return nil }
func (m *mockJetStreamMsg) Reply() string                      { return "" }
func (m *mockJetStreamMsg) Ack() error                         { return nil }
func (m *mockJetStreamMsg) Nak() error                         { return nil }
func (m *mockJetStreamMsg) NakWithDelay(_ time.Duration) error { return nil }
func (m *mockJetStreamMsg) InProgress() error                  { return nil }
func (m *mockJetStreamMsg) Term() error                        { return nil }
func (m *mockJetStreamMsg) TermWithReason(_ string) error      { return nil }
func (m *mockJetStreamMsg) DoubleAck(_ context.Context) error  { return nil }
func (m *mockJetStreamMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return nil, nil
}
