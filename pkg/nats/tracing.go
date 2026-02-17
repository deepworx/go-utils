package nats

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

const tracerName = "github.com/deepworx/go-utils/pkg/nats"

// PublishInput holds parameters for Publish operations.
type PublishInput struct {
	Subject string
	Data    []byte
	Headers nats.Header
}

// JetStreamPublishInput holds parameters for JetStream publish operations.
type JetStreamPublishInput struct {
	Subject string
	Data    []byte
	Headers nats.Header
	MsgID   string // optional, for deduplication
}

// PullConsumeInput holds parameters for pull consumer operations with tracing.
type PullConsumeInput struct {
	Consumer jetstream.Consumer
	Handler  func(ctx context.Context, msg jetstream.Msg) error
	Options  []jetstream.PullConsumeOpt
}

// PushConsumeInput holds parameters for push consumer operations with tracing.
type PushConsumeInput struct {
	Consumer jetstream.PushConsumer
	Handler  func(ctx context.Context, msg jetstream.Msg) error
	Options  []jetstream.PushConsumeOpt
}

// headerCarrier adapts nats.Header to propagation.TextMapCarrier.
type headerCarrier nats.Header

func (c headerCarrier) Get(key string) string {
	return nats.Header(c).Get(key)
}

func (c headerCarrier) Set(key, value string) {
	nats.Header(c).Set(key, value)
}

func (c headerCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// Publish sends a message with trace context injection.
func Publish(ctx context.Context, conn *nats.Conn, in PublishInput) error {
	msg := &nats.Msg{
		Subject: in.Subject,
		Data:    in.Data,
		Header:  in.Headers,
	}
	if msg.Header == nil {
		msg.Header = make(nats.Header)
	}
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier(msg.Header))

	if err := conn.PublishMsg(msg); err != nil {
		return fmt.Errorf("publish to %s: %w", in.Subject, err)
	}
	return nil
}

// JetStreamPublish sends a message to JetStream with trace context injection.
func JetStreamPublish(ctx context.Context, js jetstream.JetStream, in JetStreamPublishInput) (*jetstream.PubAck, error) {
	msg := &nats.Msg{
		Subject: in.Subject,
		Data:    in.Data,
		Header:  in.Headers,
	}
	if msg.Header == nil {
		msg.Header = make(nats.Header)
	}
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier(msg.Header))

	var opts []jetstream.PublishOpt
	if in.MsgID != "" {
		opts = append(opts, jetstream.WithMsgID(in.MsgID))
	}

	ack, err := js.PublishMsg(ctx, msg, opts...)
	if err != nil {
		return nil, fmt.Errorf("jetstream publish to %s: %w", in.Subject, err)
	}
	return ack, nil
}

// PullConsumeWithTracing starts a pull consumer with automatic trace extraction.
// Each message handler receives a context with the extracted trace context.
func PullConsumeWithTracing(ctx context.Context, in PullConsumeInput) (jetstream.ConsumeContext, error) {
	consCtx, err := in.Consumer.Consume(wrapHandlerWithTracing(ctx, in.Handler), in.Options...)
	if err != nil {
		return nil, fmt.Errorf("start pull consumer: %w", err)
	}
	return consCtx, nil
}

// PushConsumeWithTracing starts a push consumer with automatic trace extraction.
// Each message handler receives a context with the extracted trace context.
func PushConsumeWithTracing(ctx context.Context, in PushConsumeInput) (jetstream.ConsumeContext, error) {
	consCtx, err := in.Consumer.Consume(wrapHandlerWithTracing(ctx, in.Handler), in.Options...)
	if err != nil {
		return nil, fmt.Errorf("start push consumer: %w", err)
	}
	return consCtx, nil
}

func wrapHandlerWithTracing(ctx context.Context, handler func(context.Context, jetstream.Msg) error) jetstream.MessageHandler {
	tracer := otel.Tracer(tracerName)
	propagator := otel.GetTextMapPropagator()

	return func(msg jetstream.Msg) {
		msgCtx := propagator.Extract(ctx, headerCarrier(msg.Headers()))
		msgCtx, span := tracer.Start(msgCtx, "consume "+msg.Subject())
		defer span.End()

		if err := handler(msgCtx, msg); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
	}
}

// ExtractTraceContext extracts trace context from a JetStream message.
// Use this when you need manual control over span creation.
func ExtractTraceContext(ctx context.Context, msg jetstream.Msg) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, headerCarrier(msg.Headers()))
}
