-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

-- 0003: fraud_case_revisions — the tracker-owned case-edit audit trail.
--
-- Replaces the fraud_case rows a host kept in its generic revision log
-- (alitheia: entity_revisions, entity_kind = 'fraud_case'). Same row shape
-- and vocabulary, actors instead of host users. Append-only, enforced by
-- trigger like fraud_claim_reviews (0001 / alitheia 0117): this is the
-- legally load-bearing record of who changed what about a case.
--
-- case_id deliberately has NO foreign key: like the log it replaces, rows
-- outlive merged-away cases (and a cascading delete would be refused by the
-- append-only trigger).
--
-- EMBEDDED-HOST ADOPTION: on a database with entity_revisions, the
-- fraud_case rows are copied in id order (so newest-first ordering and its
-- id tie-break are preserved). Their actor_user_id values are valid actor
-- ids because 0002 backfilled those users with actor.id = users.id. The
-- host's rows are left in place; removing them is the host's own migration.

CREATE TABLE fraud_case_revisions (
    id           BIGSERIAL   PRIMARY KEY,
    case_id      BIGINT      NOT NULL,
    field_name   TEXT        NOT NULL DEFAULT '',
    before_json  JSONB       NOT NULL DEFAULT '{}'::jsonb,
    after_json   JSONB       NOT NULL DEFAULT '{}'::jsonb,
    actor_id     BIGINT      REFERENCES tracker_actors(id) ON DELETE SET NULL,
    actor_kind   TEXT        NOT NULL,
    actor_source TEXT        NOT NULL DEFAULT '',
    change_kind  TEXT        NOT NULL DEFAULT 'update',
    note         TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fcr_actor_kind_chk CHECK (actor_kind IN ('admin', 'ingester', 'system')),
    CONSTRAINT fcr_change_kind_chk CHECK (change_kind IN (
        'create', 'update', 'delete', 'lock', 'unlock', 'suppressed_write'))
);
CREATE INDEX fcr_case_idx ON fraud_case_revisions (case_id, created_at DESC, id DESC);

DO $$
BEGIN
    IF to_regclass(format('%I.entity_revisions', current_schema())) IS NOT NULL THEN
        IF EXISTS (SELECT 1 FROM entity_revisions
                    WHERE entity_kind = 'fraud_case' AND correction_id IS NOT NULL) THEN
            RAISE EXCEPTION 'fraud_case revisions carry correction_id; refusing to drop it silently';
        END IF;
        INSERT INTO fraud_case_revisions
            (case_id, field_name, before_json, after_json, actor_id, actor_kind,
             actor_source, change_kind, note, created_at)
        SELECT entity_id, field_name, before_json, after_json, actor_user_id, actor_kind,
               actor_source, change_kind, note, created_at
          FROM entity_revisions
         WHERE entity_kind = 'fraud_case'
         ORDER BY id;
    END IF;
END $$;

CREATE OR REPLACE FUNCTION fraud_case_revisions_block_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'fraud_case_revisions is append-only (% blocked)', TG_OP;
END;
$$;

CREATE TRIGGER fraud_case_revisions_no_update
    BEFORE UPDATE ON fraud_case_revisions
    FOR EACH ROW EXECUTE FUNCTION fraud_case_revisions_block_mutation();

CREATE TRIGGER fraud_case_revisions_no_delete
    BEFORE DELETE ON fraud_case_revisions
    FOR EACH ROW EXECUTE FUNCTION fraud_case_revisions_block_mutation();
