-- RFC-0021 P5: close the vocabulary — orders.status is now the FSM's.
--
-- 000011 introduced the status model but left orders.status an open
-- VARCHAR because pre-FSM rows ('shipped', 'processing' from the demo
-- seed era) were still legal on disk. Every writer has since been
-- funneled through ApplyStatusCommand, whose FSM only produces the
-- seven states below, so the column can finally say so itself.
--
-- The two UPDATEs are the data half of the same statement: they map the
-- legacy vocabulary onto the FSM's before the constraint lands. The
-- mapping mirrors what those statuses meant —
--
--   shipped    -> completed   (fulfillment finished)
--   processing -> confirmed   (paid, mid-fulfillment)
--
-- No history rows are backfilled for these flips (history begins at the
-- v1.10.0 cutover; there is no real command_id to record), and
-- updated_at is deliberately left alone: both target states are in the
-- reconciler's terminal set, so aging them out is correct.
UPDATE orders SET status = 'completed'  WHERE status = 'shipped';
UPDATE orders SET status = 'confirmed'  WHERE status = 'processing';

ALTER TABLE orders
    ADD CONSTRAINT orders_status_check CHECK (status IN (
        'pending',
        'confirmed',
        'failed',
        'completed',
        'cancelling',
        'cancelled',
        'manual_review'
    ));
