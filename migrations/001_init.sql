-- Core schema for MethodSplit: merchants, payments, idempotency records.
-- Money is always stored as an integer (amount_minor), never a float.

CREATE TABLE IF NOT EXISTS merchants (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS payments (
  id            UUID PRIMARY KEY,
  merchant_id   TEXT NOT NULL REFERENCES merchants(id),
  amount_minor  BIGINT NOT NULL CHECK (amount_minor > 0),
  currency      CHAR(3) NOT NULL DEFAULT 'BDT',
  status        TEXT NOT NULL CHECK (status IN ('CREATED', 'COMPLETED', 'REFUNDED', 'FAILED')),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS payments_merchant_created_idx ON payments (merchant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS idempotency_records (
  merchant_id      TEXT NOT NULL,
  key              TEXT NOT NULL,
  request_hash     TEXT NOT NULL,
  response_status  INT NOT NULL,
  response_body    JSONB NOT NULL,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (merchant_id, key)
);

-- Demo merchants used by msclient examples and configs/merchants.yaml.
INSERT INTO merchants (id, name) VALUES
  ('merchant_demo_a', 'Demo Merchant A'),
  ('merchant_demo_b', 'Demo Merchant B')
ON CONFLICT (id) DO NOTHING;
