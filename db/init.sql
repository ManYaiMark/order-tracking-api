CREATE TABLE orders (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_no         TEXT NOT NULL UNIQUE
                    CHECK (order_no ~ '^ORD-[0-9]+-[0-9]+$'),
    idempotency_key  TEXT UNIQUE,
    game_uid         TEXT NOT NULL,
    product          TEXT NOT NULL,
    amount           NUMERIC(12,2) NOT NULL CHECK (amount > 0),
    status           TEXT NOT NULL DEFAULT 'PENDING_PAYMENT'
                    CHECK (status IN ('PENDING_PAYMENT','QUEUED','IN_PROGRESS',
                            'COMPLETED','DELAYED','FAILED','EXPIRED','REFUNDED')),
    version          INT NOT NULL DEFAULT 1,      -- optimistic lock (เคส 2)
    attempts         INT NOT NULL DEFAULT 0,      -- retry (เคส 1)
    claimed_by       TEXT,                        -- bot ที่จับงาน
    lease_expires_at TIMESTAMPTZ,                 -- heartbeat/lease
    expires_at       TIMESTAMPTZ,                 -- หมดอายุ QR (เคส 5)
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE order_events (
    id          BIGSERIAL PRIMARY KEY,
    order_no    TEXT NOT NULL REFERENCES orders(order_no),
    from_status TEXT,
    to_status   TEXT NOT NULL,
    reason      TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_orders_status_created ON orders (status, created_at DESC);
CREATE INDEX idx_events_order ON order_events (order_no, created_at);