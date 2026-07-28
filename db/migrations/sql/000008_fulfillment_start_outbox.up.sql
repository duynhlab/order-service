-- RFC-0021 P3: outbox for the fulfillment-workflow start.
--
-- The order row and "this order needs a saga" must commit together. Before this
-- table, CreateOrder committed the order and THEN called Temporal: a crash or a
-- Temporal outage in that window left an order stuck `pending` forever, with
-- nothing durable that remembered to start it. The row is written inside the
-- same transaction as the order, so either both exist or neither does.
--
-- The inline start stays for latency; on success it marks the row DISPATCHED.
-- The dispatcher in the worker only picks up what the inline path failed to
-- start, which is why PENDING rows are normally absent rather than transient.
--
-- Deliberately NO workflow_id column: it is a pure function of the order id
-- (`order-fulfillment-<id>`), so storing it would create a second source of
-- truth that has to stay in sync for no benefit. The dispatcher derives it
-- through the same fulfillment.Start seam the API uses.
--
-- payment_method is the checkout's opaque `tok_` reference, and it is here only
-- because the dispatcher cannot rebuild it: it is not a column on `orders` by
-- design. Two things bound the exposure — it is NULLed the moment the row
-- reaches DISPATCHED (which for the common path is milliseconds after insert),
-- and the same value already lives in the Temporal workflow input for the
-- namespace retention, so this is not a new class of storage. PAN-shaped input
-- never reaches this service at all; that is rejected at the checkout edge.
CREATE TABLE IF NOT EXISTS fulfillment_start_requests (
    -- One row per order, so the primary key IS the uniqueness guarantee and a
    -- retry can never enqueue a second start for the same order.
    order_id        INTEGER PRIMARY KEY REFERENCES orders(id) ON DELETE CASCADE,

    status          VARCHAR(16) NOT NULL DEFAULT 'PENDING'
                        CHECK (status IN ('PENDING', 'DISPATCHED', 'FAILED')),

    -- Cleared on BOTH terminal transitions, DISPATCHED and FAILED. A FAILED row
    -- can sit indefinitely and a payment token is not something to keep
    -- indefinitely; by the time a row reaches the attempt cap (~two hours of
    -- retries) the authorization window has almost certainly passed anyway, so
    -- the honest operator action is to fail the order and let the customer retry
    -- rather than start a saga hours late against a stale token.
    payment_method  TEXT,

    attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Only ever a bounded grpcx reason token or a Temporal error type — never a
    -- message. Sanitized by construction at the call site, and the length cap is
    -- the backstop.
    last_error_code VARCHAR(64),

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Partial: the dispatcher only ever scans due PENDING rows, and on a healthy
-- platform that set is empty. A full index would grow with every order ever
-- created for a query that never reads them.
CREATE INDEX IF NOT EXISTS idx_fulfillment_start_requests_due
    ON fulfillment_start_requests (next_attempt_at)
    WHERE status = 'PENDING';

-- Answers "is anything stuck?" without scanning the table: the alert queries
-- oldest PENDING age, and FAILED rows are the manual-requeue worklist.
CREATE INDEX IF NOT EXISTS idx_fulfillment_start_requests_failed
    ON fulfillment_start_requests (created_at)
    WHERE status = 'FAILED';
