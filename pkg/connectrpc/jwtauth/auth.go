package jwtauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/lestrrat-go/httprc/v3"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/deepworx/go-utils/pkg/ctxutil"
	"github.com/deepworx/go-utils/pkg/tracing"
)

// ClaimsMapping defines how JWT claims map to ctxutil.Claims fields.
// Use dot notation for nested claims (e.g., "realm_access.roles" for Keycloak).
type ClaimsMapping struct {
	// UserID is the JWT claim path for user ID (e.g., "sub", "user_id").
	UserID string

	// TenantID is the JWT claim path for tenant ID (e.g., "tenant_id", "org.id").
	TenantID string

	// Roles is the JWT claim path for roles (e.g., "roles", "realm_access.roles").
	Roles string

	// Permissions is the JWT claim path for permissions (e.g., "permissions", "scope").
	Permissions string
}

// Config holds configuration for the JWT authentication interceptor.
type Config struct {
	// JWKSURL is the URL to fetch JSON Web Key Set
	// (e.g., "https://idp.example.com/.well-known/jwks.json").
	// Required.
	JWKSURL string

	// Issuer is the expected "iss" claim value.
	// Required.
	Issuer string

	// Audience is the expected "aud" claim value.
	// Required.
	Audience string

	// ClaimsMapping defines how JWT claims map to application claims.
	// If nil, defaults are used: UserID="sub", others empty.
	ClaimsMapping *ClaimsMapping

	// HTTPTimeout is the timeout for JWKS fetch requests.
	// Defaults to 10 seconds if zero.
	HTTPTimeout time.Duration

	// Leeway allows clock skew tolerance for exp/nbf/iat validation.
	// Defaults to 1 minute if zero.
	Leeway time.Duration
}

// Authenticator validates JWT tokens and extracts claims.
type Authenticator struct {
	cache    *jwk.Cache
	jwksURL  string
	issuer   string
	audience string
	mapping  ClaimsMapping
	leeway   time.Duration
}

// NewAuthenticator creates a new JWT authenticator with the given configuration.
// The ctx controls the lifecycle of the background JWKS refresh goroutine.
// Returns error if required config fields are empty or if initial JWKS fetch fails.
func NewAuthenticator(ctx context.Context, cfg Config) (*Authenticator, error) {
	if cfg.JWKSURL == "" {
		return nil, fmt.Errorf("create authenticator: JWKSURL is required")
	}
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("create authenticator: Issuer is required")
	}
	if cfg.Audience == "" {
		return nil, fmt.Errorf("create authenticator: Audience is required")
	}

	httpTimeout := cfg.HTTPTimeout
	if httpTimeout == 0 {
		httpTimeout = 10 * time.Second
	}

	leeway := cfg.Leeway
	if leeway == 0 {
		leeway = time.Minute
	}

	mapping := ClaimsMapping{UserID: "sub"}
	if cfg.ClaimsMapping != nil {
		mapping = *cfg.ClaimsMapping
		if mapping.UserID == "" {
			mapping.UserID = "sub"
		}
	}

	httpClient := &http.Client{
		Timeout: httpTimeout,
	}

	cache, err := jwk.NewCache(ctx, httprc.NewClient(
		httprc.WithHTTPClient(httpClient),
	))
	if err != nil {
		return nil, fmt.Errorf("create jwk cache: %w", err)
	}

	if err := cache.Register(ctx, cfg.JWKSURL); err != nil {
		return nil, fmt.Errorf("register jwks url %s: %w", cfg.JWKSURL, err)
	}

	if _, err := cache.Lookup(ctx, cfg.JWKSURL); err != nil {
		return nil, fmt.Errorf("initial jwks fetch from %s: %w", cfg.JWKSURL, ErrJWKSFetch)
	}

	return &Authenticator{
		cache:    cache,
		jwksURL:  cfg.JWKSURL,
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		mapping:  mapping,
		leeway:   leeway,
	}, nil
}

// Authenticate validates the JWT token and returns extracted claims.
// Token should be the raw JWT string (without "Bearer " prefix).
func (a *Authenticator) Authenticate(ctx context.Context, token string) (ctxutil.Claims, error) {
	keyset, err := tracing.WithSpanResult(ctx, "jwtauth.lookup_jwks", func(ctx context.Context) (jwk.Set, error) {
		return a.cache.Lookup(ctx, a.jwksURL)
	})
	if err != nil {
		return ctxutil.Claims{}, fmt.Errorf("lookup jwks: %w", ErrJWKSFetch)
	}

	tok, err := tracing.WithSpanResult(ctx, "jwtauth.parse_token", func(ctx context.Context) (jwt.Token, error) {
		return jwt.Parse(
			[]byte(token),
			jwt.WithKeySet(keyset),
			jwt.WithValidate(true),
			jwt.WithIssuer(a.issuer),
			jwt.WithAudience(a.audience),
			jwt.WithAcceptableSkew(a.leeway),
		)
	})
	if err != nil {
		return ctxutil.Claims{}, a.mapJWTError(err)
	}

	return a.extractClaims(tok), nil
}

func (a *Authenticator) mapJWTError(err error) error {
	if errors.Is(err, jwt.TokenExpiredError()) {
		return fmt.Errorf("validate token: %w", ErrTokenExpired)
	}
	if errors.Is(err, jwt.TokenNotYetValidError()) {
		return fmt.Errorf("validate token: %w", ErrTokenNotYetValid)
	}
	if errors.Is(err, jwt.InvalidIssuerError()) {
		return fmt.Errorf("validate token: %w", ErrInvalidIssuer)
	}
	if errors.Is(err, jwt.InvalidAudienceError()) {
		return fmt.Errorf("validate token: %w", ErrInvalidAudience)
	}

	errStr := err.Error()
	if strings.Contains(errStr, "signature") || strings.Contains(errStr, "verify") {
		return fmt.Errorf("verify token: %w", ErrSignatureVerification)
	}

	return fmt.Errorf("parse token: %w", err)
}

func (a *Authenticator) extractClaims(tok jwt.Token) ctxutil.Claims {
	var claims ctxutil.Claims

	if a.mapping.UserID != "" {
		if v, ok := getNestedClaim(tok, a.mapping.UserID); ok {
			if s, ok := v.(string); ok {
				claims.UserID = s
			}
		}
	}

	if a.mapping.TenantID != "" {
		if v, ok := getNestedClaim(tok, a.mapping.TenantID); ok {
			if s, ok := v.(string); ok {
				claims.TenantID = s
			}
		}
	}

	if a.mapping.Roles != "" {
		if v, ok := getNestedClaim(tok, a.mapping.Roles); ok {
			if roles, err := toStringSlice(v); err == nil {
				claims.Roles = roles
			}
		}
	}

	if a.mapping.Permissions != "" {
		if v, ok := getNestedClaim(tok, a.mapping.Permissions); ok {
			if perms, err := toStringSlice(v); err == nil {
				claims.Permissions = perms
			}
		}
	}

	return claims
}

// getNestedClaim retrieves a claim value using dot notation path (e.g., "realm_access.roles").
func getNestedClaim(tok jwt.Token, path string) (any, bool) {
	if path == "" {
		return nil, false
	}

	parts := strings.Split(path, ".")

	var current any
	if err := tok.Get(parts[0], &current); err != nil {
		return nil, false
	}

	for _, part := range parts[1:] {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[part]
		if !ok {
			return nil, false
		}
	}

	return current, true
}

// toStringSlice converts a claim value to []string for roles/permissions.
func toStringSlice(v any) ([]string, error) {
	switch val := v.(type) {
	case []string:
		return val, nil
	case []any:
		result := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result, nil
	case string:
		return strings.Fields(val), nil
	default:
		return nil, fmt.Errorf("cannot convert %T to []string", v)
	}
}

// NewInterceptor creates a Connect RPC interceptor that validates JWT tokens.
// It extracts the token from the Authorization header, validates it, and injects
// claims into the request context using ctxutil.WithClaims.
func NewInterceptor(auth *Authenticator) connect.Interceptor {
	return &interceptor{auth: auth}
}

type interceptor struct {
	auth *Authenticator
}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}

		ctx, err := i.authenticate(ctx, req.Header())
		if err != nil {
			return nil, err
		}

		return next(ctx, req)
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := i.authenticate(ctx, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

func (i *interceptor) authenticate(ctx context.Context, headers http.Header) (context.Context, error) {
	authHeader := headers.Get("Authorization")
	if authHeader == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, ErrMissingToken)
	}

	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(authHeader, bearerPrefix) {
		return nil, connect.NewError(connect.CodeUnauthenticated, ErrInvalidTokenFormat)
	}
	token := strings.TrimPrefix(authHeader, bearerPrefix)

	claims, err := i.auth.Authenticate(ctx, token)
	if err != nil {
		return nil, i.mapToConnectError(err)
	}

	return ctxutil.WithClaims(ctx, claims), nil
}

func (i *interceptor) mapToConnectError(err error) *connect.Error {
	switch {
	case errors.Is(err, ErrTokenExpired),
		errors.Is(err, ErrTokenNotYetValid),
		errors.Is(err, ErrInvalidIssuer),
		errors.Is(err, ErrInvalidAudience),
		errors.Is(err, ErrSignatureVerification):
		return connect.NewError(connect.CodeUnauthenticated, err)
	case errors.Is(err, ErrJWKSFetch):
		return connect.NewError(connect.CodeUnavailable, err)
	default:
		return connect.NewError(connect.CodeUnauthenticated, err)
	}
}
