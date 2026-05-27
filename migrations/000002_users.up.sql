-- 000002_users.up.sql

BEGIN;

-- citext is required for case-insensitive email comparison/uniqueness.
CREATE EXTENSION IF NOT EXISTS citext;

-- Shared trigger function: bumps updated_at on every row mutation.
-- Reused by wallets (and any future mutable table).
CREATE OR REPLACE FUNCTION trg_set_updated_at()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$;

CREATE TABLE users (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    external_id     TEXT         NOT NULL,
    email           CITEXT,
    username        TEXT         NOT NULL,
    status          user_status  NOT NULL DEFAULT 'KYC_PENDING',
    country_code    CHAR(2)      NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT users_external_id_unique UNIQUE (external_id),
    CONSTRAINT users_username_unique    UNIQUE (username),
    -- Sweepstakes are US-only; enforce ISO-3166-1 alpha-2 shape at DB level.
    CONSTRAINT users_country_code_format CHECK (country_code ~ '^[A-Z]{2}$')
);

-- Email is conditionally unique: allow NULL during pre-KYC import flows.
CREATE UNIQUE INDEX users_email_unique_idx
    ON users (email)
    WHERE email IS NOT NULL;

CREATE INDEX users_status_idx ON users (status);

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW
    EXECUTE FUNCTION trg_set_updated_at();

COMMIT;
