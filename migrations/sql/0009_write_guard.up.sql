-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

-- 0009: the freeze write guard (design §11.2).
--
-- While tracker_instance.mode is anything but 'normal' (a handoff froze the
-- instance, or it was handed off), every write to a dataset table fails
-- with read_only_sql_transaction — enforced here, in the database, so no
-- code path (CLI, web, a job, an operator's psql) can slip a write past a
-- freeze. Handlers and jobs check the mode too, but only for UX (§11.3).
--
-- Every writer statement first takes the shared side of the FTKR
-- "write barrier" advisory lock (classid 0x46544b52, objid 1). The freeze
-- transaction takes the exclusive side, so it waits for every in-flight
-- writer transaction to finish, and writers arriving later queue behind
-- it and then see 'frozen'. Every claim therefore either committed before
-- the freeze (and is in any bundle taken afterwards) or failed on the
-- source (its dedupe key never written, so the target re-ingests it).
--
-- The importer bypasses the guard with
-- SET LOCAL corruption_tracker.allow_frozen_write = 'on'.
--
-- Operational tables are NOT guarded, so reads, logins, the status page
-- and job bookkeeping keep working while frozen: tracker_job_runs,
-- tracker_access_log, tracker_users, tracker_sessions, tracker_settings,
-- tracker_instance. A missing tracker_instance row reads as 'normal'.

CREATE FUNCTION tracker_write_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF current_setting('corruption_tracker.allow_frozen_write', true) = 'on' THEN RETURN NULL; END IF;
  PERFORM pg_advisory_xact_lock_shared(1179929426, 1);
  IF COALESCE((SELECT mode FROM tracker_instance LIMIT 1), 'normal') <> 'normal' THEN
    RAISE EXCEPTION 'corruption-tracker is % — dataset is read-only', (SELECT mode FROM tracker_instance LIMIT 1)
      USING ERRCODE = 'read_only_sql_transaction';
  END IF;
  RETURN NULL;
END $$;

DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'tracker_actors',
    'fraud_typologies', 'fraud_agencies', 'fraud_regions', 'fraud_profession_categories', 'fraud_sectors',
    'fraud_persons', 'fraud_organisations',
    'fraud_cases',
    'fraud_case_typologies', 'fraud_case_agencies', 'fraud_case_participants', 'fraud_case_organisations',
    'fraud_sources',
    'fraud_case_events',
    'fraud_claims', 'fraud_claim_reviews', 'fraud_case_match_candidates',
    'fraud_appeal_checks', 'fraud_report_snapshots', 'fraud_case_revisions',
    'tracker_connector_state', 'tracker_news_items'
  ] LOOP
    EXECUTE format(
      'CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION tracker_write_guard()',
      t || '_write_guard', t);
  END LOOP;
END $$;
