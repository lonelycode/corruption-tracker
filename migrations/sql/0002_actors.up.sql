-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

-- 0002: actor directory — the tracker's own record of who did what.
--
-- Every reviewer / verifier / decider / appeal-checker column now references
-- tracker_actors instead of a host application's users table, so the tracker
-- schema has no dependency on any host identity model (standalone accounts,
-- alitheia users, imported legacy reviewers are all just actors).
--
-- external_ref is the stable host identity ("alitheia:user:42", "local:7",
-- "system:recode"); hosts resolve principals to actors by it, never by id.
--
-- EMBEDDED-HOST ADOPTION (the only place the tracker looks at host tables):
-- on a database that has alitheia's users table, every user referenced by a
-- tracker column — or by a fraud_case entity_revisions row, which 0003 moves
-- — is backfilled as an actor with actor.id = users.id. Keeping the ids
-- means no reviewer value is rewritten, which matters because
-- fraud_claim_reviews is append-only (its trigger blocks UPDATE). New actors
-- get sequence ids above the backfilled ones. A fresh standalone database
-- skips the block. The guards look only at the schema being migrated, never
-- through search_path, so a tracker schema migrated beside a host's (e.g. a
-- test schema on a shared database) never reads or locks host tables.

CREATE TABLE tracker_actors (
    id            BIGSERIAL   PRIMARY KEY,
    external_ref  TEXT        NOT NULL UNIQUE,
    display_label TEXT        NOT NULL DEFAULT '',
    email         TEXT,
    kind          TEXT        NOT NULL DEFAULT 'user',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT tracker_actors_kind_chk CHECK (kind IN ('user', 'system', 'connector'))
);

DO $$
BEGIN
    IF to_regclass(format('%I.users', current_schema())) IS NOT NULL THEN
        INSERT INTO tracker_actors (id, external_ref, display_label, email, kind)
        SELECT u.id, 'alitheia:user:' || u.id, u.email, u.email, 'user'
          FROM users u
         WHERE u.id IN (
                SELECT verified_by FROM fraud_cases
                UNION SELECT verified_by FROM fraud_claims
                UNION SELECT reviewer    FROM fraud_claim_reviews
                UNION SELECT decided_by  FROM fraud_case_match_candidates
                UNION SELECT checked_by  FROM fraud_appeal_checks)
         ORDER BY u.id
        ON CONFLICT DO NOTHING;
        -- Each statement is planned only when reached, so a host without a
        -- revision log never touches entity_revisions.
        IF to_regclass(format('%I.entity_revisions', current_schema())) IS NOT NULL THEN
            INSERT INTO tracker_actors (id, external_ref, display_label, email, kind)
            SELECT u.id, 'alitheia:user:' || u.id, u.email, u.email, 'user'
              FROM users u
             WHERE u.id IN (SELECT actor_user_id FROM entity_revisions
                             WHERE entity_kind = 'fraud_case')
             ORDER BY u.id
            ON CONFLICT DO NOTHING;
        END IF;
    END IF;
END $$;

SELECT setval(pg_get_serial_sequence('tracker_actors', 'id'),
              COALESCE((SELECT max(id) FROM tracker_actors), 1),
              (SELECT max(id) FROM tracker_actors) IS NOT NULL);

-- Repoint the five actor columns. The DROPs are no-ops on a fresh database
-- (the baseline never had host FKs).
ALTER TABLE fraud_cases                 DROP CONSTRAINT IF EXISTS fraud_cases_verified_by_fkey;
ALTER TABLE fraud_claims                DROP CONSTRAINT IF EXISTS fraud_claims_verified_by_fkey;
ALTER TABLE fraud_claim_reviews         DROP CONSTRAINT IF EXISTS fraud_claim_reviews_reviewer_fkey;
ALTER TABLE fraud_case_match_candidates DROP CONSTRAINT IF EXISTS fraud_case_match_candidates_decided_by_fkey;
ALTER TABLE fraud_appeal_checks         DROP CONSTRAINT IF EXISTS fraud_appeal_checks_checked_by_fkey;

-- ON DELETE SET NULL mirrors the old users FKs. Actors are never deleted
-- (accounts are disabled instead); on fraud_claim_reviews a delete would be
-- refused by the append-only trigger anyway.
ALTER TABLE fraud_cases
    ADD CONSTRAINT fraud_cases_verified_by_fkey FOREIGN KEY (verified_by)
    REFERENCES tracker_actors(id) ON DELETE SET NULL;
ALTER TABLE fraud_claims
    ADD CONSTRAINT fraud_claims_verified_by_fkey FOREIGN KEY (verified_by)
    REFERENCES tracker_actors(id) ON DELETE SET NULL;
ALTER TABLE fraud_claim_reviews
    ADD CONSTRAINT fraud_claim_reviews_reviewer_fkey FOREIGN KEY (reviewer)
    REFERENCES tracker_actors(id) ON DELETE SET NULL;
ALTER TABLE fraud_case_match_candidates
    ADD CONSTRAINT fraud_case_match_candidates_decided_by_fkey FOREIGN KEY (decided_by)
    REFERENCES tracker_actors(id) ON DELETE SET NULL;
ALTER TABLE fraud_appeal_checks
    ADD CONSTRAINT fraud_appeal_checks_checked_by_fkey FOREIGN KEY (checked_by)
    REFERENCES tracker_actors(id) ON DELETE SET NULL;
