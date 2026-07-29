-- Constrain the participant column now that it decides where stock is written.
--
-- Migration 000009 added it as a bare VARCHAR(16) when it was only a hint the
-- reconciler used to classify a missing reservation. It is now the authority every
-- saga start resolves the stock branch from, and a value outside the enum makes
-- the workflow stall on an unknown participant (saga.OrderFulfillmentInput.
-- participant panics deliberately rather than guessing which service holds the
-- stock). A column that routes an inventory write earns the same guard the
-- ORDER_STOCK_PARTICIPANT flag has had since it was introduced.
--
-- NULL stays legal: rows written before 000009 have it, and both the resolver and
-- the reconciler read an absent value as the product path.
--
-- One plain ADD CONSTRAINT rather than the NOT VALID + VALIDATE dance: the runner
-- applies each migration file in a single transaction, so the ACCESS EXCLUSIVE
-- lock is held until commit either way and the split would buy nothing.
ALTER TABLE fulfillment_start_requests
    DROP CONSTRAINT IF EXISTS fulfillment_start_requests_participant_check;

ALTER TABLE fulfillment_start_requests
    ADD CONSTRAINT fulfillment_start_requests_participant_check
    CHECK (participant IS NULL OR participant IN ('product', 'inventory'));
