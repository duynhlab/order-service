# Recorded workflow histories (determinism gate)

Real `OrderFulfillmentWorkflow` executions exported from local-stack Temporal.
`replay_test.go` replays the **current generation** (`gen2/history_*.json`)
against the current workflow code on every `go test` run — a failure means the
change is **history-incompatible** and would break in-flight workflows at the
next worker deploy. Never delete or re-record a history to make the gate pass.

## Generations (ADR-030 worker versioning)

The corpus is split by worker deployment version, because pinned versioning
means a build only ever executes histories recorded by its own generation:

- **`gen1/`** — recorded against the ≤ v1.9.x workflow (unconditional
  `UpdateStatus` writes, no `Complete` tail). Kept in-tree for forensics and
  for cherry-picking onto a v1.9.x maintenance branch (whose own replay_test
  would glob it); nothing on main executes it. The P5 rewrite legitimately
  diverges from these (reason-carrying `FailOrder`, `MarkManualReview`, a
  `Complete` activity in the happy tail), and under pinned worker versioning
  no gen-1 execution is ever claimed by a gen-2 build. Local-stack runs the
  worker UNVERSIONED (no `TEMPORAL_WORKER_DEPLOYMENT_NAME`), so rebuilding
  the order image there while gen-1 workflows are in flight WILL hit
  nondeterminism — bring the stack up fresh instead; that is an accepted
  dev-only sharp edge.
- **`gen2/`** — recorded against the P5 (v1.10.0) workflow. **Recording this
  corpus is a hard gate for cutting the v1.10.0 tag**: `replay_test.go` skips
  (loudly) while the directory is empty, and the tag must not be cut with the
  skip still firing.

How to fix a replay failure depends on what changed. A *branch* on new input
needs no marker: homelab ADR-030 chose Temporal **Worker Versioning** over
`workflow.GetVersion`, so a new build takes new workflows while old ones drain
on the build that started them. A change to the command sequence a recorded
history already took is a genuine incompatibility within a generation —
restructure the change, do not touch the corpus. Only a new worker deployment
version may open a new generation directory.

## gen1 corpus

| File | Scenario | Activity path |
|------|----------|---------------|
| `gen1/history_v0_happy.json` | full confirm | Authorize → ReserveStock → CreateShipment → Capture → ConfirmOrder → Notify → Receipt → ClearCart |
| `gen1/history_v0_payment_declined.json` | authorize declined (step 0) | Authorize(declined) → FailOrder |
| `gen1/history_v0_insufficient_stock.json` | reserve fails (step 1) | Authorize → ReserveStock(insufficient) → VoidPayment → FailOrder |
| `gen1/history_v0_shipment_failed.json` | shipment fails (step 2) | Authorize → ReserveStock → CreateShipment(down) → **ReleaseStock** → VoidPayment → FailOrder |
| `gen1/history_v1_inventory_happy.json` | full confirm, inventory participant | Authorize → **ReserveInventory** → CreateShipment → Capture → ConfirmOrder → Notify → Receipt → ClearCart → **CommitInventory** |

`v0` = the product stock-participant call graph (what every history recorded
before RFC-0021 P3 carries, with no `StockParticipant` on the input). `v1` =
the inventory participant.

## gen2 corpus (to record before the v1.10.0 tag)

| File | Scenario | New coverage vs gen1 |
|------|----------|----------------------|
| `gen2/history_happy_product.json` | full confirm, product path | `Complete` tail |
| `gen2/history_happy_inventory.json` | full confirm, inventory path | `CommitInventory` then `Complete` |
| `gen2/history_payment_declined.json` | authorize declined | reason-carrying `FailOrder` |
| `gen2/history_shipment_failed.json` | shipment fails | rewritten compensation helpers |
| `gen2/history_compensation_exhausted.json` | a compensation exhausts | `MarkManualReview` terminal |
| `gen2/history_insufficient_stock.json` | reserve declined (out of stock) | `INSUFFICIENT_STOCK` reason + the release-skip branch |

## Recording procedure (gen2 and later; adding NEW scenarios only)

The legacy REST create path is removed in P5, so scenarios are driven through
the **checkout funnel** (the SPA at `:3001`, or curl against
`/checkout/v1/private/checkout/sessions` — see homelab `docs/api/checkout.md`
for the session → confirm flow; address field is `country`, len 2).

1. `cd homelab/local-stack && docker compose up -d --build` (all repos on the
   revision being recorded).
2. Drive the scenario (login `alice` / `password123`; items come from the CART):
   - happy: normal checkout from the seeded cart.
   - payment declined: a cart whose confirmed total makes mockpay decline
     (amount-dependent; verify the decline in the payment logs, retry with a
     different amount if needed).
   - insufficient stock: a checkout whose quantity exceeds availability; if
     the checkout funnel's own availability gate rejects it before an order
     exists, shrink the product's stock row directly in the DB between
     session creation and confirm so ReserveStock is what fails.
   - shipment failed: complete checkout up to confirm, `docker compose stop
     shipping`, confirm; restart shipping afterwards.
   - compensation exhausted: as shipment-failed, but also `docker compose stop
     payment` before the void retries finish (10 attempts ≈ several minutes),
     so `VoidPayment` exhausts and the workflow parks the order in
     `manual_review`; restart payment afterwards and resolve the order.
   - inventory participant: the cluster default; in local-stack set
     `ORDER_STOCK_PARTICIPANT=inventory` on the `order` service via a local,
     uncommitted `compose.override.yaml`, `docker compose up -d --no-deps
     order`, and remove the override afterwards.
3. Wait for the workflow to close, then export:

   ```bash
   docker compose exec -T temporal \
     temporal workflow show -w order-fulfillment-<order-id> -n mop -o json \
     > order-service/internal/saga/testdata/gen2/history_<name>.json
   ```

4. Sanity-check the activity list matches the intended scenario before
   committing (parse `events[].activityTaskScheduledEventAttributes`).
