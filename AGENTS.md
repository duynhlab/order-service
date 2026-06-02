# order-service

> AI Agent context for understanding this repository

## 📋 Overview

Order processing microservice. Handles order creation, tracking, and aggregated
order details with shipment info. It is a gRPC **client** to auth, shipping, and
notification, and a REST client to cart.

## 🔌 East-West Dependencies

gRPC is the official east-west transport. Clients are wired in `cmd/main.go` and
stored as package-level globals in `internal/web/v1` via `SetX` setters.

| Dependency | Transport | Method | Env var | When |
|------------|-----------|--------|---------|------|
| auth | gRPC | `auth.v1.AuthService/GetMe` | `AUTH_GRPC_ADDR` | JWT validation (shared `authmw`) |
| shipping | gRPC | `shipping.v1.ShippingService/GetShipmentByOrder` | `SHIPPING_GRPC_ADDR` | order-details aggregation |
| notification | gRPC | `notification.v1.NotificationService/SendEmail` | `NOTIFICATION_GRPC_ADDR` | best-effort on checkout |
| cart | REST | `GET`/`DELETE /cart/v1/private/cart` | `CART_SERVICE_URL` | read items on create, clear after |

- auth, shipping, notification clients dial at startup; a dial failure aborts
  startup. The notification **publish** at request time is best-effort and never
  fails the order. Cart calls forward the caller's `Authorization` header.

## 📈 Observability

- **Chain** (`cmd/main.go` `setupServer`): tracing → logging → metrics.
- **Tracing**: OpenTelemetry via `middleware.TracingMiddleware`.
- **Logging**: Zap; `middleware.LoggingMiddleware` derives the log `trace_id`
  from the active span using `obsx.TraceIDFromContext` (falls back to header/generated).
- **Metrics**: single `/metrics` endpoint. HTTP RED metrics from
  `middleware.PrometheusMiddleware`. `obsx.SetupMetrics()` (startup, when
  `METRICS_ENABLED=true`, **before** any `grpcx.Dial`) bridges gRPC client RED
  metrics (`rpc_client_*`) onto the same shared Prometheus registry — no separate
  port. Platform ServiceMonitor scrapes `/metrics`.
- **Profiling**: Pyroscope (`PROFILING_ENABLED`).

## 🏗️ Architecture Guidelines

### 3-Layer Architecture

| Layer | Location | Responsibility |
|-------|----------|----------------|
| **Web** | `internal/web/v1/` | HTTP, validation, **aggregation**, service clients |
| **Logic** | `internal/logic/v1/service.go` | Business rules (❌ NO SQL) |
| **Core** | `internal/core/` | Domain models, repositories, DB connection |

**Web layer files:** `handler.go` (order CRUD; `OrderHandler` holds the injected
`*logic.OrderService`), `aggregation.go` (`/orders/:id/details`), `cart_client.go`
(REST), `shipping_grpc_client.go`, `notification_client.go`, `validation.go`.

**Aggregation:** `/orders/:id/details` combines order + shipment, fetching the
shipment via the shipping **gRPC** client (`shipmentFetcher`). A missing shipment
is non-fatal (response omits it).

**Client wiring note:** the order service (`*logic.OrderService`) is
constructor-injected into `OrderHandler`. The shipping/notification/cart clients
are package-level globals set via `SetShippingClient` / `SetNotificationClient` /
`SetCartClient` (not handler fields). Match this pattern when adding clients.

### 3-Layer Coding Rules

**CRITICAL**: Strict layer boundaries. Violations will be rejected in code review.

#### Layer Boundaries

| Layer | Location | ALLOWED | FORBIDDEN |
|-------|----------|---------|-----------|
| **Web** | `internal/web/v1/` | HTTP handling, JSON binding, DTO mapping, call Logic, aggregation | SQL queries, direct DB access, business rules |
| **Logic** | `internal/logic/v1/` | Business rules, call repository interfaces, domain errors | SQL queries, `database.GetPool()`, HTTP handling, `*gin.Context` |
| **Core** | `internal/core/` | Domain models, repository implementations, SQL queries, DB connection | HTTP handling, business orchestration |

#### Dependency Direction

```
Web -> Logic -> Core (one-way only, never reverse)
```

- Web imports Logic and Core/domain
- Logic imports Core/domain and Core/repository interfaces
- Core imports nothing from Web or Logic

#### DO

- Put HTTP handlers, request validation, error-to-status mapping in `web/`
- Put business rules, orchestration, transaction logic in `logic/`
- Put SQL queries in `core/repository/` implementations
- Use repository interfaces (defined in `core/domain/`) for data access in Logic layer
- Use dependency injection (constructor parameters) for all service dependencies

#### DO NOT

- Write SQL or call `database.GetPool()` in Logic layer
- Import `gin` or handle HTTP in Logic layer
- Put business rules in Web layer (Web only translates and delegates)
- Call Logic functions directly from another service (cross-service calls go through Web-layer clients — gRPC for auth/shipping/notification, REST for cart)
- Skip the Logic layer (Web must not call Core/repository directly)

### Directory Structure

```
order-service/
├── cmd/main.go
├── config/config.go
├── db/migrations/sql/
├── internal/
│   ├── core/
│   │   ├── database.go
│   │   ├── domain/
│   │   └── repository/
│   ├── logic/v1/service.go
│   └── web/v1/
│       ├── handler.go
│       ├── aggregation.go
│       ├── cart_client.go
│       ├── shipping_grpc_client.go
│       ├── notification_client.go
│       └── validation.go
├── middleware/        # tracing, logging, prometheus, profiling, resource
└── Dockerfile
```

## 🛠️ Development Workflow

### Code Quality

**MANDATORY**: All code changes MUST pass lint before committing.

- Linter: `golangci-lint` v2+ with `.golangci.yml` config (60+ linters enabled)
- Zero tolerance: PRs with lint errors will NOT be merged
- CI enforces: `go-check` job runs lint on every PR

#### Commands (run in order)

```bash
go mod tidy              # Clean dependencies
go build ./...           # Verify compilation
go test ./...            # Run tests
golangci-lint run --timeout=10m  # Lint (MUST pass)
```

#### Pre-commit One-liner

```bash
go build ./... && go test ./... && golangci-lint run --timeout=10m
```

### Common Lint Fixes

- `perfsprint`: Use `errors.New()` instead of `fmt.Errorf()` when no format verbs
- `nosprintfhostport`: Use `net.JoinHostPort()` instead of `fmt.Sprintf("%s:%s", host, port)`
- `errcheck`: Always check error returns (or explicitly `_ = fn()`)
- `goconst`: Extract repeated string literals to constants
- `gocognit`: Extract helper functions to reduce complexity
- `noctx`: Use `http.NewRequestWithContext()` instead of `http.NewRequest()`

## 🔧 Tech Stack

| Component | Technology |
|-----------|------------|
| Runtime | Go 1.26 |
| Framework | Gin |
| Database | PostgreSQL 18 via pgx/v5 |
| Shared libs | `github.com/duynhlab/pkg` (`grpcx`, `authmw`, `obsx`, `proto/*`) |
| Tracing / Metrics / Profiling | OpenTelemetry / Prometheus / Pyroscope |

## 🏗️ Infrastructure Details

### Database

| Component | Value |
|-----------|-------|
| **Cluster** | transaction-db (CloudNativePG) |
| **PostgreSQL** | 18 |
| **HA** | 3 instances (1 primary + 2 replicas) |
| **Pooler** | PgCat HA (2 replicas) |
| **Endpoint** | `pgcat.cart.svc.cluster.local:5432` |
| **Database Name** | `order` (separate from `cart` database) |
| **Replication** | **Synchronous** (zero data loss) |
| **Shared Cluster** | Yes (with cart-service) |

**Query Routing (PgCat):**
- `SELECT` → `transaction-db-r` (replicas, load balanced)
- `INSERT/UPDATE/DELETE` → `transaction-db-rw` (primary)

### Graceful Shutdown

**VictoriaMetrics Pattern:**
1. `/ready` → 503 when shutting down
2. Drain delay (5s)
3. Sequential: HTTP → Database → Tracer

## 🔌 API Reference

All order routes are **private** — JWT middleware is applied at the `/order/v1/private` router group.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/order/v1/private/orders` | List user orders |
| `GET` | `/order/v1/private/orders/:id` | Get order by ID |
| `GET` | `/order/v1/private/orders/:id/details` | **Aggregated** order + shipment |
| `POST` | `/order/v1/private/orders` | Create new order |

The order-details aggregation fetches the shipment over **gRPC**
(`shipping.v1.ShippingService/GetShipmentByOrder`, target `SHIPPING_GRPC_ADDR`).
Order creation reads the user's cart over REST (`GET /cart/v1/private/cart`) for
authoritative items/pricing, then best-effort clears it (`DELETE /cart/v1/private/cart`)
and publishes an order-created notification over gRPC. Cart REST calls forward
the user's `Authorization` header. See the East-West Dependencies table above.

Full convention + inventory: [`homelab/docs/api/api-naming-convention.md`](https://github.com/duynhlab/homelab/blob/main/docs/api/api-naming-convention.md).
