-- V5__idempotency_per_user.sql
-- Scope the idempotency-key uniqueness per user to match the repository lookup
-- (FindByIdempotencyKey filters WHERE idempotency_key = $1 AND user_id = $2).
-- The old global index made user B reusing user A's key miss the replay lookup
-- and then trip the global constraint -> opaque 500. A composite (user_id,
-- idempotency_key) index lets each user own their own key namespace.

DROP INDEX IF EXISTS uq_orders_idempotency_key;

CREATE UNIQUE INDEX IF NOT EXISTS uq_orders_user_idempotency_key
    ON orders (user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
