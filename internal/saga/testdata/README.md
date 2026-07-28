# Recorded workflow histories (RFC-0021 P3 determinism gate)

Real `OrderFulfillmentWorkflow` executions exported from local-stack Temporal.
`replay_test.go` replays every `history_*.json` here against the current
workflow code on every `go test` run — a failure means the change is
**history-incompatible** and would break in-flight workflows at the next worker
deploy. Never delete or re-record a history to make the gate pass.

How to fix a failure depends on what changed. A *branch* on new input (the
RFC-0021 participant) needs no marker: homelab ADR-030 chose Temporal **Worker
Versioning** over `workflow.GetVersion`, so a new build takes new workflows while
old ones drain on the build that started them. A change to the command sequence a
recorded history already took is a genuine incompatibility — restructure the
change, do not touch the corpus.

| File | Scenario | Activity path |
|------|----------|---------------|
| `history_v0_happy.json` | full confirm | Authorize → ReserveStock → CreateShipment → Capture → ConfirmOrder → Notify → Receipt → ClearCart |
| `history_v0_payment_declined.json` | authorize declined (step 0) | Authorize(declined) → FailOrder |
| `history_v0_insufficient_stock.json` | reserve fails (step 1) | Authorize → ReserveStock(insufficient) → VoidPayment → FailOrder |
| `history_v0_shipment_failed.json` | shipment fails (step 2) | Authorize → ReserveStock → CreateShipment(down) → **ReleaseStock** → VoidPayment → FailOrder |
| `history_v1_inventory_happy.json` | full confirm, inventory participant | Authorize → **ReserveInventory** → CreateShipment → Capture → ConfirmOrder → Notify → Receipt → ClearCart → **CommitInventory** |

`v0` = the product stock-participant call graph (what every history recorded
before RFC-0021 P3 carries, with no `StockParticipant` on the input). `v1` = the
inventory participant.

`history_v0_shipment_failed.json` earns its place: it is the only v0 history that
schedules `ReleaseStock`, so it is what covers the compensation helper the P3
change rewrote. `history_v1_inventory_happy.json` covers the branch itself —
note `CommitInventory` is LAST, after the customer-visible tail, so an inventory
outage cannot withhold the confirmation email and leave an uncleared cart.

## Re-recording procedure (adding NEW scenarios only)

1. `cd homelab/local-stack && docker compose up -d --build` (all repos on main).
2. Drive the scenario through the REST API (login `alice`/`password123`; items
   come from the CART, not the request body):
   - happy: order from the seeded cart.
   - payment declined: cart totaling `% 100 == 95` (all seed prices are X.99 and
     the REST path adds a 500-minor demo fee, so a total quantity of 5 works —
     e.g. product 1 × 5) → mockpay declines.
   - insufficient stock: cart line with quantity far above stock (e.g. product 6
     × 9999) → ReserveStock fails, saga compensates.
   - shipment failed: complete the checkout up to the payment step, then
     `docker compose stop shipping` and confirm; restart shipping afterwards.
   - inventory participant: set `ORDER_STOCK_PARTICIPANT=inventory` on the
     `order` service (a local, uncommitted `compose.override.yaml` keeps
     `compose.yaml` clean) and `docker compose up -d --no-deps order`; remove the
     override afterwards and confirm the flag reads `product` again.
3. Wait for the workflow to close, then export:

   ```bash
   docker compose exec -T temporal \
     temporal workflow show -w order-fulfillment-<order-id> -n mop -o json \
     > order-service/internal/saga/testdata/history_<name>.json
   ```

4. Sanity-check the activity list matches the intended scenario before
   committing (`python3 - <<'EOF'` … parse `events[].activityTaskScheduledEventAttributes`).
