# go-utils

`go get github.com/deepworx/go-utils`

Shared Go utility library for services.

## Installation

```bash
go get github.com/deepworx/go-utils
```

## Packages

| Package | Import | Description |
|---------|--------|-------------|
| ctxutil | `pkg/ctxutil` | Context helpers (request ID, user claims) |
| shutdown | `pkg/shutdown` | Graceful shutdown orchestration |
| otel | `pkg/otel` | OpenTelemetry initialization |
| tracing | `pkg/tracing` | Manual span creation helpers |
| postgres | `pkg/postgres` | Database pool with tracing and health check |
| grpchealth | `pkg/grpchealth` | gRPC health check aggregator |
| slogutil | `pkg/slogutil` | Global slog logger setup |
| koanfutil | `pkg/koanfutil` | Koanf configuration helpers |
| jwtauth | `pkg/connectrpc/jwtauth` | JWT authentication interceptor |
| recovery | `pkg/connectrpc/recovery` | Panic recovery interceptor |
| logging | `pkg/connectrpc/logging` | Request/response logging interceptor |
| requestid | `pkg/connectrpc/requestid` | Request ID propagation interceptor |
| errors | `pkg/connectrpc/errors` | Error mapping interceptor |
| deadline | `pkg/connectrpc/deadline` | Deadline enforcement interceptor |
| interceptor | `pkg/connectrpc/interceptor` | Default interceptor chain builder |
| otelconnect | `connectrpc.com/otelconnect` | OpenTelemetry tracing/metrics (external) |
| validate | `connectrpc.com/validate` | Request validation with protovalidate (external) |

## Documentation

### ctxutil

Store and retrieve request-scoped values: request ID and JWT claims (UserID, TenantID, Roles, Permissions).

```go
ctx = ctxutil.WithRequestID(ctx, "req-123")
ctx = ctxutil.WithClaims(ctx, ctxutil.Claims{UserID: "user-456", TenantID: "tenant-789"})

id, ok := ctxutil.RequestID(ctx)
userID, ok := ctxutil.UserID(ctx)
```

### shutdown

Graceful shutdown orchestration with LIFO handler execution and OS signal support.

```go
shutdown.Register(otel.Shutdown)  // called 3rd
shutdown.Register(server.Stop)    // called 2nd
shutdown.Register(db.Close)       // called 1st (LIFO)

shutdown.WaitForSignal(ctx) // blocks until SIGINT/SIGTERM, 30s timeout for handlers
```

Custom timeout:

```go
shutdown.WaitForSignalWithTimeout(ctx, 60*time.Second) // 60s for handlers to complete
```

Default: `shutdown.DefaultShutdownTimeout` (30s)

### otel

OpenTelemetry setup for tracing, metrics, and logging. Configurable via `OTEL_*` env vars.

```go
otel.Setup(ctx, otel.Config{ServiceName: "my-service", ServiceVersion: "1.0.0"})
// Shutdown is automatically registered with the shutdown orchestrator
```

Env vars: `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER` (`otlp|console|none`)

### tracing

Span wrapper with automatic error recording. Works without TracerProvider (no-op).

```go
err := tracing.WithSpan(ctx, "operation", func(ctx context.Context) error {
    return doWork(ctx)
})

result, err := tracing.WithSpanResult(ctx, "fetch", func(ctx context.Context) (User, error) {
    return fetchUser(ctx)
})
```

### postgres

Database pool with OpenTelemetry tracing and health checking.

```go
pool, _ := postgres.NewPool(ctx, postgres.Config{DSN: "postgres://localhost/db"})

// Health check for grpchealth aggregator
checker := postgres.NewHealthChecker(pool)
```

### grpchealth

Health check aggregator for [connectrpc.com/grpchealth](https://pkg.go.dev/connectrpc.com/grpchealth). Probes checkers in parallel, sets `StatusServing` only if all pass. Automatically registers with `shutdown` for graceful termination.

```go
aggregator := grpchealth.NewAggregator(ctx, grpchealth.DefaultConfig()).
    Register("postgres", postgres.NewHealthChecker(pool)).
    Register("redis", redisChecker)

mux.Handle(aggregator.Handler())
// Background goroutine runs automatically, stopped by shutdown.WaitForSignal()
```

### slogutil

Configure the global slog logger with level and format.

```go
slogutil.Setup(slogutil.Config{Level: "debug", Format: "json"})
// or with defaults (level: info, format: text)
slogutil.Setup(slogutil.DefaultConfig())
```

### koanfutil

Helpers for [koanf](https://github.com/knadh/koanf) configuration loading.

```go
k := koanf.New(".")
k.Load(koanfutil.WithDefaults(postgres.DefaultConfig()), nil) // load defaults
k.Load(file.Provider("config.toml"), toml.Parser())           // override with file
k.Load(koanfutil.FileResolver(k), nil)                        // resolve file:// URIs
```

- `WithDefaults(T)` - Load struct as default values (uses `koanf` tags)
- `FileResolver(k)` - Resolve `file:///path` URIs to file contents

All packages provide `DefaultConfig()` with sensible defaults (see godoc).

### connectrpc/jwtauth

JWT authentication interceptor with JWKS support. IDP-independent, configurable claims mapping.

```go
auth, _ := jwtauth.NewAuthenticator(ctx, jwtauth.Config{
    JWKSURL:  "https://auth.example.com/.well-known/jwks.json",
    Issuer:   "https://auth.example.com",
    Audience: "my-api",
    ClaimsMapping: &jwtauth.ClaimsMapping{
        UserID:   "sub",
        TenantID: "tenant_id",
        Roles:    "realm_access.roles",
    },
})

mux.Handle(servicepb.NewServiceHandler(
    &Server{},
    connect.WithInterceptors(jwtauth.NewInterceptor(auth)),
))
```

Claims available via `ctxutil.UserID(ctx)`, `ctxutil.Roles(ctx)`, etc.

### connectrpc/recovery

Panic recovery interceptor. Catches panics, logs with stack trace, returns `CodeInternal`.

```go
mux.Handle(servicepb.NewServiceHandler(
    &Server{},
    connect.WithInterceptors(
        recovery.NewInterceptor(),
        // ... other interceptors
    ),
))
```

Logs include: procedure, panic value, stack trace, request_id (if present).

### connectrpc/logging

Structured request/response logging. Success at Info, errors at Warn.

```go
mux.Handle(servicepb.NewServiceHandler(
    &Server{},
    connect.WithInterceptors(logging.NewInterceptor()),
))
```

Log attributes: procedure, status, request_id, user_id, error (on failure).

### connectrpc/requestid

Propagates or generates request IDs. Stores in context via `ctxutil.WithRequestID`.

```go
mux.Handle(servicepb.NewServiceHandler(
    &Server{},
    connect.WithInterceptors(requestid.NewInterceptor(requestid.Config{
        HeaderName: "X-Request-ID", // default
    })),
))
```

Generated IDs are UUID v4 without hyphens (32 characters).

### connectrpc/errors

Maps errors to Connect RPC codes. Unmapped errors return `CodeInternal` with sanitized message.

```go
mux.Handle(servicepb.NewServiceHandler(
    &Server{},
    connect.WithInterceptors(errors.NewInterceptor()),
))
```

Implement `ConnectCoder` on errors:

```go
type ErrNotFound struct{ ID string }

func (e *ErrNotFound) Error() string {
    return fmt.Sprintf("resource %s not found", e.ID)
}

func (e *ErrNotFound) ConnectCode() connect.Code {
    return connect.CodeNotFound
}
```

Error mapping priority:
1. `context.Canceled` → `CodeCanceled`
2. `context.DeadlineExceeded` → `CodeDeadlineExceeded`
3. `ConnectCoder` interface → custom code
4. `*connect.Error` → preserved
5. Other errors → `CodeInternal` (message: "internal error")

Mapped errors (1-4) preserve original message. Unmapped errors (5) hide details.

### connectrpc/deadline

Enforces deadlines on server-side unary calls. Applies a default timeout when none exists, and caps existing deadlines to a maximum.

```go
mux.Handle(servicepb.NewServiceHandler(
    &Server{},
    connect.WithInterceptors(deadline.NewInterceptor(deadline.Config{
        DefaultTimeout: 30 * time.Second, // applied when no deadline exists
        MaxTimeout:     60 * time.Second, // caps existing deadlines (0 = no cap)
    })),
))
```

### connectrpc/interceptor

Default interceptor chain builder. Order: recovery → deadline → requestid → otel → logging → [jwtauth] → validate → errors.

```go
interceptors, _ := interceptor.BuildDefault()                      // 7 interceptors
interceptors, _ := interceptor.BuildDefaultWithAuth(auth)          // 8 interceptors (with JWT)
interceptors, _ := interceptor.BuildDefault(                       // with options
    interceptor.WithDeadline(deadline.Config{DefaultTimeout: 60 * time.Second}),
)
```

### connectrpc/tracing (via otelconnect)

For OpenTelemetry tracing and metrics, use the official `otelconnect` library:

```bash
go get connectrpc.com/otelconnect
```

```go
import "connectrpc.com/otelconnect"

otelInterceptor, _ := otelconnect.NewInterceptor()

mux.Handle(servicepb.NewServiceHandler(
    &Server{},
    connect.WithInterceptors(otelInterceptor),
))
```

Options:
- `WithTrustRemote()` - Trust incoming trace context (internal services)
- `WithoutServerPeerAttributes()` - Reduce metric cardinality

### connectrpc/validation (via validate)

For request validation using protovalidate rules, use the official `validate` library:

```bash
go get connectrpc.com/validate
```

```go
import "connectrpc.com/validate"

validateInterceptor, _ := validate.NewInterceptor()

mux.Handle(servicepb.NewServiceHandler(
    &Server{},
    connect.WithInterceptors(validateInterceptor),
))
```

Returns `CodeInvalidArgument` with field-level violation details on validation failure.
