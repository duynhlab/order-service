-- RFC-0021 P5: the order aggregate's status-model schema, additively.
--
-- orders.status is written today by a single unconditional UPDATE with no
-- guard: a lost-ack ConfirmOrder retried after the compensation branch wrote
-- `failed` silently flips the order back, and nothing records that either
-- write happened. This migration adds the two things the domain layer needs
-- to close that hole — a version column for compare-and-set writes, and an
-- append-only history table whose UNIQUE(order_id, command_id) makes every
-- transition idempotent by construction (a retried command replays instead
-- of double-applying).
--
-- Purely additive: no column changes shape, no CHECK lands on orders.status
-- yet (dev seeds still carry pre-FSM vocabulary; a later migration
-- normalizes then constrains), and code pinned before this release never
-- names the new columns. Old worker builds draining under ADR-030 keep
-- writing through the unversioned UPDATE — their orders simply carry no
-- history rows.
--
-- Deliberately NO history backfill: a history row asserts "a real command
-- did this", and pre-cutover transitions have no truthful command_id or
-- actor to record. History begins at the P5 cutover.
--
-- New timestamp columns are TIMESTAMPTZ like every table since 000008;
-- the legacy naive-TIMESTAMP orders.created_at/updated_at pair stays as-is
-- (the reconciler's session-timezone caveat documented in
-- postgres_start_request_repository.go is unchanged).

-- Optimistic-concurrency version. Existing rows start at 1; every domain
-- transition increments it inside the same guarded UPDATE.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;

-- Bounded reason/metadata columns. All reason values are enum tokens owned
-- by the Go domain package — VARCHAR(64) is the backstop, not the authority,
-- so adding a code never needs a migration (same stance as
-- fulfillment_start_requests.last_error_code).
ALTER TABLE orders ADD COLUMN IF NOT EXISTS failure_code         VARCHAR(64);
ALTER TABLE orders ADD COLUMN IF NOT EXISTS cancellation_reason  VARCHAR(64);
ALTER TABLE orders ADD COLUMN IF NOT EXISTS manual_review_reason VARCHAR(64);

-- Which workflow execution drove the order's lifecycle. Unlike the start
-- outbox (which derives `order-fulfillment-<id>` and stores nothing), the
-- history's job is audit: run_id pins WHICH execution applied a transition,
-- and that is not derivable after a reset mints a new run.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS workflow_id VARCHAR(255);
ALTER TABLE orders ADD COLUMN IF NOT EXISTS run_id      VARCHAR(64);

-- Per-terminal timestamps, written by the transition that reaches them.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS confirmed_at TIMESTAMPTZ;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS cancelled_at TIMESTAMPTZ;

-- Append-only transition log. One row per applied domain command; the
-- UNIQUE(order_id, command_id) pair is the idempotency anchor the repository
-- checks before writing (same schema-anchored stance as the per-user
-- idempotency index from 000005).
CREATE TABLE IF NOT EXISTS order_status_history (
    id          BIGSERIAL PRIMARY KEY,
    order_id    INTEGER NOT NULL REFERENCES orders(id) ON DELETE CASCADE,

    from_status VARCHAR(50) NOT NULL,
    to_status   VARCHAR(50) NOT NULL,

    -- Bounded token or NULL; never a message (Go enum is the authority).
    reason_code VARCHAR(64),

    -- Who issued the command. The only CHECK in this table: actor_type is a
    -- closed set the audit trail is sorted and alerted by, and a typo here
    -- is a corrupted audit dimension rather than a missing nicety.
    actor_type  VARCHAR(16) NOT NULL
                    CHECK (actor_type IN ('SYSTEM', 'WORKFLOW', 'USER', 'OPERATOR')),
    -- user id, operator id, or NULL for SYSTEM/WORKFLOW actors.
    actor_id    VARCHAR(255),

    -- Operator audit note (ResolveManualReview requires one); free text is
    -- acceptable here because it is human-authored input, never error text.
    note        TEXT,

    -- Deterministic command id minted by the caller (`confirm:<order-id>`,
    -- `fail:<order-id>:<reason>`, ...). The uniqueness pair below turns a
    -- retried command into a replay instead of a second transition.
    command_id  VARCHAR(255) NOT NULL,

    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_order_status_history_command UNIQUE (order_id, command_id)
);

-- The audit read is always "this order's transitions in order"; the UNIQUE
-- constraint above already serves the command_id replay lookup.
CREATE INDEX IF NOT EXISTS idx_order_status_history_order_created
    ON order_status_history (order_id, created_at);
