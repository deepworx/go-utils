package jwtauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/deepworx/go-utils/pkg/ctxutil"
)

func TestConfig_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{
			name:    "valid config",
			cfg:     Config{JWKSURL: "https://example.com/jwks", Issuer: "iss", Audience: "aud"},
			wantErr: nil,
		},
		{
			name:    "missing JWKSURL",
			cfg:     Config{Issuer: "iss", Audience: "aud"},
			wantErr: ErrJWKSURLRequired,
		},
		{
			name:    "missing Issuer",
			cfg:     Config{JWKSURL: "https://example.com/jwks", Audience: "aud"},
			wantErr: ErrIssuerRequired,
		},
		{
			name:    "missing Audience",
			cfg:     Config{JWKSURL: "https://example.com/jwks", Issuer: "iss"},
			wantErr: ErrAudienceRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cfg.Validate()
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetNestedClaim(t *testing.T) {
	t.Parallel()

	tok, err := jwt.NewBuilder().
		Subject("user123").
		Claim("tenant_id", "tenant456").
		Claim("realm_access", map[string]any{
			"roles": []any{"admin", "user"},
		}).
		Claim("org", map[string]any{
			"team": map[string]any{
				"id": "team-1",
			},
		}).
		Build()
	if err != nil {
		t.Fatalf("failed to build token: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		wantVal any
		wantOK  bool
	}{
		{
			name:    "simple claim sub",
			path:    "sub",
			wantVal: "user123",
			wantOK:  true,
		},
		{
			name:    "simple custom claim",
			path:    "tenant_id",
			wantVal: "tenant456",
			wantOK:  true,
		},
		{
			name:    "nested claim one level",
			path:    "realm_access.roles",
			wantVal: []any{"admin", "user"},
			wantOK:  true,
		},
		{
			name:    "deeply nested claim",
			path:    "org.team.id",
			wantVal: "team-1",
			wantOK:  true,
		},
		{
			name:   "missing claim",
			path:   "nonexistent",
			wantOK: false,
		},
		{
			name:   "invalid nested path",
			path:   "tenant_id.invalid",
			wantOK: false,
		},
		{
			name:   "empty path",
			path:   "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := getNestedClaim(tok, tt.path)
			if ok != tt.wantOK {
				t.Errorf("getNestedClaim() ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantOK && !jsonEqual(got, tt.wantVal) {
				t.Errorf("getNestedClaim() = %v, want %v", got, tt.wantVal)
			}
		})
	}
}

func TestToStringSlice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   any
		want    []string
		wantErr bool
	}{
		{
			name:  "string slice",
			input: []string{"a", "b", "c"},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "any slice with strings",
			input: []any{"admin", "user"},
			want:  []string{"admin", "user"},
		},
		{
			name:  "space-separated string",
			input: "read write delete",
			want:  []string{"read", "write", "delete"},
		},
		{
			name:  "single word string",
			input: "admin",
			want:  []string{"admin"},
		},
		{
			name:  "empty string",
			input: "",
			want:  []string{},
		},
		{
			name:    "integer",
			input:   123,
			wantErr: true,
		},
		{
			name:    "nil",
			input:   nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := toStringSlice(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("toStringSlice() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !stringSliceEqual(got, tt.want) {
				t.Errorf("toStringSlice() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewAuthenticator_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{
			name:    "missing JWKSURL",
			cfg:     Config{Issuer: "iss", Audience: "aud"},
			wantErr: ErrJWKSURLRequired,
		},
		{
			name:    "missing Issuer",
			cfg:     Config{JWKSURL: "https://example.com/jwks", Audience: "aud"},
			wantErr: ErrIssuerRequired,
		},
		{
			name:    "missing Audience",
			cfg:     Config{JWKSURL: "https://example.com/jwks", Issuer: "iss"},
			wantErr: ErrAudienceRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewAuthenticator(context.Background(), tt.cfg)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestAuthenticator_Authenticate(t *testing.T) {
	t.Parallel()

	privKey, pubKey := generateTestKeys(t)
	srv := setupTestJWKSServer(t, pubKey)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	auth, err := NewAuthenticator(ctx, Config{
		JWKSURL:  srv.URL,
		Issuer:   "test-issuer",
		Audience: "test-audience",
		ClaimsMapping: &ClaimsMapping{
			UserID:      "sub",
			TenantID:    "tenant_id",
			Roles:       "roles",
			Permissions: "scope",
		},
		Leeway: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewAuthenticator() error = %v", err)
	}

	tests := []struct {
		name       string
		buildToken func() string
		wantClaims ctxutil.Claims
		wantErr    error
	}{
		{
			name: "valid token with all claims",
			buildToken: func() string {
				return signTestToken(t, privKey, map[string]any{
					"iss":       "test-issuer",
					"aud":       []string{"test-audience"},
					"sub":       "user-123",
					"tenant_id": "tenant-456",
					"roles":     []any{"admin", "user"},
					"scope":     "read write",
					"exp":       time.Now().Add(time.Hour).Unix(),
				})
			},
			wantClaims: ctxutil.Claims{
				UserID:      "user-123",
				TenantID:    "tenant-456",
				Roles:       []string{"admin", "user"},
				Permissions: []string{"read", "write"},
			},
		},
		{
			name: "valid token with minimal claims",
			buildToken: func() string {
				return signTestToken(t, privKey, map[string]any{
					"iss": "test-issuer",
					"aud": []string{"test-audience"},
					"sub": "user-minimal",
					"exp": time.Now().Add(time.Hour).Unix(),
				})
			},
			wantClaims: ctxutil.Claims{
				UserID: "user-minimal",
			},
		},
		{
			name: "expired token",
			buildToken: func() string {
				return signTestToken(t, privKey, map[string]any{
					"iss": "test-issuer",
					"aud": []string{"test-audience"},
					"sub": "user-123",
					"exp": time.Now().Add(-time.Hour).Unix(),
				})
			},
			wantErr: ErrTokenExpired,
		},
		{
			name: "token not yet valid",
			buildToken: func() string {
				return signTestToken(t, privKey, map[string]any{
					"iss": "test-issuer",
					"aud": []string{"test-audience"},
					"sub": "user-123",
					"nbf": time.Now().Add(time.Hour).Unix(),
					"exp": time.Now().Add(2 * time.Hour).Unix(),
				})
			},
			wantErr: ErrTokenNotYetValid,
		},
		{
			name: "invalid issuer",
			buildToken: func() string {
				return signTestToken(t, privKey, map[string]any{
					"iss": "wrong-issuer",
					"aud": []string{"test-audience"},
					"sub": "user-123",
					"exp": time.Now().Add(time.Hour).Unix(),
				})
			},
			wantErr: ErrInvalidIssuer,
		},
		{
			name: "invalid audience",
			buildToken: func() string {
				return signTestToken(t, privKey, map[string]any{
					"iss": "test-issuer",
					"aud": []string{"wrong-audience"},
					"sub": "user-123",
					"exp": time.Now().Add(time.Hour).Unix(),
				})
			},
			wantErr: ErrInvalidAudience,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			token := tt.buildToken()
			claims, err := auth.Authenticate(ctx, token)

			if tt.wantErr != nil {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("Authenticate() error = %v", err)
			}

			if claims.UserID != tt.wantClaims.UserID {
				t.Errorf("UserID = %q, want %q", claims.UserID, tt.wantClaims.UserID)
			}
			if claims.TenantID != tt.wantClaims.TenantID {
				t.Errorf("TenantID = %q, want %q", claims.TenantID, tt.wantClaims.TenantID)
			}
			if !stringSliceEqual(claims.Roles, tt.wantClaims.Roles) {
				t.Errorf("Roles = %v, want %v", claims.Roles, tt.wantClaims.Roles)
			}
			if !stringSliceEqual(claims.Permissions, tt.wantClaims.Permissions) {
				t.Errorf("Permissions = %v, want %v", claims.Permissions, tt.wantClaims.Permissions)
			}
		})
	}
}

func TestInterceptor_Authenticate(t *testing.T) {
	t.Parallel()

	privKey, pubKey := generateTestKeys(t)
	srv := setupTestJWKSServer(t, pubKey)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	auth, err := NewAuthenticator(ctx, Config{
		JWKSURL:  srv.URL,
		Issuer:   "test-issuer",
		Audience: "test-audience",
	})
	if err != nil {
		t.Fatalf("NewAuthenticator() error = %v", err)
	}

	i := NewInterceptor(auth).(*interceptor)

	tests := []struct {
		name       string
		authHeader string
		wantCode   connect.Code
		wantErr    error
	}{
		{
			name:       "missing authorization header",
			authHeader: "",
			wantCode:   connect.CodeUnauthenticated,
			wantErr:    ErrMissingToken,
		},
		{
			name:       "invalid format - no Bearer prefix",
			authHeader: "Basic abc123",
			wantCode:   connect.CodeUnauthenticated,
			wantErr:    ErrInvalidTokenFormat,
		},
		{
			name:       "invalid token",
			authHeader: "Bearer invalid.token.here",
			wantCode:   connect.CodeUnauthenticated,
		},
		{
			name: "valid token",
			authHeader: "Bearer " + signTestToken(t, privKey, map[string]any{
				"iss": "test-issuer",
				"aud": []string{"test-audience"},
				"sub": "user-123",
				"exp": time.Now().Add(time.Hour).Unix(),
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			headers := http.Header{}
			if tt.authHeader != "" {
				headers.Set("Authorization", tt.authHeader)
			}

			newCtx, err := i.authenticate(ctx, headers)

			if tt.wantErr != nil || tt.wantCode != 0 {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				var connectErr *connect.Error
				if !errors.As(err, &connectErr) {
					t.Fatalf("expected connect.Error, got %T", err)
				}
				if tt.wantCode != 0 && connectErr.Code() != tt.wantCode {
					t.Errorf("code = %v, want %v", connectErr.Code(), tt.wantCode)
				}
				if tt.wantErr != nil && !errors.Is(connectErr.Unwrap(), tt.wantErr) {
					t.Errorf("unwrapped error = %v, want %v", connectErr.Unwrap(), tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("authenticate() error = %v", err)
			}

			claims, ok := ctxutil.GetClaims(newCtx)
			if !ok {
				t.Fatal("claims not found in context")
			}
			if claims.UserID != "user-123" {
				t.Errorf("UserID = %q, want %q", claims.UserID, "user-123")
			}
		})
	}
}

func TestAuthenticator_InferAlgorithmFromKey(t *testing.T) {
	t.Parallel()

	privKey, pubKey := generateTestKeysWithoutAlg(t)
	srv := setupTestJWKSServer(t, pubKey)
	t.Cleanup(srv.Close)

	ctx := context.Background()

	t.Run("fails without InferAlgorithmFromKey", func(t *testing.T) {
		t.Parallel()

		auth, err := NewAuthenticator(ctx, Config{
			JWKSURL:               srv.URL,
			Issuer:                "test-issuer",
			Audience:              "test-audience",
			InferAlgorithmFromKey: false,
		})
		if err != nil {
			t.Fatalf("NewAuthenticator() error = %v", err)
		}

		token := signTestTokenWithKeyID(t, privKey, "test-key-id-no-alg", map[string]any{
			"iss": "test-issuer",
			"aud": []string{"test-audience"},
			"sub": "user-123",
			"exp": time.Now().Add(time.Hour).Unix(),
		})

		_, err = auth.Authenticate(ctx, token)
		if err == nil {
			t.Fatal("expected error when key has no alg and InferAlgorithmFromKey is false")
		}
	})

	t.Run("succeeds with InferAlgorithmFromKey", func(t *testing.T) {
		t.Parallel()

		auth, err := NewAuthenticator(ctx, Config{
			JWKSURL:               srv.URL,
			Issuer:                "test-issuer",
			Audience:              "test-audience",
			InferAlgorithmFromKey: true,
		})
		if err != nil {
			t.Fatalf("NewAuthenticator() error = %v", err)
		}

		token := signTestTokenWithKeyID(t, privKey, "test-key-id-no-alg", map[string]any{
			"iss": "test-issuer",
			"aud": []string{"test-audience"},
			"sub": "user-123",
			"exp": time.Now().Add(time.Hour).Unix(),
		})

		claims, err := auth.Authenticate(ctx, token)
		if err != nil {
			t.Fatalf("Authenticate() error = %v", err)
		}

		if claims.UserID != "user-123" {
			t.Errorf("UserID = %q, want %q", claims.UserID, "user-123")
		}
	})
}

func TestMapToConnectError(t *testing.T) {
	t.Parallel()

	i := &interceptor{}

	tests := []struct {
		name     string
		err      error
		wantCode connect.Code
	}{
		{
			name:     "token expired",
			err:      ErrTokenExpired,
			wantCode: connect.CodeUnauthenticated,
		},
		{
			name:     "token not yet valid",
			err:      ErrTokenNotYetValid,
			wantCode: connect.CodeUnauthenticated,
		},
		{
			name:     "invalid issuer",
			err:      ErrInvalidIssuer,
			wantCode: connect.CodeUnauthenticated,
		},
		{
			name:     "invalid audience",
			err:      ErrInvalidAudience,
			wantCode: connect.CodeUnauthenticated,
		},
		{
			name:     "signature verification failed",
			err:      ErrSignatureVerification,
			wantCode: connect.CodeUnauthenticated,
		},
		{
			name:     "JWKS fetch failed",
			err:      ErrJWKSFetch,
			wantCode: connect.CodeUnavailable,
		},
		{
			name:     "unknown error",
			err:      errors.New("unknown error"),
			wantCode: connect.CodeUnauthenticated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			connectErr := i.mapToConnectError(tt.err)
			if connectErr.Code() != tt.wantCode {
				t.Errorf("code = %v, want %v", connectErr.Code(), tt.wantCode)
			}
		})
	}
}

// Test helpers

func generateTestKeys(t *testing.T) (*rsa.PrivateKey, jwk.Key) {
	t.Helper()

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	pubJWK, err := jwk.Import(&privKey.PublicKey)
	if err != nil {
		t.Fatalf("failed to create JWK: %v", err)
	}
	if err := pubJWK.Set(jwk.KeyIDKey, "test-key-id"); err != nil {
		t.Fatalf("failed to set key ID: %v", err)
	}
	if err := pubJWK.Set(jwk.AlgorithmKey, jwa.RS256()); err != nil {
		t.Fatalf("failed to set algorithm: %v", err)
	}

	return privKey, pubJWK
}

func generateTestKeysWithoutAlg(t *testing.T) (*rsa.PrivateKey, jwk.Key) {
	t.Helper()

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	pubJWK, err := jwk.Import(&privKey.PublicKey)
	if err != nil {
		t.Fatalf("failed to create JWK: %v", err)
	}
	if err := pubJWK.Set(jwk.KeyIDKey, "test-key-id-no-alg"); err != nil {
		t.Fatalf("failed to set key ID: %v", err)
	}
	// Intentionally NOT setting algorithm to test InferAlgorithmFromKey

	return privKey, pubJWK
}

func setupTestJWKSServer(t *testing.T, pubKey jwk.Key) *httptest.Server {
	t.Helper()

	keyset := jwk.NewSet()
	if err := keyset.AddKey(pubKey); err != nil {
		t.Fatalf("failed to add key to set: %v", err)
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(keyset); err != nil {
			t.Errorf("failed to encode JWKS: %v", err)
		}
	}))
}

func signTestToken(t *testing.T, privKey *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	return signTestTokenWithKeyID(t, privKey, "test-key-id", claims)
}

func signTestTokenWithKeyID(t *testing.T, privKey *rsa.PrivateKey, keyID string, claims map[string]any) string {
	t.Helper()

	tok := jwt.New()
	for k, v := range claims {
		if err := tok.Set(k, v); err != nil {
			t.Fatalf("failed to set claim %s: %v", k, err)
		}
	}

	privJWK, err := jwk.Import(privKey)
	if err != nil {
		t.Fatalf("failed to import private key: %v", err)
	}
	if err := privJWK.Set(jwk.KeyIDKey, keyID); err != nil {
		t.Fatalf("failed to set key ID: %v", err)
	}

	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), privJWK))
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	return string(signed)
}

func jsonEqual(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
