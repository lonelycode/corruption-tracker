-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

-- 0007: tracker_settings — the standalone operator's configuration.
-- One row per setting the operator has changed from its
-- default; environment variables override rows at runtime. Secret settings
-- (API keys, mailbox passwords) are stored only as AES-256-GCM ciphertext
-- sealed with the instance key (data volume secret.key or TRACKER_SECRET);
-- their value column is always empty. An embedded host (alitheia) never
-- writes this table: it supplies settings from its own configuration.
CREATE TABLE tracker_settings (
    key        TEXT        PRIMARY KEY,
    value      TEXT        NOT NULL DEFAULT '',
    secret     BOOLEAN     NOT NULL DEFAULT false,
    ciphertext BYTEA,
    updated_by BIGINT      REFERENCES tracker_actors(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT tracker_settings_secret_chk CHECK (
        (secret AND value = '' AND ciphertext IS NOT NULL)
        OR (NOT secret AND ciphertext IS NULL))
);
