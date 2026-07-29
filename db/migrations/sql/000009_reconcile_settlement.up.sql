-- RFC-0021 P3: record whether an order's stock SETTLED — that its inventory
-- reservation reached the state its terminal status implies.
--
-- A separate file rather than an edit to 000008, even though 000008 introduced
-- this very table one commit earlier. golang-migrate keys on
-- schema_migrations.version: any database that already ran 8 skips the file
-- forever, so an edit reports "migrations applied" and silently changes nothing.
-- The next CreateOrder then fails with 42703 inside the order's own transaction
-- (the outbox insert names `participant`), which turns a schema oversight into a
-- total write outage. CI cannot catch it either — integration tests start a fresh
-- container and replay every file from scratch, so they are green precisely
-- because they never exercise an already-migrated database.
--
-- ADD COLUMN IF NOT EXISTS / CREATE INDEX IF NOT EXISTS keep this idempotent, so
-- it is also safe on a database that was migrated by an intermediate build.
--
-- Additive only: nullable columns and one partial index, no rewrite of existing
-- rows, so old and new code are both valid against the schema at every step.

-- Which service owns this order's stock. Persisted because the participant is
-- otherwise only in the Temporal workflow input, and two readers need it from the
-- table: the reconciler, to tell an order that legitimately has NO reservation
-- (product path) from a confirmed inventory-path order whose reservation has gone
-- missing — an invariant breach that would otherwise read as "fine"; and the
-- outbox dispatcher, which starts the deferred saga with the participant the API
-- recorded rather than re-deciding from the worker's own copy of the flag (the two
-- processes roll out at different times).
--
-- NULL for rows written before this column existed. Those are product-path by
-- definition, which is also how the reconciler reads an empty value.
ALTER TABLE fulfillment_start_requests
    ADD COLUMN IF NOT EXISTS participant VARCHAR(16);

-- Set once the order's stock state agrees with its outcome, whether it already
-- did or the reconciler repaired it. NULL means "not settled yet", and that is
-- what the reconciler scans for.
--
-- Without it the reconciler had to re-examine EVERY terminal order in a 24h
-- window on every pass; consistent orders ate the batch, so at ordinary volume it
-- only ever looked at the oldest few hours and newer inconsistencies aged out of
-- the window unexamined — while the backlog gauge, computed over the examined
-- prefix, read zero.
ALTER TABLE fulfillment_start_requests
    ADD COLUMN IF NOT EXISTS reconciled_at TIMESTAMPTZ;

-- Set when the disagreement cannot be repaired by any valid transition (a failed
-- order whose stock is COMMITTED, a confirmed order whose stock went back). Such
-- a row stays unreconciled on purpose — it IS inconsistent, so it belongs in the
-- backlog until a human resolves it — but recording the REASON lets the
-- reconciler report it ONCE instead of once per pass, lets the scan put fresh work
-- ahead of known breaches so they cannot starve it, and keeps the table
-- answerable after the log line has aged out.
ALTER TABLE fulfillment_start_requests
    ADD COLUMN IF NOT EXISTS reconcile_breach_code VARCHAR(64);

-- Drives the reconciler's scan and its backlog count. Partial, because on a
-- healthy platform almost every row is settled: the unreconciled set is what the
-- reconciler works from, so its cost tracks unsettled work rather than lifetime
-- order volume. Joining orders from here (rather than scanning orders by
-- updated_at) is also why no index is added to that hot table.
CREATE INDEX IF NOT EXISTS idx_fulfillment_start_requests_unreconciled
    ON fulfillment_start_requests (order_id)
    WHERE reconciled_at IS NULL;
