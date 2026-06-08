-- V3__fix_sequences.sql
-- Fix sequence desynchronization caused by seed data
-- Last Updated: 2026-02-09

-- Set the sequence for orders table to the max id
SELECT setval('orders_id_seq', (SELECT MAX(id) FROM orders));

-- Set the sequence for order_items table to the max id
SELECT setval('order_items_id_seq', (SELECT MAX(id) FROM order_items));
