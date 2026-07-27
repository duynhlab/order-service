# Recorded workflow histories (RFC-0021 P3 determinism gate)

Real `OrderFulfillmentWorkflow` executions exported from local-stack Temporal.
`replay_test.go` replays every `history_*.json` here against the current
workflow code on every `go test` run — a failure means the change is
**history-incompatible** and would break in-flight workflows at the next worker
deploy. Fix the workflow (add a `workflow.GetVersion` branch); never delete or
re-record a history to make the gate pass.

| File | Scenario | Activity path |
|------|----------|---------------|
| `history_v0_happy.json` | full confirm | Authorize → ReserveStock → CreateShipment → Capture → ConfirmOrder → Notify → Receipt → ClearCart |
| `history_v0_payment_declined.json` | authorize declined (step 0) | Authorize(declined) → FailOrder |
| `history_v0_insufficient_stock.json` | reserve fails (step 1) | Authorize → ReserveStock(insufficient) → VoidPayment → FailOrder |

`v0` = the pre-`inventory-extraction-v1` (product stock participant) call
graph. `v1` histories (inventory branch) are appended when the versioned
workflow ships.

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
3. Wait for the workflow to close, then export:

   ```bash
   docker compose exec -T temporal \
     temporal workflow show -w order-fulfillment-<order-id> -n mop -o json \
     > order-service/internal/saga/testdata/history_<name>.json
   ```

4. Sanity-check the activity list matches the intended scenario before
   committing (`python3 - <<'EOF'` … parse `events[].activityTaskScheduledEventAttributes`).
