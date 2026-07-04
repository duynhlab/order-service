-- =============================================================================
-- Order Service - Seed Data
-- =============================================================================
-- Purpose: Demo orders and order items for local/dev/demo environments
-- Usage: Run after V1 migration to populate test orders
-- Note: References auth.users (user_id) and product.products (product_id)
-- =============================================================================

-- =============================================================================
-- ORDERS
-- =============================================================================
-- Alice: 2 orders (1 completed, 1 shipped)
-- David: 2 orders (1 pending, 1 processing)
-- Eve: 1 order (completed)

INSERT INTO orders (id, user_id, subtotal, shipping, total, status, created_at, updated_at) VALUES
    -- Alice's orders
    (1, 1, 10997, 500, 11497, 'completed', NOW() - INTERVAL '10 days', NOW() - INTERVAL '8 days'),
    (2, 1, 16997, 500, 17497, 'shipped', NOW() - INTERVAL '3 days', NOW() - INTERVAL '1 day'),
    
    -- David's orders
    (3, 4, 5998, 500, 6498, 'pending', NOW() - INTERVAL '2 days', NOW() - INTERVAL '2 days'),
    (4, 4, 14999, 500, 15499, 'processing', NOW() - INTERVAL '5 days', NOW() - INTERVAL '4 days'),
    
    -- Eve's order
    (5, 5, 7999, 500, 8499, 'completed', NOW() - INTERVAL '20 days', NOW() - INTERVAL '18 days')
ON CONFLICT (id) DO NOTHING;

-- =============================================================================
-- ORDER ITEMS
-- =============================================================================
INSERT INTO order_items (id, order_id, product_id, product_name, quantity, price, subtotal, created_at) VALUES
    -- Order 1 (Alice, completed): Wireless Mouse x2, USB-C Hub x1, Laptop Stand x1
    (1, 1, 1, 'Wireless Mouse', 2, 2999, 5998, NOW() - INTERVAL '10 days'),
    (2, 1, 3, 'USB-C Hub', 1, 3999, 3999, NOW() - INTERVAL '10 days'),
    (3, 1, 4, 'Laptop Stand', 1, 4499, 4499, NOW() - INTERVAL '10 days'),
    
    -- Order 2 (Alice, shipped): Webcam HD x1, Gaming Headset x1
    (4, 2, 5, 'Webcam HD', 1, 5999, 5999, NOW() - INTERVAL '3 days'),
    (5, 2, 7, 'Gaming Headset', 1, 8999, 8999, NOW() - INTERVAL '3 days'),
    
    -- Order 3 (David, pending): Wireless Mouse x2
    (6, 3, 1, 'Wireless Mouse', 2, 2999, 5998, NOW() - INTERVAL '2 days'),
    
    -- Order 4 (David, processing): Monitor 24" x1
    (7, 4, 6, 'Monitor 24"', 1, 14999, 14999, NOW() - INTERVAL '5 days'),
    
    -- Order 5 (Eve, completed): Mechanical Keyboard x1
    (8, 5, 2, 'Mechanical Keyboard', 1, 7999, 7999, NOW() - INTERVAL '20 days')
ON CONFLICT (id) DO NOTHING;

-- =============================================================================
-- VERIFICATION
-- =============================================================================
-- Verify seed data loaded
SELECT 
    'Orders seeded' as status,
    (SELECT COUNT(*) FROM orders) as order_count,
    (SELECT COUNT(*) FROM order_items) as order_item_count,
    (SELECT SUM(total) FROM orders) as total_revenue
FROM orders
LIMIT 1;

-- =============================================================================
-- FIX SEQUENCES
-- =============================================================================
-- Reset sequences to max id to prevent duplicate key errors on new inserts
SELECT setval('orders_id_seq', (SELECT MAX(id) FROM orders));
SELECT setval('order_items_id_seq', (SELECT MAX(id) FROM order_items));
