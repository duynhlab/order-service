-- Convert money columns from DECIMAL(10,2) dollars to BIGINT minor units (cents).
-- Integer minor units give exact arithmetic (no float drift) and are the unit
-- the payment authorize/refund path speaks. Existing rows are scaled ×100.
--
-- The two business-rule CHECK constraints reference these columns, so they are
-- dropped before the type change and recreated afterwards (the invariants hold
-- identically in minor units: total = subtotal + shipping, subtotal = qty × price).

BEGIN;

ALTER TABLE orders DROP CONSTRAINT check_order_total;
ALTER TABLE order_items DROP CONSTRAINT check_item_subtotal;

ALTER TABLE orders
    ALTER COLUMN subtotal TYPE BIGINT USING (round(subtotal * 100))::bigint,
    ALTER COLUMN shipping TYPE BIGINT USING (round(shipping * 100))::bigint,
    ALTER COLUMN total    TYPE BIGINT USING (round(total * 100))::bigint;
ALTER TABLE orders ALTER COLUMN shipping SET DEFAULT 500;

ALTER TABLE order_items
    ALTER COLUMN price    TYPE BIGINT USING (round(price * 100))::bigint,
    ALTER COLUMN subtotal TYPE BIGINT USING (round(subtotal * 100))::bigint;

ALTER TABLE orders ADD CONSTRAINT check_order_total CHECK (total = subtotal + shipping);
ALTER TABLE order_items ADD CONSTRAINT check_item_subtotal CHECK (subtotal = quantity * price);

COMMENT ON COLUMN orders.subtotal IS 'Sum of all order items subtotals (minor units / cents)';
COMMENT ON COLUMN orders.shipping IS 'Shipping cost in minor units (currently fixed at 500 = $5.00)';
COMMENT ON COLUMN orders.total IS 'Subtotal + Shipping (minor units / cents)';
COMMENT ON COLUMN order_items.price IS 'Product price at time of order (minor units / cents)';
COMMENT ON COLUMN order_items.subtotal IS 'Quantity × Price (minor units / cents)';

COMMIT;
