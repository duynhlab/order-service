# order-service

> AI Agent context for understanding this repository

## 📋 Overview

Order processing microservice. Handles order creation, tracking, and aggregated order details with shipment info.

## 🏗️ Architecture Guidelines

### 3-Layer Architecture

| Layer | Location | Responsibility |
|-------|----------|----------------|
| **Web** | `internal/web/v1/handler.go` | HTTP, validation, **aggregation** |
| **Logic** | `internal/logic/v1/service.go` | Business rules (❌ NO SQL) |
| **Core** | `internal/core/` | Domain models, repositories |

**Aggregation:** `/orders/:id/details` combines order + shipment (HTTP call to shipping-service).

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
- Call Logic functions directly from another service (use HTTP aggregation in Web layer)
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
│   │   └── domain/
│   ├── logic/v1/service.go
│   └── web/v1/handler.go
├── middleware/
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
| Framework | Gin |
| Database | PostgreSQL 18 via pgx/v5 |
| Tracing | OpenTelemetry |

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

### Cluster paths (what this service mounts)

All order routes are **private** — the service applies JWT middleware at the `/api/v1` group level.

| Method | Cluster path | Audience | Description |
|--------|--------------|----------|-------------|
| `GET` | `/api/v1/orders` | private | List user orders |
| `GET` | `/api/v1/orders/:id` | private | Get order by ID |
| `GET` | `/api/v1/orders/:id/details` | private | **Aggregated** order + shipment |
| `POST` | `/api/v1/orders` | private | Create new order |

### Edge paths (what the browser sends)

Kong in the `order` namespace rewrites `/order/v1/private/orders/...` → `/api/v1/orders/...`.

| Edge path (browser) | → Cluster path |
|---------------------|----------------|
| `GET gateway.duynhne.me/order/v1/private/orders` | `GET /api/v1/orders` |
| `GET gateway.duynhne.me/order/v1/private/orders/:id` | `GET /api/v1/orders/:id` |
| `GET gateway.duynhne.me/order/v1/private/orders/:id/details` | `GET /api/v1/orders/:id/details` |
| `POST gateway.duynhne.me/order/v1/private/orders` | `POST /api/v1/orders` |

The aggregation endpoint calls `shipping-service` via in-cluster DNS (`http://shipping.shipping.svc.cluster.local:8080/api/v1/shipping/orders/:orderId`) — that internal route is **not** on the gateway.

Convention + rewrite rule: [`homelab/docs/api/api-naming-convention.md`](https://github.com/duynhlab/homelab/blob/main/docs/api/api-naming-convention.md).
