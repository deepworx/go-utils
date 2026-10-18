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
| ctxutil | `pkg/ctxutil` | Context helpers (request ID, claims, tenant, actor, correlation, causation) |
| shutdown | `pkg/shutdown` | Graceful shutdown orchestration |
| otel | `pkg/otel` | OpenTelemetry initialization |
| tracing | `pkg/tracing` | Manual span creation helpers |
| postgres | `pkg/postgres` | Database pool, transactions, and UnitOfWork |
| nats | `pkg/nats` | NATS/JetStream connection with tracing and metrics |
| grpchealth | `pkg/grpchealth` | gRPC health check aggregator |
| slogutil | `pkg/slogutil` | Global slog logger setup and ctx-correlated logs |
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

Event sourcing on Postgres (`pkg/eventsourcing/...`, `pkg/inbox`, `pkg/scheduler`): start with the [quickstart](docs/eventsourcing/quickstart.md).

## Documentation

### ctxutil

Store and retrieve request-scoped values: request ID and JWT claims (UserID, TenantID, Roles, Permissions).

```go
ctx = ctxutil.WithRequestID(ctx, "req-123")
ctx = ctxutil.WithClaims(ctx, ctxutil.Claims{UserID: "user-456", TenantID: "tenant-789"})

id, ok := ctxutil.RequestID(ctx)
userID, ok := ctxutil.UserID(ctx)
```

Event metadata (empty strings and `uuid.Nil` count as absent):

```go
ctx = ctxutil.WithTenantID(ctx, "tenant-789")      // takes precedence over Claims.TenantID
ctx = ctxutil.WithActor(ctx, "system:scheduler:daily-close")
ctx = ctxutil.WithCorrelationID(ctx, "corr-1")
ctx = ctxutil.WithCausationID(ctx, eventID)        // uuid.UUID of the causing event

tenant, ok := ctxutil.TenantID(ctx) // explicit value, then Claims.TenantID
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

- Histograms are exported as base-2 exponential (native) histograms. Set `Config.Histograms` to `otel.HistogramsExplicit` (`histograms: explicit`) for explicit buckets.
- `service.instance.id` is a random UUID per process; override it via `OTEL_RESOURCE_ATTRIBUTES=service.instance.id=...`.
- `Setup` wraps the `slog.Default()` handler in `slogutil.ContextHandler`, so `slog.InfoContext` etc. carry the ctx correlation fields.

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

// UnitOfWork keeps the transaction in ctx; nested Execute calls join it.
uow := postgres.NewUnitOfWork(pool, postgres.SetLocalTenant())
uow.Execute(ctx, func(ctx context.Context) error {
    // Conn returns the open transaction, or the pool outside one (sqlc DBTX).
    return db.New(postgres.Conn(ctx, pool)).SaveOrder(ctx, params)
})

// In-memory prototypes: Stage buffers writes until the transaction commits.
memUoW := postgres.NewInMemoryUnitOfWork()
memUoW.Execute(ctx, func(ctx context.Context) error {
    postgres.Stage(ctx, func() { orders[id] = order })
    return nil
})
```

### nats

NATS and JetStream connection with TLS/mTLS support, health checks, tracing, and metrics.

```go
conn, _ := nats.Connect(ctx, nats.Config{URL: "nats://localhost:4222"})

// Health check for grpchealth aggregator
checker := nats.NewHealthChecker(conn)
```

Publish with trace context:

```go
nats.Publish(ctx, conn, nats.PublishInput{
    Subject: "orders.created",
    Data:    payload,
})

// JetStream with deduplication
js, _ := jetstream.New(conn)
nats.JetStreamPublish(ctx, js, nats.JetStreamPublishInput{
    Subject: "orders.created",
    Data:    payload,
    MsgID:   "order-123", // optional deduplication
})
```

Consume with automatic trace extraction (pull and push):

```go
// Pull consumer
nats.PullConsumeWithTracing(ctx, nats.PullConsumeInput{
    Consumer: consumer,
    Handler: func(ctx context.Context, msg jetstream.Msg) error {
        // ctx contains extracted trace context
        return processOrder(ctx, msg)
    },
})

// Push consumer
nats.PushConsumeWithTracing(ctx, nats.PushConsumeInput{
    Consumer: pushConsumer,
    Handler: func(ctx context.Context, msg jetstream.Msg) error {
        return processOrder(ctx, msg)
    },
})
```

### grpchealth

Health check aggregator for [connectrpc.com/grpchealth/v2](https://pkg.go.dev/connectrpc.com/grpchealth/v2). Probes checkers in parallel, sets `StatusServing` only if all pass. Automatically registers with `shutdown` for graceful termination.

```go
aggregator := grpchealth.NewAggregator(ctx, grpchealth.DefaultConfig()).
    Register("postgres", postgres.NewHealthChecker(pool)).
    Register("redis", redisChecker)

aggregator.RegisterService(server) // server is a *connect.Server
// Background goroutine runs automatically, stopped by shutdown.WaitForSignal()
```

### slogutil

Configure the global slog logger with level and format.

```go
slogutil.Setup(slogutil.Config{Level: "debug", Format: "json"})
// or with defaults (level: info, format: text)
slogutil.Setup(slogutil.DefaultConfig())
```

`Setup` wraps the handler in `slogutil.ContextHandler`, which adds `request_id`,
`tenant_id`, `user_id`, `correlation_id`, `trace_id` and `span_id` from the ctx
to every record logged with a `*Context` method. Wrap a custom handler with
`slogutil.NewContextHandler(h)`.

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

server := connect.NewServer(jwtauth.NewInterceptor(auth))
servicepb.RegisterServiceHandler(server, &Server{})
connecthttp.Mount(mux, server)
```

Claims available via `ctxutil.UserID(ctx)`, `ctxutil.Roles(ctx)`, etc.

### connectrpc/recovery

Panic recovery interceptor. Catches panics, logs with stack trace, returns `CodeInternal`.

```go
server := connect.NewServer(
    recovery.NewInterceptor(),
    // ... other interceptors
)
servicepb.RegisterServiceHandler(server, &Server{})
connecthttp.Mount(mux, server)
```

Logs include: procedure, panic value, stack trace. Request fields come from `slogutil.ContextHandler`.

### connectrpc/logging

Structured request/response logging. Success at Info, errors at Warn.

```go
server := connect.NewServer(logging.NewInterceptor())
servicepb.RegisterServiceHandler(server, &Server{})
connecthttp.Mount(mux, server)
```

Log attributes: procedure, status, error (on failure). Request fields come from `slogutil.ContextHandler`.

### connectrpc/requestid

Propagates or generates request IDs. Stores in context via `ctxutil.WithRequestID`.

```go
server := connect.NewServer(requestid.NewInterceptor(requestid.Config{
    HeaderName: "X-Request-ID", // default
}))
servicepb.RegisterServiceHandler(server, &Server{})
connecthttp.Mount(mux, server)
```

Generated IDs are UUID v4 without hyphens (32 characters).

### connectrpc/errors

Maps errors to Connect RPC codes. Unmapped errors return `CodeInternal` with sanitized message.

```go
server := connect.NewServer(errors.NewInterceptor())
servicepb.RegisterServiceHandler(server, &Server{})
connecthttp.Mount(mux, server)
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

Mapped errors (1-4) preserve original message. Unmapped errors (5) hide details; the original error stays available server-side as the cause (`errors.Is`/`errors.As`).

### connectrpc/deadline

Enforces deadlines on unary calls; streaming calls pass through. Applies a default timeout when none exists, and caps existing deadlines to a maximum.

```go
server := connect.NewServer(deadline.NewInterceptor(deadline.Config{
    DefaultTimeout: 30 * time.Second, // applied when no deadline exists
    MaxTimeout:     60 * time.Second, // caps existing deadlines (0 = no cap)
}))
servicepb.RegisterServiceHandler(server, &Server{})
connecthttp.Mount(mux, server)
```

### connectrpc/interceptor

All `connectrpc` interceptors target [connect-go v2](https://pkg.go.dev/connectrpc.com/connect/v2) and are `connect.ServerInterceptor`s. connect-go v2 limits messages to 4 MiB by default; raise it with `connecthttp.WithReadMaxBytes` on `connecthttp.Mount`.

Default interceptor chain builder. Order: recovery → deadline → requestid → otel → logging → [jwtauth] → validate → errors.

```go
interceptors, _ := interceptor.BuildDefault()                      // 7 interceptors
interceptors, _ := interceptor.BuildDefaultWithAuth(auth)          // 8 interceptors (with JWT)
interceptors, _ := interceptor.BuildDefault(                       // with options
    interceptor.WithDeadline(deadline.Config{DefaultTimeout: 60 * time.Second}),
)

server := connect.NewServer(interceptors...)
servicepb.RegisterServiceHandler(server, &Server{})
connecthttp.Mount(mux, server)
```

### connectrpc/tracing (via otelconnect)

For OpenTelemetry tracing and metrics, use the official `otelconnect` library:

```bash
go get connectrpc.com/otelconnect
```

```go
import "connectrpc.com/otelconnect"

otelInterceptor, _ := otelconnect.NewServerInterceptor()

server := connect.NewServer(otelInterceptor)
servicepb.RegisterServiceHandler(server, &Server{})
connecthttp.Mount(mux, server)
```

Options:
- `WithTrustRemote()` - Trust incoming trace context (internal services)
- `WithServerPeerAttributes()` - Add peer address/port to server spans (off by default, high cardinality)

### connectrpc/validation (via validate)

For request validation using protovalidate rules, use the official `validate` library:

```bash
go get connectrpc.com/validate
```

```go
import "connectrpc.com/validate"

validateInterceptor := validate.NewServerInterceptor()

server := connect.NewServer(validateInterceptor)
servicepb.RegisterServiceHandler(server, &Server{})
connecthttp.Mount(mux, server)
```

Returns `CodeInvalidArgument` with field-level violation details on validation failure.
