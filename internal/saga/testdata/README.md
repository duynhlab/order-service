# Recorded workflow histories (determinism gate)

Real `OrderFulfillmentWorkflow` executions exported from local-stack Temporal.
`replay_test.go` replays the **current generation** (`gen3/history_*.json`)
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
- **`gen2/`** — recorded against the P5 (v1.10.0–v1.12.x) workflow. It is the
  **last generation that can serve a product-participant history at all**, so it
  stays in-tree as the gate for maintenance builds of those versions. Four of
  its six histories carry `participant=product` and stop replaying against
  current code — measured, not assumed, which is what forced a new generation
  rather than an edit to this one.
- **`gen3/`** — recorded against the RFC-0021 P4 workflow, which deleted the
  product stock branch. **Recording this corpus is a hard gate for cutting the
  next order tag**: `replay_test.go` skips (loudly) while the directory is
  empty, and fails outright under `ORDER_RELEASE_GATE=1`, so the tag cannot be
  cut with the skip still firing.

`TestReplayCarriedForwardHistories` additionally replays the two gen2 histories
this build must **still** serve
(`history_happy_inventory.json`, `history_cancellation_happy.json`). They are
listed explicitly rather than globbed: the other four carry
`participant=product` and legitimately stop replaying, and an exclusion glob
would go stale without anyone noticing. It is not redundant with `gen3/` — it
asserts that a history recorded by the PREVIOUS generation still replays, which
no gen3 file can show.

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

## gen2 corpus (recorded 2026-08-01 from local-stack, order-service main)

| File | Scenario | New coverage vs gen1 |
|------|----------|----------------------|
| `gen2/history_happy_product.json` | full confirm, product path (order 6) | projection stages + `Complete` tail |
| `gen2/history_happy_inventory.json` | full confirm, inventory path (order 14) | `CommitInventory` → `INVENTORY_COMMITTED` → `Complete` |
| `gen2/history_payment_declined.json` | authorize declined (order 7, magic amount `…02`) | reason-carrying `FailOrder` |
| `gen2/history_shipment_failed.json` | shipment fails, compensations converge (order 8) | rewritten compensation helpers incl. the unconditional `CancelShipment` |
| `gen2/history_compensation_exhausted.json` | `CancelShipment` compensation exhausts (order 13) | `MarkManualReview` terminal |
| `gen2/history_cancellation_happy.json` | `CancellationWorkflow`: refund + release + cancelled (order 6, epoch v3) | the whole cancellation ladder |

An **insufficient-stock** history is deliberately absent: the checkout
funnel's own availability gate rejects the cart before an order exists, and
no deterministic hook can shrink stock inside the ~100 ms between checkout's
validation and the saga's reserve. The `INSUFFICIENT_STOCK` reason
propagation and the release-skip branch are pinned by workflow unit tests
instead; record the history if a stock-failure hook ever exists.

## gen3 corpus (recorded 2026-08-04 from local-stack, order-service P4)

| File | Scenario | Activity path (RecordProcessingStage elided) |
|------|----------|----------------------------------------------|
| `gen3/history_happy_inventory.json` | full confirm (order 6) | Authorize → ReserveInventory → CreateShipment → Capture → ConfirmOrder → Notify → Receipt → ClearCart → **CommitInventory** → Complete |
| `gen3/history_payment_declined.json` | authorize declined (order 8, total `65102` — mockpay declines a charge whose minor amount ends `02`) | Authorize(declined) → FailOrder |
| `gen3/history_shipment_failed.json` | shipment fails, compensations converge (order 9) | Authorize → ReserveInventory → CreateShipment(down) → CancelShipment → **ReleaseInventory** → VoidPayment → FailOrder |
| `gen3/history_compensation_exhausted.json` | compensation exhausts → parked (order 10) | Authorize(down) → VoidPayment(exhausts) → MarkManualReview |
| `gen3/history_cancellation_happy.json` | `CancellationWorkflow` (order 12, epoch v3) | the cancellation ladder |

There is **no product-path history**, and there cannot be: this build refuses
that participant, which is what forced the generation split.

`compensation_exhausted` reaches the park from a different direction than gen2's
(payment unreachable at authorize, so the void exhausts before any shipment)
rather than gen2's shipment-failed-then-payment-down route. The terminal
`MarkManualReview` command sequence — the thing the gate protects — is the same;
the pre-pivot compensation ladder is covered by `shipment_failed` above.

The tax figure to reproduce a total is **truncated**, not rounded:
`tax = int((subtotal + shipping) * 0.08)`. Order 6 is `2999 + 300 + 263 = 3562`.

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
   - shipment failed: complete checkout up to confirm, `docker compose stop
     shipping`, confirm; restart shipping afterwards.
   - compensation exhausted: as shipment-failed, but also `docker compose stop
     payment` before the void retries finish (10 attempts ≈ several minutes),
     so `VoidPayment` exhausts and the workflow parks the order in
     `manual_review`; restart payment afterwards and resolve the order.
3. Wait for the workflow to close, then export:

   ```bash
   docker compose exec -T temporal \
     temporal workflow show -w order-fulfillment-<order-id> -n mop -o json \
     > order-service/internal/saga/testdata/gen3/history_<name>.json
   ```

4. Sanity-check the activity list matches the intended scenario before
   committing (parse `events[].activityTaskScheduledEventAttributes`).
