-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

-- 0004: tracker_job_runs — one row per scheduled job attempt.
--
-- Written by the in-process scheduler (corruption-tracker/scheduler; see
-- design §9.2). A host that schedules the jobs
-- itself (alitheia: launchd) never writes here. Operational state, not
-- dataset: it is exempt from the freeze write guard and survives a freeze
-- (§11), and the handoff drain waits for no row to be 'running'.
--
-- Skipped attempts are recorded too, so an operator can see why a job did
-- not run (already running, ingestion paused, or a needed setting absent).
-- counters holds the run's stats as the CLI reports them (fetched / new).

CREATE TABLE tracker_job_runs (
    id         BIGSERIAL   PRIMARY KEY,
    job        TEXT        NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at   TIMESTAMPTZ,
    status     TEXT        NOT NULL
        CHECK (status IN ('running', 'ok', 'error',
                          'skipped_running', 'skipped_paused', 'skipped_unconfigured')),
    error      TEXT        NOT NULL DEFAULT '',
    counters   JSONB       NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX tracker_job_runs_job_started_idx ON tracker_job_runs (job, started_at DESC);
CREATE INDEX tracker_job_runs_running_idx ON tracker_job_runs (job) WHERE status = 'running';
