// Package ctxutil provides utility functions for storing and retrieving
// request-scoped values in context.Context.
package ctxutil

import "context"

// ctxKey is an unexported type for context keys to prevent collisions.
type ctxKey int

const (
	requestIDKey ctxKey = iota
	claimsKey
)

// Claims holds JWT-related identity information.
type Claims struct {
	UserID      string
	TenantID    string
	Roles       []string
	Permissions []string
}

// WithRequestID returns a new context with the request ID set.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request ID from the context.
func RequestID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey).(string)
	return id, ok
}

// WithClaims returns a new context with the claims set.
func WithClaims(ctx context.Context, claims Claims) context.Context {
	return context.WithValue(ctx, claimsKey, claims)
}

// GetClaims returns the claims from the context.
func GetClaims(ctx context.Context) (Claims, bool) {
	claims, ok := ctx.Value(claimsKey).(Claims)
	return claims, ok
}

// UserID returns the user ID from the context claims.
func UserID(ctx context.Context) (string, bool) {
	claims, ok := GetClaims(ctx)
	if !ok {
		return "", false
	}
	return claims.UserID, true
}

// TenantID returns the tenant ID from the context claims.
func TenantID(ctx context.Context) (string, bool) {
	claims, ok := GetClaims(ctx)
	if !ok {
		return "", false
	}
	return claims.TenantID, true
}

// Roles returns the roles from the context claims.
func Roles(ctx context.Context) ([]string, bool) {
	claims, ok := GetClaims(ctx)
	if !ok {
		return nil, false
	}
	return claims.Roles, true
}

// Permissions returns the permissions from the context claims.
func Permissions(ctx context.Context) ([]string, bool) {
	claims, ok := GetClaims(ctx)
	if !ok {
		return nil, false
	}
	return claims.Permissions, true
}
