-- RFC-0021 P5: the processing-stage read projection.
--
-- orders.status answers "what is commercially true"; this table answers
-- "where is processing right now" for the UI (`/details` renders it). It is
-- a PROJECTION, never a correctness gate: workflows upsert it best-effort at
-- each stage boundary, OUTSIDE the status CAS transaction — a lost write
-- self-heals at the next boundary, and coupling it into the CAS would hand a
-- UX table veto power over money-bearing transitions.
--
-- stage carries a CHECK (unlike the reason columns) because the set is the
-- rendering contract with the SPA: a typo'd stage would render as an unknown
-- chip forever, and stages change rarely enough that a migration per
-- addition is the cheaper failure mode.
CREATE TABLE IF NOT EXISTS order_processing_projection (
    order_id             INTEGER PRIMARY KEY REFERENCES orders(id) ON DELETE CASCADE,
    stage                VARCHAR(32) NOT NULL CHECK (stage IN (
                             'ORDER_CREATED',
                             'PAYMENT_AUTHORIZED',
                             'INVENTORY_RESERVED',
                             'SHIPMENT_PREPARED',
                             'PAYMENT_CAPTURED',
                             'ORDER_CONFIRMED',
                             'POST_PROCESSING',
                             'INVENTORY_COMMITTED',
                             'COMPENSATING',
                             'CANCELLING',
                             'MANUAL_REVIEW',
                             'DONE'
                         )),
    -- Bounded activity-name token; NULL until the first step lands.
    last_successful_step VARCHAR(32),
    -- Bounded grpcx/domain reason token, never a message (same stance as
    -- fulfillment_start_requests.last_error_code).
    last_error_code      VARCHAR(64),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
