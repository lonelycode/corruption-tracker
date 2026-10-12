-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

-- Dev-only.
DELETE FROM tracker_job_runs WHERE status = 'skipped_disabled';
ALTER TABLE tracker_job_runs DROP CONSTRAINT tracker_job_runs_status_check;
ALTER TABLE tracker_job_runs ADD CONSTRAINT tracker_job_runs_status_check
    CHECK (status IN ('running', 'ok', 'error',
                      'skipped_running', 'skipped_paused', 'skipped_unconfigured'));
DROP TABLE IF EXISTS tracker_connector_state;
DROP TABLE IF EXISTS tracker_instance;
