// Package jwtauth provides JWT authentication for Connect RPC services.
package jwtauth

import "errors"

// Sentinel errors for JWT authentication.
var (
	// ErrMissingToken is returned when no Authorization header is present.
	ErrMissingToken = errors.New("missing authorization token")

	// ErrInvalidTokenFormat is returned when the token format is invalid.
	ErrInvalidTokenFormat = errors.New("invalid token format")

	// ErrTokenExpired is returned when the token has expired.
	ErrTokenExpired = errors.New("token has expired")

	// ErrTokenNotYetValid is returned when the token is not yet valid (nbf claim).
	ErrTokenNotYetValid = errors.New("token not yet valid")

	// ErrInvalidIssuer is returned when the issuer claim does not match.
	ErrInvalidIssuer = errors.New("invalid token issuer")

	// ErrInvalidAudience is returned when the audience claim does not match.
	ErrInvalidAudience = errors.New("invalid token audience")

	// ErrSignatureVerification is returned when signature verification fails.
	ErrSignatureVerification = errors.New("signature verification failed")

	// ErrJWKSFetch is returned when JWKS cannot be fetched.
	ErrJWKSFetch = errors.New("failed to fetch JWKS")

	// ErrJWKSURLRequired is returned when JWKSURL is empty.
	ErrJWKSURLRequired = errors.New("jwks_url is required")

	// ErrIssuerRequired is returned when Issuer is empty.
	ErrIssuerRequired = errors.New("issuer is required")

	// ErrAudienceRequired is returned when Audience is empty.
	ErrAudienceRequired = errors.New("audience is required")
)
