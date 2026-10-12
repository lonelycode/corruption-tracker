-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

-- 0005: standalone local accounts, sessions and the access log.
--
-- Used by the standalone server only (corruption-tracker/standalone/
-- localauth, accesslog; design §2 D3, §5.2). An
-- embedding host (alitheia) authenticates its own users and audits into its
-- own log, so these tables stay empty there. Operational state, not dataset:
-- tracker_users and tracker_sessions never leave an instance in a bundle
-- (§10.4), tracker_access_log only on request, and all three are exempt
-- from the freeze write guard (§11.2) so logins keep working.
--
-- One role per user, strictly nested admin ⊃ reviewer ⊃ viewer; export is a
-- per-user grant meaningful only for viewers. Each user is exactly one
-- tracker_actors row (1:1). A legacy actor imported from another instance
-- has no tracker_users row until an admin claims it, so nobody can sign in
-- as it. Users are disabled, never deleted, so their audit trail survives.

CREATE TABLE tracker_users (
    id              BIGSERIAL   PRIMARY KEY,
    actor_id        BIGINT      NOT NULL UNIQUE REFERENCES tracker_actors(id),
    username        TEXT        NOT NULL,
    password_hash   TEXT        NOT NULL,
    role            TEXT        NOT NULL CHECK (role IN ('admin', 'reviewer', 'viewer')),
    can_export      BOOLEAN     NOT NULL DEFAULT FALSE,
    disabled        BOOLEAN     NOT NULL DEFAULT FALSE,
    -- The access-basis note for the current grant (e.g. the agreement
    -- reference). Every grant's note is also written to tracker_access_log.
    access_basis    TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at   TIMESTAMPTZ
);

CREATE UNIQUE INDEX tracker_users_username_idx ON tracker_users (lower(username));

-- Sessions store only a SHA-256 of the cookie token.
CREATE TABLE tracker_sessions (
    token_hash   TEXT        PRIMARY KEY,
    user_id      BIGINT      NOT NULL REFERENCES tracker_users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX tracker_sessions_user_idx ON tracker_sessions (user_id);
CREATE INDEX tracker_sessions_expiry_idx ON tracker_sessions (expires_at);

-- The Privacy-Act audit stream (person-level views and exports) plus the
-- access-administration trail (grants, role changes, disables, claims).
-- actor_label is denormalised so rows stay readable whatever happens to
-- the actor.
CREATE TABLE tracker_access_log (
    id          BIGSERIAL   PRIMARY KEY,
    actor_id    BIGINT      REFERENCES tracker_actors(id),
    actor_label TEXT        NOT NULL DEFAULT '',
    entitlement TEXT        NOT NULL DEFAULT '',
    surface     TEXT        NOT NULL,
    detail      TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX tracker_access_log_created_idx ON tracker_access_log (created_at DESC);
CREATE INDEX tracker_access_log_actor_idx ON tracker_access_log (actor_id, created_at DESC);
