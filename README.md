# order-service

Order processing microservice for creating and tracking orders.

## Features

- Order creation (items sourced from cart-service; server-side pricing)
- Idempotent order creation (`Idempotency-Key` header)
- Order status tracking
- Aggregated order details (order + shipment)
- Order history

## API Endpoints

All routes follow Variant A naming and require JWT (audience = `private`). See [homelab naming convention](https://github.com/duynhlab/homelab/blob/main/docs/api/api-naming-convention.md).

| Method | Path | Note |
|--------|------|------|
| `GET` | `/order/v1/private/orders` | List user orders |
| `GET` | `/order/v1/private/orders/:id` | Get order |
| `GET` | `/order/v1/private/orders/:id/details` | Aggregated with shipment (via shipping gRPC) |
| `POST` | `/order/v1/private/orders` | Create order from the user's cart |

JWT is validated by shared `authmw` middleware (`github.com/duynhlab/pkg/authmw`) on
the `/order/v1/private` router group; it calls `auth.v1.AuthService/GetMe` over gRPC.

`POST /orders` reads the caller's cart over REST as the authoritative item/price
source, persists the order, then best-effort clears the cart and publishes an
order-created notification (neither failure fails the order).

## East-West Dependencies

order-service is a gRPC **client** to two services and a REST client to one.
gRPC is the official east-west transport. JWT validation on private routes is
local-only (shared `authmw` against the auth JWKS) — no auth gRPC fallback.

| Dependency | Transport | Target env var | When |
|------------|-----------|----------------|------|
| shipping | gRPC (`shipping.v1.ShippingService/GetShipmentByOrder`) | `SHIPPING_GRPC_ADDR` | order-details aggregation |
| notification | gRPC (`notification.v1.NotificationService/SendEmail`) | `NOTIFICATION_GRPC_ADDR` | best-effort on checkout |
| cart | REST (`GET`/`DELETE /cart/v1/private/cart`) | `CART_SERVICE_URL` | read items on create, clear cart after |

Cart REST calls forward the caller's `Authorization` header.

## Observability

- **Tracing**: OpenTelemetry → OTel Collector (`middleware.TracingMiddleware`).
- **Logging**: structured Zap; logging middleware tags each line with the active
  span's trace ID via `obsx.TraceIDFromContext`, falling back to header/generated.
- **Metrics**: a single `/metrics` endpoint (Prometheus). HTTP RED metrics come
  from `middleware.PrometheusMiddleware` (`request_duration_seconds`, etc.).
  `obsx.SetupMetrics()` (called at startup when `METRICS_ENABLED=true`) bridges
  the gRPC client RED metrics (`rpc_client_*`) from the `grpcx` clients onto the
  **same** shared Prometheus registry — no separate port. The platform
  ServiceMonitor scrapes `/metrics`.
- **Profiling**: Pyroscope (`PROFILING_ENABLED`).

Middleware chain (in order): tracing → logging → metrics (Prometheus).

## Tech Stack

- Go 1.26 + Gin
- PostgreSQL 18 (transaction-db cluster, shared with cart) via pgx/v5
- PgCat connection pooling (transaction mode)
- gRPC clients via `github.com/duynhlab/pkg` (`grpcx`, `authmw`, `obsx`, `proto/*`)
- OpenTelemetry tracing, Prometheus metrics, Pyroscope profiling

## Configuration

Loaded by `config.Load()` from env (with `.env` fallback for local dev).

| Env var | Default | Purpose |
|---------|---------|---------|
| `SERVICE_NAME` | _(required)_ | Service identity (traces/profiling/logs) |
| `PORT` | `8080` | HTTP listen port |
| `AUTH_JWKS_URL` | `http://auth.auth.svc.cluster.local:8080/auth/v1/public/jwks` | Auth JWKS endpoint for local JWT verification |
| `SHIPPING_GRPC_ADDR` | _(empty)_ | Shipping gRPC target (dialed at startup) |
| `NOTIFICATION_GRPC_ADDR` | `dns:///notification.notification.svc.cluster.local:9090` | Notification gRPC target |
| `CART_SERVICE_URL` | `http://cart.cart.svc.cluster.local:8080` | Cart REST base URL |
| `DB_HOST` / `DB_PORT` / `DB_NAME` / `DB_USER` / `DB_PASSWORD` | — | PostgreSQL connection |
| `DB_SSLMODE` | `disable` | SSL mode |
| `DB_POOL_MAX_CONNECTIONS` | `25` | Max pool connections |
| `TRACING_ENABLED` / `OTEL_COLLECTOR_ENDPOINT` / `OTEL_SAMPLE_RATE` | `true` / collector / `0.1` | Tracing |
| `PROFILING_ENABLED` / `PYROSCOPE_ENDPOINT` | `true` / pyroscope | Profiling |
| `METRICS_ENABLED` / `METRICS_PATH` | `true` / `/metrics` | Metrics |
| `SHUTDOWN_TIMEOUT` | `10s` | Graceful shutdown timeout |
| `READINESS_DRAIN_DELAY` | `5s` (max 30s) | Readiness drain before shutdown |

## Development

### Prerequisites

- Go 1.26+
- [golangci-lint](https://golangci-lint.run/welcome/install/) v2+
- Docker (only for the integration tests — see [Testing](#testing))

### Local Development

```bash
# Install dependencies
go mod tidy
go mod download

# Build
go build ./...

# Unit tests (no Docker needed)
go test ./...

# Lint (must pass before PR merge)
golangci-lint run --timeout=10m

# Run locally (requires .env or env vars)
go run cmd/main.go
```

### Testing

Unit tests use the stdlib `testing` package with hand-written mocks and table-driven
subtests (no testify/gomock). The **repository layer** is covered by **integration tests**
against a real PostgreSQL via [testcontainers](https://golang.testcontainers.org/).

```bash
# Unit tests (no Docker)
go test ./...

# With coverage (as CI runs it)
go test -race -coverprofile=coverage.out ./...

# Integration tests — repository layer, real Postgres (needs a running Docker daemon)
go test -tags=integration ./internal/core/repository/...
```

Integration tests are build-tagged `//go:build integration`, so the default `go test ./...`
skips them and the service binary never links testcontainers. CI runs both jobs and merges
their coverage into SonarCloud (gate: ≥ 80% on new code).

### Pre-push Checklist

```bash
go build ./... && \
  go test ./... && \
  go test -tags=integration ./internal/core/repository/... && \
  golangci-lint run --timeout=10m
```

## License

MIT
