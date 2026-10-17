package ctxutil

import (
	"context"

	"github.com/google/uuid"
)

// WithTenantID returns a new context with the tenant ID set explicitly.
// An explicit tenant ID takes precedence over Claims.TenantID.
func WithTenantID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, tenantIDKey, id)
}

// TenantID returns the tenant ID set by WithTenantID, falling back to
// Claims.TenantID. Empty values count as absent.
func TenantID(ctx context.Context) (string, bool) {
	if id, ok := nonEmptyString(ctx, tenantIDKey); ok {
		return id, true
	}
	claims, _ := GetClaims(ctx)
	return claims.TenantID, claims.TenantID != ""
}

// WithActor returns a new context with the actor set: the user or
// component (system:<component>:<name>) on whose behalf work runs.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey, actor)
}

// Actor returns the actor set by WithActor. Claims are not consulted.
// An empty value counts as absent.
func Actor(ctx context.Context) (string, bool) {
	return nonEmptyString(ctx, actorKey)
}

// WithCorrelationID returns a new context with the correlation ID set,
// shared by everything one request, scheduled run or inbox message caused.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDKey, id)
}

// CorrelationID returns the correlation ID set by WithCorrelationID.
// The request ID is not consulted. An empty value counts as absent.
func CorrelationID(ctx context.Context) (string, bool) {
	return nonEmptyString(ctx, correlationIDKey)
}

// WithCausationID returns a new context with the causation ID set: the
// EventID of the event that caused the work running in this context.
func WithCausationID(ctx context.Context, eventID uuid.UUID) context.Context {
	return context.WithValue(ctx, causationIDKey, eventID)
}

// CausationID returns the causation EventID set by WithCausationID.
// uuid.Nil counts as absent.
func CausationID(ctx context.Context) (uuid.UUID, bool) {
	id, _ := ctx.Value(causationIDKey).(uuid.UUID)
	return id, id != uuid.Nil
}

func nonEmptyString(ctx context.Context, key ctxKey) (string, bool) {
	s, _ := ctx.Value(key).(string)
	return s, s != ""
}
