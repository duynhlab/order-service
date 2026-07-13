-- RFC-0015 P4: the charged total composes from the caller's quoted fee, tax,
-- and promo discount (total = subtotal + shipping + tax - discount). The old
-- CHECK hard-coded the demo-fee era (total = subtotal + shipping) and made
-- every discounted/taxed insert fail.

ALTER TABLE orders ADD COLUMN IF NOT EXISTS tax      BIGINT NOT NULL DEFAULT 0 CHECK (tax >= 0);
ALTER TABLE orders ADD COLUMN IF NOT EXISTS discount BIGINT NOT NULL DEFAULT 0 CHECK (discount >= 0);

ALTER TABLE orders DROP CONSTRAINT IF EXISTS check_order_total;
ALTER TABLE orders ADD CONSTRAINT check_order_total
    CHECK (total = subtotal + shipping + tax - discount);
