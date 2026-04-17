# order-service

Order processing microservice for creating and tracking orders.

## Features

- Order creation
- Order status tracking
- Aggregated order details (with shipment)
- Order history

## API Endpoints

> **Browser callers** hit `https://gateway.duynhne.me/order/v1/private/orders/…` (all routes private — JWT required); Kong rewrites to the cluster paths below. See [homelab naming convention](https://github.com/duynhlab/homelab/blob/main/docs/api/api-naming-convention.md).

| Method | Cluster path | Edge path (via gateway) |
|--------|--------------|-------------------------|
| `GET` | `/api/v1/orders` | `/order/v1/private/orders` |
| `GET` | `/api/v1/orders/:id` | `/order/v1/private/orders/:id` |
| `GET` | `/api/v1/orders/:id/details` | `/order/v1/private/orders/:id/details` |
| `POST` | `/api/v1/orders` | `/order/v1/private/orders` |

## Tech Stack

- Go + Gin framework
- PostgreSQL 18 (transaction-db cluster, shared with cart)
- PgCat connection pooling
- OpenTelemetry tracing

## Development

### Prerequisites

- Go 1.25+
- [golangci-lint](https://golangci-lint.run/welcome/install/) v2+

### Local Development

```bash
# Install dependencies
go mod tidy
go mod download

# Build
go build ./...

# Test
go test ./...

# Lint (must pass before PR merge)
golangci-lint run --timeout=10m

# Run locally (requires .env or env vars)
go run cmd/main.go
```

### Pre-push Checklist

```bash
go build ./... && go test ./... && golangci-lint run --timeout=10m
```

## License

MIT
