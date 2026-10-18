# How k6 talks to a Connect server

Research for [DWX-278](https://linear.app/deepworx/issue/DWX-278/how-k6-talks-to-a-connect-server)
(child of map DWX-276 "Event store load test via Connect and k6").
Researched 2026-10-18 against k6 **v2.3.0** (2026-09-21), connect-go **v1.21.1** (the version this
repo uses; v2.0.1 is the latest major), `connectrpc.com/grpcreflect` **v1.3.1** (v2.0.0 for
connect-go v2), `connectrpc.com/grpchealth` v1.5.0, Go 1.27.1.

## Question

What are the options for driving a connect-go server from k6, and what does each require?
Compared: (1) Connect protocol JSON over `k6/http`, (2) gRPC via `k6/net/grpc`, (3) xk6 extensions
for Connect. Covered: setup effort, per-RPC metrics and tags, k6-side overhead, Connect error
codes, keeping proto changes in sync with scripts.

## TL;DR

- **Recommendation: use `k6/net/grpc` against the connect-go handler, with `client.load()` of the
  repo's `.proto` files (or a `buf build` protoset).** Server reflection is a valid convenience for
  ad-hoc runs. It gives full-fidelity status codes in the `status` tag, low-cardinality
  `name`/`service`/`method` tags out of the box, client-side schema validation of request bodies
  (typos fail loudly), and binary protobuf on the wire, so the server is measured on its
  production codec. It needs zero server changes beyond h2c if the target runs without TLS.
- **connect-go's gRPC handler works with k6's gRPC client over plaintext**, verified end to end
  (k6 v2.3.0 + connect-go v1.21.1). The server **must** enable unencrypted HTTP/2
  (`http.Protocols.SetUnencryptedHTTP2(true)`); without it k6 fails at connect with
  `error reading server preface: ... frame header looked like an HTTP/1.1 header`.
- **Connect JSON over `k6/http` is the zero-dependency fallback.** It works over plain HTTP/1.1, the
  URLs are static per RPC (no cardinality problem), but HTTP status collapses Connect codes
  (`aborted` and `already_exists` are both 409; `invalid_argument`, `failed_precondition` and
  `out_of_range` are all 400), and connect-go's JSON codec silently discards unknown fields, so
  script/proto drift goes unnoticed. It also exercises `protojson` on the server, which is not
  what production gRPC/Connect-proto clients cost.
- **There is no maintained, k6-v2-compatible Connect extension.** The only one,
  `bumberboy/xk6-connectrpc` (v0.6.1, 6 stars, single maintainer), imports `go.k6.io/k6 v1.4.2`;
  k6 v2 changed the module path to `go.k6.io/k6/v2`, so it cannot be built into k6 v2 without a port.

## Option 1: Connect protocol JSON over `k6/http`

**How it works.** A Connect unary call is a plain `POST /<package>.<Service>/<Method>` with
`Content-Type: application/json` (or `application/proto`); `Connect-Protocol-Version: 1` is
recommended, `Connect-Timeout-Ms` optional
([Connect protocol](https://connectrpc.com/docs/protocol/)). "Bidirectional streaming requires
HTTP/2, but the other RPC types also support HTTP/1.1" (ibid.). connect-go only enforces the
version header with `WithRequireConnectProtocolHeader()` (connect-go v1.21.1 `option.go`).

```js
http.post(`${base}/grpc.health.v1.Health/Check`, JSON.stringify({ service: 'demo.v1.Demo' }),
  { headers: { 'Content-Type': 'application/json', 'Connect-Protocol-Version': '1' } });
```

**Setup effort.** Lowest: stock k6, no server changes, no proto artefacts. Streaming RPCs are
impractical (Connect streaming uses binary enveloping, `application/connect+json`).

**Transport.** k6/http builds an `http.Transport` with `ForceAttemptHTTP2 = true` and no
`Protocols`/h2c setting (k6 `internal/js/runner.go`), so HTTP/2 is only negotiated via TLS ALPN
([k6 HTTP/2 docs](https://grafana.com/docs/k6/latest/using-k6/protocols/http-2/)). Against a
plaintext server k6 uses HTTP/1.1 (verified: `res.proto === 'HTTP/1.1'`), i.e. one keep-alive
connection per VU. h2c in k6 only appears in the design doc of the future `k6/x/net/http` API
(`docs/design/018-new-http-api.md`).

**Metrics and tags.** Standard `http_req_*` metrics: `http_req_duration` (sending + waiting +
receiving), `http_req_waiting`, `http_req_failed`, `http_reqs`, etc.
([metrics reference](https://grafana.com/docs/k6/latest/using-k6/metrics/reference/)).
`name` defaults to the full URL. Because Connect paths are static (`/pkg.Svc/Method`, no path
params) the URL is low-cardinality by itself; still set `tags: { name: 'AppendEvents' }` to get
short names ([tags and groups](https://grafana.com/docs/k6/latest/using-k6/tags-and-groups/)).
Observed tags: `{method: POST, name: http://127.0.0.1:18080/grpc.health.v1.Health/Check,
proto: HTTP/1.1, status: 404, expected_response: true}`.

**Error codes.** connect-go maps codes to HTTP status per the spec (`connectCodeToHTTP` in
`protocol_connect.go`); the body is `{"code":"not_found","message":"..."}`. Consequences:
- `status` tag is lossy (409 = `aborted` **or** `already_exists`; 400 = three codes). For an event
  store, `aborted` (optimistic concurrency conflict) vs. `already_exists` (duplicate command ID)
  matters, so the script must parse `r.json('code')` and emit its own `Counter` with a `code` tag.
- `http_req_failed` is driven by `http.setResponseCallback(http.expectedStatuses(...))`; expected
  conflicts need whitelisting by HTTP status, which again cannot distinguish codes.
- Parsing the code requires the error body; with `discardResponseBodies: true` (recommended to cut
  memory/GC, [options](https://grafana.com/docs/k6/latest/using-k6/k6-options/reference/)) set
  `responseType: 'text'` on those requests.

**k6-side overhead.** Lowest of the three: `JSON.stringify` in JS, Go `net/http`, response body
copied into a JS string unless discarded. In a local smoke run (2 VUs, loopback) median
`http_req_duration` was ~150 µs vs. ~245 µs `grpc_req_duration`; not a benchmark, only an order
of magnitude.

**Server-side bias.** Requests are decoded with `protojson`, which is markedly more expensive than
binary protobuf. A load test via JSON measures the JSON codec, not what a gRPC or Connect-proto
client would cost. Sending `application/proto` from k6/http would need pre-encoded binary bodies.

**Proto sync.** None. URLs and JSON field names are hand-written strings. connect-go's JSON codec
uses `protojson.UnmarshalOptions{DiscardUnknown: true}` (v1.21.1 `codec.go`), so a renamed or
mistyped field is **silently dropped**. Verified: posting `{"servize": ...}` to `Health/Check`
returned 200 `SERVING` (empty service = overall health), i.e. a wrong test that looks green.

## Option 2: gRPC via `k6/net/grpc`

**How it works.** k6's gRPC client is grpc-go (v1.84.0 in k6 v2.3.0). Descriptors come from
`client.load(importPaths, ...protoFiles)` or `client.loadProtoset(path)` (both init context only),
or from server reflection with `client.connect(addr, { reflect: true })`
([gRPC protocol docs](https://grafana.com/docs/k6/latest/using-k6/protocols/grpc/),
[`Client.load`](https://grafana.com/docs/k6/latest/javascript-api/k6-net-grpc/client/client-load/),
[`Client.connect`](https://grafana.com/docs/k6/latest/javascript-api/k6-net-grpc/client/client-connect/)).
`connect()` must run in the VU context (once per VU, guarded by a flag). Unary, server, client and
bidi streaming are supported. Each VU holds one HTTP/2 connection.

**connect-go compatibility.** connect-go handlers "support the gRPC protocol by default"
([gRPC compatibility](https://connectrpc.com/docs/go/grpc-compatibility/)); no handler change.

**Plaintext / h2c.** With `plaintext: true` k6 uses `insecure.NewCredentials()` (k6
`internal/js/modules/k6/grpc/client.go`), i.e. HTTP/2 prior knowledge. The Go server must accept
h2c ([connect-go deployment](https://connectrpc.com/docs/go/deployment/)):

```go
p := new(http.Protocols)
p.SetHTTP1(true)
p.SetUnencryptedHTTP2(true)
srv := &http.Server{Addr: addr, Handler: mux, Protocols: p, ReadHeaderTimeout: 5 * time.Second}
```

HTTP/1 and h2c coexist on one port (Go detects the HTTP/2 preface), so k6/http and k6/net/grpc can
hit the same listener. With TLS, k6 sets ALPN `h2` and nothing else is needed.

**Reflection.** k6 uses `jhump/protoreflect`'s `grpcreflect.NewClientAuto` (k6
`internal/lib/netext/grpcext/reflect.go`), which tries `grpc.reflection.v1` and falls back to
`v1alpha`. For connect-go v1 mount both handlers from `connectrpc.com/grpcreflect` v1.3.1:

```go
reflector := grpcreflect.NewStaticReflector(eventstorev1connect.EventStoreName)
mux.Handle(grpcreflect.NewHandlerV1(reflector))
mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))
```

(For connect-go v2 the API is `grpcreflect.Register(server)` in `connectrpc.com/grpcreflect/v2`,
which serves both versions.) `reflectMetadata` passes e.g. auth headers to the reflection call.
Reflection is a new production dependency and exposes the schema, so gate it to test builds or
load-test deployments.

**Metrics and tags.** `grpc_req_duration` (Trend, "time to receive response") per unary call;
`grpc_streams`, `grpc_streams_msgs_sent/received` for streams
([metrics reference](https://grafana.com/docs/k6/latest/using-k6/metrics/reference/)). There is no
`grpc_req_failed` or connecting/waiting breakdown. Tags (k6 `params.go`, `conn.go`), observed:
`{name: /grpc.health.v1.Health/Check, service: grpc.health.v1.Health, method: Check,
status: "5", url: 127.0.0.1:18080/grpc.health.v1.Health/Check}`. `name` is the full method and
can be overridden via `params.tags.name`; all of them are per-RPC constants, so no cardinality
work is needed.

**Error codes.** Full fidelity: `res.status` is the gRPC code (`grpc.StatusAborted`,
`grpc.StatusAlreadyExists`, ...) and `res.error` carries `{code, message, details}`; the `status`
tag holds the numeric code, so thresholds like `grpc_req_duration{status:"10"}` work. Connect and
gRPC codes are the same set, so the server's `connect.NewError(connect.CodeAborted, ...)` surfaces
as 10. A failure-rate threshold needs a custom `Rate` (or `checks`), since there is no built-in
failed metric.

**k6-side overhead.** Higher than k6/http: JS object → JSON → `protojson.Unmarshal` into a
`dynamicpb.Message` → binary marshal, and the reverse for responses (`EmitUnpopulated: true`,
k6 `conn.go`). `discardResponseMessage: true` skips response decoding when only status matters.
This is the cost to watch for large `AppendEvents` batches; scale load generators accordingly.

**Proto sync.** `client.load()` points at the same `.proto` files the server is generated from (or
`buf build -o events.binpb` + `client.loadProtoset()`), so k6 sees schema changes at the next run.
Request bodies are validated client-side: an unknown field fails with
`unable to serialise request object to protocol buffer: ... unknown field "servize"` (verified),
and an unknown method fails at invoke. With `reflect: true` the schema comes from the deployed
server, so scripts can never be out of sync with the binary under test, only with what they send.

## Option 3: xk6 extensions for Connect

- The k6 extension registry lists no Connect extension (checked `registry.k6.io/registry.json`;
  only gRPC-adjacent built-ins).
- [`bumberboy/xk6-connectrpc`](https://github.com/bumberboy/xk6-connectrpc): Apache-2.0,
  v0.6.1 (2026-01-04), 6 stars, one human contributor plus Renovate, last code change 2026-02.
  Speaks Connect (JSON or proto), gRPC and gRPC-Web via connect-go v1.19.1, supports streaming,
  loads protos/protosets, ships `protoc-gen-k6-connectrpc` for typed JS clients, and emits its own
  metrics (`connectrpc_req_duration`, request/stream/connection counters, tags `method`,
  `service`, `procedure`, `protocol`, `content_type`).
- **Blocker:** it depends on `go.k6.io/k6 v1.4.2`. k6 v2.0.0 changed the module path to
  `go.k6.io/k6/v2` and states "all extensions must update their import paths to be compatible with
  v2" ([k6 v2.0.0 release notes](https://github.com/grafana/k6/releases/tag/v2.0.0)). Using it
  means pinning k6 v1.x or forking it, plus building a custom binary with `xk6` in CI.

## Comparison

| | k6/http + Connect JSON | k6/net/grpc | xk6-connectrpc |
|---|---|---|---|
| Setup | none | h2c on server (if no TLS); protos or reflection | custom binary; k6 v1 only |
| Server change | none | `SetUnencryptedHTTP2`; optional grpcreflect | as needed per protocol |
| Wire format measured | JSON (protojson) | binary proto | JSON or proto |
| Duration metric | `http_req_duration` (+ phases) | `grpc_req_duration` | `connectrpc_req_duration` |
| Per-RPC tag | `name` (set it; URL is static anyway) | `name` = `/pkg.Svc/Method`, `service`, `method` | `method`, `procedure` |
| Error code fidelity | lossy HTTP status; parse body | exact gRPC code in `status` tag | Connect code |
| Proto drift | silent (unknown fields dropped) | fails at k6 side | fails at k6 side |
| k6 overhead | lowest | JSON→dynamicpb→proto per call | similar to gRPC |
| Maintenance risk | none | none (core k6) | high |

## Verification

Local experiment (scratch, not committed): connect-go v1.21.1 server serving `grpchealth` and
`grpcreflect` v1.3.1 (V1 + V1Alpha) on `http.Server` with HTTP1 + UnencryptedHTTP2; k6 v2.3.0 built
from source.
- `k6/net/grpc`, `plaintext: true, reflect: true`: OK and NotFound (status 5) checks pass; tags as
  quoted above; misspelled field rejected by k6.
- Same server with `SetUnencryptedHTTP2(false)`: gRPC connect fails with "error reading server
  preface ... looked like an HTTP/1.1 header".
- `k6/http` Connect JSON: `proto=HTTP/1.1`; 404 with `{"code":"not_found",...}`; misspelled field
  returns 200.

## Open points for the map

- Whether the load-test deployment terminates TLS (then no h2c change needed) or runs plaintext.
- Whether reflection may ship in the service binary or only in a load-test build.
- Payload size of `AppendEvents` batches, which drives the k6-side dynamicpb cost and the
  number of load generators.
