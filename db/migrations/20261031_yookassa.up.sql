ALTER TABLE plans ADD COLUMN yookassa_amount bigint NOT NULL DEFAULT 0 CHECK (yookassa_amount >= 0 AND yookassa_amount <= 100000000);
ALTER TABLE plans ADD CONSTRAINT plans_yookassa_terms CHECK (yookassa_amount = 0 OR (NOT self_signup AND license_type IN ('perpetual', 'subscription') AND (license_type <> 'subscription' OR billing_interval IN ('month', 'year'))));
CREATE TABLE yookassa_orders (
    shop_id text NOT NULL,
    test_mode boolean NOT NULL,
    id uuid PRIMARY KEY,
    plan_id uuid NOT NULL REFERENCES plans(id),
    email text NOT NULL,
    amount bigint NOT NULL CHECK (amount > 0),
    quantity integer NOT NULL CHECK (quantity > 0),
    license_type text NOT NULL,
    billing_interval text NOT NULL DEFAULT '',
    updates_days integer NOT NULL DEFAULT 0,
    recurring boolean NOT NULL DEFAULT false,
    renewal_license_id uuid REFERENCES licenses(id),
    period_start timestamptz,
    payment_id text UNIQUE,
    request jsonb NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    license_id uuid REFERENCES licenses(id) DEFERRABLE INITIALLY DEFERRED,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (renewal_license_id, period_start)
);
CREATE INDEX yookassa_orders_pending ON yookassa_orders(created_at) WHERE status = 'pending';
CREATE TABLE yookassa_subscriptions (
    plan_id uuid NOT NULL REFERENCES plans(id),
    quantity integer NOT NULL CHECK (quantity > 0),
    shop_id text NOT NULL,
    test_mode boolean NOT NULL,
    license_id uuid PRIMARY KEY REFERENCES licenses(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    payment_method_id text NOT NULL,
    amount bigint NOT NULL CHECK (amount > 0),
    billing_interval text NOT NULL CHECK (billing_interval IN ('month', 'year')),
    next_charge_at timestamptz NOT NULL,
    canceled boolean NOT NULL DEFAULT false
);
