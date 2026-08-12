# order-service

Orders, and the saga that fulfils them: this service is the only writer of the
order database and the orchestrator of the fulfilment workflow.

## Responsibilities

- **Owns:** orders and their items, the totals, idempotency records, the
  append-only status history, the processing projection, and the cancellation
  and fulfilment-start outboxes. It owns the fulfilment task queue and both
  workflows on it.
- **Does not own:** money (`payment-service`), stock (`inventory-service`),
  shipments (`shipping-service`), notifications (`notification-service`), the
  cart (`cart-service`), or price validation and the checkout funnel
  (`checkout-service`). It holds cross-service references with no foreign keys
  and reaches those owners over gRPC.

## Tech

| Area | Technology |
|------|------------|
| Runtime | Go 1.26 |
| Transports | HTTP (private reads and cancel) · gRPC server (order creation) · gRPC client (enrichment and saga activities) |
| Workflows | Temporal — orchestrator, under worker deployment versioning |
| Data | PostgreSQL |
| Platform libraries | `authmw`, `dbx`, `flagx`, `grpcx`, `httpx`, `logger/zapx`, `migratex`, `obsx`, `proto`, `temporalx` |

## API

- **Canonical contract:** [`homelab/docs/api/order.md`](https://github.com/duynhlab/homelab/blob/main/docs/api/order.md)
- **Shared conventions:** [`homelab/docs/api/api.md`](https://github.com/duynhlab/homelab/blob/main/docs/api/api.md)
- **Surfaces:** JWT-protected HTTP for a customer's own orders and the cancel
  request, and `order.v1.OrderService` east-west — checkout creates orders
  through it. There is no REST creation path. HTTP `:8080` also carries
  `/health` and `/ready`.

Routes, payloads, RPC semantics and error codes live in the contract, so there
is one place to change when they change.

## Run locally

Prefer the homelab **local-stack** — an order is a saga across five services, so
it is not meaningful in isolation.

One binary, four modes:

```bash
go run cmd/main.go migrate   # apply schema migrations
go run cmd/main.go seed      # demo orders — development only, refuses production
go run cmd/main.go           # serve HTTP :8080 + gRPC :9090
go run cmd/main.go worker    # run the Temporal worker instead
```

The worker serves its own probes and no API. In the cluster it is deployed once
per build id — see the worker note in AGENTS.md before changing workflow code.

## Verify

The commands CI runs, so a green local run means a green pipeline:

```bash
go build ./...
go test -race ./...
go test -tags=integration ./internal/core/repository/...   # needs Docker (testcontainers)
golangci-lint run
```

`internal/saga/testdata/` holds recorded workflow histories. The replay test
that reads them is the determinism guard — see AGENTS.md.

## Docs

- [Canonical contract](https://github.com/duynhlab/homelab/blob/main/docs/api/order.md)
- [local-stack guide](https://github.com/duynhlab/homelab/blob/main/local-stack/README.md)

## License

MIT
