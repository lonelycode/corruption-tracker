-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

-- 0006: tracker_instance and tracker_connector_state.
--
-- tracker_instance is a singleton describing this installation
-- (design §11.1): its stable id (recorded as
-- source.instance_id in export bundles), the handoff mode, and the global
-- ingestion pause the scheduler honours (§9.3). A missing row reads as
-- mode 'normal', not paused, so test harnesses may truncate it; the store
-- re-creates it on demand. The freeze write guard (§11.2) that consults
-- `mode` is a later migration.
--
-- tracker_connector_state holds per-connector operational state
-- (§10.4): whether an operator enabled it, and resume bookkeeping. It IS
-- dataset (exported in bundles) so a successor resumes where the source
-- left off. A connector without a row is enabled.

CREATE TABLE tracker_instance (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    mode              TEXT        NOT NULL DEFAULT 'normal'
        CHECK (mode IN ('normal', 'frozen', 'handed_off')),
    freeze_id         UUID,
    frozen_at         TIMESTAMPTZ,
    frozen_by         TEXT,
    handoff_export_id UUID,
    successor_url     TEXT,
    imported_from     JSONB,
    ingestion_paused  BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one row.
CREATE UNIQUE INDEX tracker_instance_singleton ON tracker_instance ((true));

INSERT INTO tracker_instance DEFAULT VALUES;

CREATE TABLE tracker_connector_state (
    connector       TEXT        PRIMARY KEY, -- agency:<slug> | classify-news | mail | newsfeed:<id>
    enabled         BOOLEAN     NOT NULL DEFAULT TRUE,
    last_run_at     TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    resume_from     TIMESTAMPTZ,
    catch_up        BOOLEAN     NOT NULL DEFAULT FALSE,
    state           JSONB       NOT NULL DEFAULT '{}'::jsonb
);

-- A scheduled job whose connectors an operator disabled is recorded as
-- skipped_disabled, not run.
ALTER TABLE tracker_job_runs DROP CONSTRAINT tracker_job_runs_status_check;
ALTER TABLE tracker_job_runs ADD CONSTRAINT tracker_job_runs_status_check
    CHECK (status IN ('running', 'ok', 'error',
                      'skipped_running', 'skipped_paused', 'skipped_unconfigured', 'skipped_disabled'));
