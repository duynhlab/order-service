-- V4__idempotency.sql
-- Add idempotency support to prevent duplicate orders on retry / double-submit.
-- A partial unique index leaves existing rows (NULL key) untouched and only
-- enforces uniqueness for client-supplied keys.

ALTER TABLE orders ADD COLUMN IF NOT EXISTS idempotency_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS uq_orders_idempotency_key
    ON orders (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

COMMENT ON COLUMN orders.idempotency_key IS
    'Client-supplied Idempotency-Key header; dedupes order creation on retry. NULL for legacy/no-key orders.';
