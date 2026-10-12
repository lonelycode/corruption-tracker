-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

-- Dev-only: drops the whole tracker schema. Standalone never runs down
-- migrations; on alitheia this would delete the tracker's data.
DROP VIEW IF EXISTS fraud_case_lags;
DROP TABLE IF EXISTS
    fraud_appeal_checks, fraud_report_snapshots,
    fraud_case_match_candidates, fraud_claim_reviews, fraud_claims,
    fraud_case_events, fraud_sources, fraud_case_organisations,
    fraud_case_participants, fraud_case_agencies, fraud_case_typologies,
    fraud_cases, fraud_organisations, fraud_persons,
    fraud_sectors, fraud_profession_categories, fraud_regions,
    fraud_agencies, fraud_typologies;
DROP FUNCTION IF EXISTS fraud_claim_reviews_block_mutation();
