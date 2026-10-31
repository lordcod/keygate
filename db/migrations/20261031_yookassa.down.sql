DROP TABLE yookassa_subscriptions;
DROP TABLE yookassa_orders;
ALTER TABLE plans DROP CONSTRAINT plans_yookassa_terms;
ALTER TABLE plans DROP COLUMN yookassa_amount;
