-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

-- Dev-only. Drops the actor FKs and the directory; it does NOT restore any
-- host FKs (the tracker never owned those).
ALTER TABLE fraud_cases                 DROP CONSTRAINT IF EXISTS fraud_cases_verified_by_fkey;
ALTER TABLE fraud_claims                DROP CONSTRAINT IF EXISTS fraud_claims_verified_by_fkey;
ALTER TABLE fraud_claim_reviews         DROP CONSTRAINT IF EXISTS fraud_claim_reviews_reviewer_fkey;
ALTER TABLE fraud_case_match_candidates DROP CONSTRAINT IF EXISTS fraud_case_match_candidates_decided_by_fkey;
ALTER TABLE fraud_appeal_checks         DROP CONSTRAINT IF EXISTS fraud_appeal_checks_checked_by_fkey;
DROP TABLE IF EXISTS tracker_actors;
