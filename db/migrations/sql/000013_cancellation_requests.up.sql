-- RFC-0021 P5: outbox for the cancellation-workflow start.
--
-- Same durability argument as the fulfillment start outbox (000008): the
-- status flip to `cancelling` and "this order needs a cancellation workflow"
-- must commit together, or a crash between them leaves an order telling the
-- customer it is cancelling with nothing left to drive it. The row is
-- written in the same transaction as the CAS transition; the inline start
-- closes it in the common case and the dispatcher sweeps the leftovers.
--
-- Simpler than the fulfillment outbox on purpose: no payment token (the
-- workflow reads payment state server-side) and no max-row-age money hazard
-- (every cancellation activity is idempotent and CompleteCancellation is
-- CAS'd, so a late duplicate start is harmless).
--
-- epoch is the orders.version observed when the episode was requested. It
-- namespaces BOTH the workflow id (order-cancellation-<id>-v<epoch>) and the
-- episode's command ids, which is what lets a legally repeated episode
-- (manual_review -> confirmed -> cancel again) re-arm this row: the PK keeps
-- one live row per order, and re-arming resets it for the new epoch.
CREATE TABLE IF NOT EXISTS cancellation_requests (
    order_id        INTEGER PRIMARY KEY REFERENCES orders(id) ON DELETE CASCADE,

    status          VARCHAR(16) NOT NULL DEFAULT 'PENDING'
                        CHECK (status IN ('PENDING', 'DISPATCHED', 'FAILED')),

    epoch           BIGINT  NOT NULL,

    attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Bounded grpcx reason token or Temporal error type, never a message.
    last_error_code VARCHAR(64),

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The dispatcher scans due PENDING rows; healthy platforms keep this empty.
CREATE INDEX IF NOT EXISTS idx_cancellation_requests_due
    ON cancellation_requests (next_attempt_at)
    WHERE status = 'PENDING';

-- Backs the outbox gauges without touching DISPATCHED rows.
CREATE INDEX IF NOT EXISTS idx_cancellation_requests_open
    ON cancellation_requests (status, created_at)
    WHERE status <> 'DISPATCHED';
