-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

-- 0001: corruption & fraud tracker — baseline schema.
--
-- The tracker's own migration track starts here (golang-migrate table
-- tracker_schema_migrations; see design §8). This
-- baseline is the NET EFFECT of the fraud_* DDL that alitheia's migration
-- track applied in 0116–0144, reproduced verbatim (comments included) in the
-- original order, so a fresh database ends up with exactly the schema an
-- alitheia-built database has. Differences, all deliberate:
--
--   - The five reviewer/actor columns (fraud_cases.verified_by,
--     fraud_claims.verified_by, fraud_claim_reviews.reviewer,
--     fraud_case_match_candidates.decided_by, fraud_appeal_checks.checked_by)
--     are plain BIGINT here: a standalone database has no users table.
--     alitheia-built databases still carry FKs to users(id) until 0002
--     repoints them at the tracker's own actor directory.
--   - alitheia 0118 (team_feature_flags / team_feature_access_log) and 0143
--     (users.role 'fraud_admin') are NOT part of the tracker: they are
--     alitheia's org/identity model.
--   - alitheia 0129 and 0135 were data-only clean-ups of existing rows; on a
--     fresh database they are no-ops, so they are omitted.
--   - pg_trgm (word_similarity in case matching) is created here; alitheia
--     created it in its own 0014.
--
-- On a database that already has these tables (alitheia dev/prod), this
-- file is never executed: migrations.Adopt stamps version 1 instead.

CREATE EXTENSION IF NOT EXISTS pg_trgm;


-- ════════════════ from alitheia migration 0116_fraud_case_tracker.up.sql ════════════════

-- 0116: corruption & fraud case tracker — core schema (legal-safety critical).
--
-- A structured replacement for the Fraud/Bribery/Corruption spreadsheet:
-- case-centric (persons are participants, never the unit of analysis),
-- lifecycle tracked as events, all ingestion review-gated. Full design in
-- the original tracker design notes.
--
-- HARD SEPARATION INVARIANT: fraud_persons / fraud_organisations are
-- deliberately NOT foreign-keyed to the political people / organisations
-- tables. Joining conviction data to the politician dataset is the single
-- highest-risk operation this platform could perform; if ever wanted it is
-- a deliberate, legally-reviewed feature, not a schema convenience. No
-- migration may add such an FK without that review.
--
-- Output is never public: these tables feed an org-flag-gated weekly report
-- only. excerpt columns are PRIVATE and never selected by a public surface.
--
-- Table groups:
--   taxonomies   — fraud_typologies / _agencies / _regions /
--                  _profession_categories / _sectors, each with an aliases
--                  array so free-text inputs map deterministically.
--   core         — fraud_cases, fraud_case_participants, fraud_persons,
--                  fraud_organisations, fraud_case_organisations,
--                  fraud_case_events (lifecycle spine), fraud_sources
--                  (mandatory provenance).
--   review queue — fraud_claims, fraud_claim_reviews (append-only; trigger
--                  in 0117), fraud_case_match_candidates (claim → case
--                  attachment, never auto-confirmed).
--
-- Legal invariants and where they live (DB vs Go):
--   - enum domains                  -> DB CHECK constraints (this file).
--   - case status derived from latest material event, never hand-edited
--                                   -> Go (RecomputeCaseStatus); DB keeps the
--                                      column for query efficiency only.
--   - fraud_claim_reviews append-only -> DB trigger (migration 0117).
--   - pre-verdict participants unnamed by default in any report output
--                                   -> naming_approved gate (this file) +
--                                      template layer.
--   - verified claim requires reviewer + attestation
--                                   -> DB CHECK backstop + Go guard.
--   - dedupe identical source capture -> DB UNIQUE (case_id, content_hash)
--                                      and (claim) dedupe_key partial-unique.

-- ──────────────────────────── taxonomies ────────────────────────────
-- Shared shape: slug is the stable identifier; aliases hold lowercased
-- free-text variants seen in the wild (spreadsheet vocabulary, agency
-- spelling variants) so mapping is deterministic and auditable.

CREATE TABLE fraud_typologies (
    id      BIGSERIAL PRIMARY KEY,
    slug    TEXT      NOT NULL UNIQUE,
    label   TEXT      NOT NULL,
    aliases TEXT[]    NOT NULL DEFAULT '{}',
    active  BOOLEAN   NOT NULL DEFAULT true,
    sort    INT       NOT NULL DEFAULT 0
);

CREATE TABLE fraud_agencies (
    id      BIGSERIAL PRIMARY KEY,
    slug    TEXT      NOT NULL UNIQUE,
    label   TEXT      NOT NULL,
    aliases TEXT[]    NOT NULL DEFAULT '{}',
    active  BOOLEAN   NOT NULL DEFAULT true,
    sort    INT       NOT NULL DEFAULT 0
);

CREATE TABLE fraud_regions (
    id      BIGSERIAL PRIMARY KEY,
    slug    TEXT      NOT NULL UNIQUE,
    label   TEXT      NOT NULL,
    aliases TEXT[]    NOT NULL DEFAULT '{}',
    active  BOOLEAN   NOT NULL DEFAULT true,
    sort    INT       NOT NULL DEFAULT 0
);

CREATE TABLE fraud_profession_categories (
    id      BIGSERIAL PRIMARY KEY,
    slug    TEXT      NOT NULL UNIQUE,
    label   TEXT      NOT NULL,
    aliases TEXT[]    NOT NULL DEFAULT '{}',
    active  BOOLEAN   NOT NULL DEFAULT true,
    sort    INT       NOT NULL DEFAULT 0
);

CREATE TABLE fraud_sectors (
    id      BIGSERIAL PRIMARY KEY,
    slug    TEXT      NOT NULL UNIQUE,
    label   TEXT      NOT NULL,
    aliases TEXT[]    NOT NULL DEFAULT '{}',
    active  BOOLEAN   NOT NULL DEFAULT true,
    sort    INT       NOT NULL DEFAULT 0
);

-- ──────────────────────────── persons / orgs ────────────────────────────
-- Minimal PII by design: name + aliases only. No DOB, no address, no
-- contact data, no FK to the political roster (see header invariant).

CREATE TABLE fraud_persons (
    id         BIGSERIAL   PRIMARY KEY,
    full_name  TEXT        NOT NULL,
    aliases    JSONB       NOT NULL DEFAULT '[]',
    notes      TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX fraud_persons_name_idx ON fraud_persons (lower(full_name));

CREATE TABLE fraud_organisations (
    id         BIGSERIAL   PRIMARY KEY,
    name       TEXT        NOT NULL,
    nzbn       TEXT,
    notes      TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX fraud_organisations_name_idx ON fraud_organisations (lower(name));

-- ──────────────────────────── fraud_cases ────────────────────────────
-- The unit of analysis. status is DERIVED from the latest material
-- fraud_case_events row by Go (RecomputeCaseStatus) — never hand-edited;
-- it exists as a column purely so reporting queries don't need a lateral
-- join on every aggregate.

CREATE TABLE fraud_cases (
    id                         BIGSERIAL   PRIMARY KEY,
    case_name                  TEXT        NOT NULL,      -- internal label, never published
    status                     TEXT        NOT NULL DEFAULT 'charges_laid',

    typology_id                BIGINT      REFERENCES fraud_typologies(id) ON DELETE SET NULL,
    typology_raw               TEXT        NOT NULL DEFAULT '',  -- original free text, provenance
    sector_id                  BIGINT      REFERENCES fraud_sectors(id) ON DELETE SET NULL,
    region_id                  BIGINT      REFERENCES fraud_regions(id) ON DELETE SET NULL,
    region_raw                 TEXT        NOT NULL DEFAULT '',
    prosecuting_agency_id      BIGINT      REFERENCES fraud_agencies(id) ON DELETE SET NULL,
    agency_raw                 TEXT        NOT NULL DEFAULT '',

    total_amount_nzd           NUMERIC(14,2),
    amount_basis               TEXT,                      -- alleged | proven | reparations_ordered
    amount_note                TEXT        NOT NULL DEFAULT '',

    offending_period_start     DATE,
    offending_period_end       DATE,
    offending_period_precision TEXT,                      -- day | month | year

    court_name                 TEXT        NOT NULL DEFAULT '',
    court_level                TEXT        NOT NULL DEFAULT '',

    victim_class               TEXT,
    victim_detail              TEXT        NOT NULL DEFAULT '',
    victim_count               INT,

    covid_related              BOOLEAN     NOT NULL DEFAULT false,
    summary                    TEXT        NOT NULL DEFAULT '',

    -- Pre-verdict cases default to unnamed in ALL report output; a reviewer
    -- must explicitly confirm no suppression order applies before flipping
    -- this. The template layer additionally withholds names while status is
    -- pre-verdict regardless of this flag unless it is set.
    naming_approved            BOOLEAN     NOT NULL DEFAULT false,

    review_state               TEXT        NOT NULL DEFAULT 'unverified',
    verified_by                BIGINT,
    verified_at                TIMESTAMPTZ,

    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT fraud_cases_status_chk CHECK (status IN (
        'charges_laid', 'before_court', 'convicted', 'acquitted',
        'appeal_pending', 'overturned', 'upheld', 'discontinued')),
    CONSTRAINT fraud_cases_amount_basis_chk CHECK (amount_basis IS NULL OR amount_basis IN (
        'alleged', 'proven', 'reparations_ordered')),
    CONSTRAINT fraud_cases_offending_precision_chk CHECK (offending_period_precision IS NULL
        OR offending_period_precision IN ('day', 'month', 'year')),
    CONSTRAINT fraud_cases_victim_class_chk CHECK (victim_class IS NULL OR victim_class IN (
        'taxpayer_central_govt', 'local_govt', 'employer', 'investors',
        'consumers', 'individuals', 'charity', 'iwi', 'mixed')),
    CONSTRAINT fraud_cases_review_state_chk CHECK (review_state IN (
        'unverified', 'verified')),
    CONSTRAINT fraud_cases_verified_needs_reviewer_chk CHECK (
        review_state <> 'verified'
        OR (verified_by IS NOT NULL AND verified_at IS NOT NULL))
);
CREATE INDEX fraud_cases_status_idx   ON fraud_cases (status);
CREATE INDEX fraud_cases_typology_idx ON fraud_cases (typology_id);
CREATE INDEX fraud_cases_region_idx   ON fraud_cases (region_id);
CREATE INDEX fraud_cases_agency_idx   ON fraud_cases (prosecuting_agency_id);

-- Secondary typologies / co-prosecuting agencies (m2m).
CREATE TABLE fraud_case_typologies (
    case_id     BIGINT NOT NULL REFERENCES fraud_cases(id) ON DELETE CASCADE,
    typology_id BIGINT NOT NULL REFERENCES fraud_typologies(id) ON DELETE CASCADE,
    PRIMARY KEY (case_id, typology_id)
);
CREATE TABLE fraud_case_agencies (
    case_id   BIGINT NOT NULL REFERENCES fraud_cases(id) ON DELETE CASCADE,
    agency_id BIGINT NOT NULL REFERENCES fraud_agencies(id) ON DELETE CASCADE,
    PRIMARY KEY (case_id, agency_id)
);

-- ──────────────────────── fraud_case_participants ───────────────────────
CREATE TABLE fraud_case_participants (
    id                     BIGSERIAL   PRIMARY KEY,
    case_id                BIGINT      NOT NULL REFERENCES fraud_cases(id) ON DELETE CASCADE,
    person_id              BIGINT      NOT NULL REFERENCES fraud_persons(id) ON DELETE CASCADE,
    role                   TEXT        NOT NULL DEFAULT 'defendant',

    profession_raw         TEXT        NOT NULL DEFAULT '',
    profession_category_id BIGINT      REFERENCES fraud_profession_categories(id) ON DELETE SET NULL,
    gender                 TEXT        NOT NULL DEFAULT '',   -- 'm' | 'f' | '' (as sourced; analysis only)

    individual_charges     TEXT        NOT NULL DEFAULT '',
    -- Only set when distinguishable from the case total; aggregates use the
    -- case-level total_amount_nzd to avoid co-accused multi-counting.
    individual_amount_nzd  NUMERIC(14,2),

    outcome                TEXT,                              -- NULL while pre-verdict
    sentence_type          TEXT,
    sentence_detail        TEXT        NOT NULL DEFAULT '',
    sentence_months        INT,                               -- custodial component only

    suppression_flag       BOOLEAN     NOT NULL DEFAULT false,
    clean_slate_possible   BOOLEAN     NOT NULL DEFAULT false, -- recomputed on a schedule

    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT fcp_role_chk CHECK (role IN ('defendant', 'co_accused', 'named_associate')),
    CONSTRAINT fcp_outcome_chk CHECK (outcome IS NULL OR outcome IN (
        'convicted', 'acquitted', 'charges_withdrawn', 'conviction_overturned')),
    CONSTRAINT fcp_sentence_type_chk CHECK (sentence_type IS NULL OR sentence_type IN (
        'imprisonment', 'home_detention', 'community_detention', 'community_work',
        'fine', 'reparation_only', 'discharge', 'deportation_followed')),
    UNIQUE (case_id, person_id)
);
CREATE INDEX fcp_case_idx   ON fraud_case_participants (case_id);
CREATE INDEX fcp_person_idx ON fraud_case_participants (person_id);

-- ──────────────────────── fraud_case_organisations ──────────────────────
CREATE TABLE fraud_case_organisations (
    case_id         BIGINT NOT NULL REFERENCES fraud_cases(id) ON DELETE CASCADE,
    organisation_id BIGINT NOT NULL REFERENCES fraud_organisations(id) ON DELETE CASCADE,
    role            TEXT   NOT NULL,
    CONSTRAINT fco_role_chk CHECK (role IN (
        'vehicle', 'victim', 'employer_of_offender', 'regulator')),
    PRIMARY KEY (case_id, organisation_id, role)
);

-- ──────────────────────────── fraud_sources ────────────────────────────
-- Mandatory provenance, attached at case level; events point at the source
-- that evidences them. excerpt is PRIVATE: no public surface selects it.
-- licence_basis records the per-source determination (Crown copyright /
-- NZGOAL CC-BY / fair-report / manual) made at connector build time.
CREATE TABLE fraud_sources (
    id             BIGSERIAL   PRIMARY KEY,
    case_id        BIGINT      NOT NULL REFERENCES fraud_cases(id) ON DELETE CASCADE,
    url            TEXT        NOT NULL DEFAULT '',
    publisher      TEXT        NOT NULL DEFAULT '',
    source_type    TEXT        NOT NULL,
    licence_basis  TEXT        NOT NULL DEFAULT '',
    published_date DATE,
    retrieved_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    excerpt        TEXT        NOT NULL DEFAULT '',   -- PRIVATE; never republished
    content_hash   TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fraud_sources_type_chk CHECK (source_type IN (
        'agency_release', 'news', 'judgment', 'manual')),
    UNIQUE (case_id, content_hash)
);
CREATE INDEX fraud_sources_case_idx ON fraud_sources (case_id);

-- ──────────────────────────── fraud_case_events ─────────────────────────
-- The lifecycle spine. Case status is derived from the latest material
-- event here. The model tolerates gaps: a case may jump straight from
-- 'convicted' to an appeal outcome with no appeal_filed event (appeal
-- filings are seldom announced anywhere pollable).
CREATE TABLE fraud_case_events (
    id                   BIGSERIAL   PRIMARY KEY,
    case_id              BIGINT      NOT NULL REFERENCES fraud_cases(id) ON DELETE CASCADE,
    event_type           TEXT        NOT NULL,
    event_date           DATE,
    event_date_precision TEXT,
    court                TEXT        NOT NULL DEFAULT '',
    outcome_detail       TEXT        NOT NULL DEFAULT '',
    source_id            BIGINT      REFERENCES fraud_sources(id) ON DELETE SET NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fce_type_chk CHECK (event_type IN (
        'charges_laid', 'first_appearance', 'plea', 'trial_start', 'verdict',
        'sentencing', 'appeal_filed', 'appeal_heard', 'appeal_outcome',
        'retrial', 'discontinued')),
    CONSTRAINT fce_precision_chk CHECK (event_date_precision IS NULL
        OR event_date_precision IN ('day', 'month', 'year'))
);
CREATE INDEX fce_case_idx ON fraud_case_events (case_id, event_date);

-- ──────────────────────────── fraud_claims ────────────────────────────
-- The review queue: every connector output and manual submission lands here
-- as an UNVERIFIED claim. Nothing reaches fraud_cases without a reviewer
-- approving the claim (which promotes it into a new case or attaches it as
-- an event on an existing one). Same state machine as conviction_claims.
CREATE TABLE fraud_claims (
    id                      BIGSERIAL   PRIMARY KEY,

    -- Set when a reviewer confirms attachment (existing case) or on promote
    -- (new case). ON DELETE SET NULL: the claim record outlives the case.
    case_id                 BIGINT      REFERENCES fraud_cases(id) ON DELETE SET NULL,
    match_status            TEXT        NOT NULL DEFAULT 'unmatched',

    -- Full structured extraction (fraud_case_v1 schema); reviewer edits are
    -- applied at promote time, the original payload is never rewritten.
    payload                 JSONB       NOT NULL DEFAULT '{}',
    headline                TEXT        NOT NULL DEFAULT '',  -- queue display only
    confidence_score        REAL,

    review_state            TEXT        NOT NULL DEFAULT 'pending',
    suppression_flag        BOOLEAN     NOT NULL DEFAULT false,
    needs_manual_extraction BOOLEAN     NOT NULL DEFAULT false,

    verified_by             BIGINT,
    verified_at             TIMESTAMPTZ,
    attestation_note        TEXT,

    -- Inline provenance for the single capture that produced this claim.
    source_url              TEXT        NOT NULL DEFAULT '',
    source_publisher        TEXT        NOT NULL DEFAULT '',
    source_type             TEXT        NOT NULL,
    licence_basis           TEXT        NOT NULL DEFAULT '',
    published_date          DATE,
    retrieved_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    excerpt                 TEXT        NOT NULL DEFAULT '',   -- PRIVATE
    content_hash            TEXT        NOT NULL,

    dedupe_key              TEXT,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at              TIMESTAMPTZ,

    CONSTRAINT fraud_claims_review_state_chk CHECK (review_state IN (
        'pending', 'in_review', 'verified', 'rejected',
        'needs_more_info', 'quarantined_suppression')),
    CONSTRAINT fraud_claims_match_status_chk CHECK (match_status IN (
        'unmatched', 'candidates_pending', 'confirmed', 'no_match')),
    CONSTRAINT fraud_claims_source_type_chk CHECK (source_type IN (
        'agency_release', 'news', 'judgment', 'manual')),
    CONSTRAINT fraud_claims_verified_needs_reviewer_chk CHECK (
        review_state <> 'verified'
        OR (verified_by IS NOT NULL AND verified_at IS NOT NULL))
);
CREATE INDEX fraud_claims_state_idx ON fraud_claims (review_state)
    WHERE deleted_at IS NULL;
CREATE INDEX fraud_claims_case_idx ON fraud_claims (case_id)
    WHERE deleted_at IS NULL AND case_id IS NOT NULL;
-- Re-ingestion of the same matter collides instead of duplicating; rejected
-- claims keep deleted_at NULL so they too block resurfacing.
CREATE UNIQUE INDEX fraud_claims_dedupe_uq ON fraud_claims (dedupe_key)
    WHERE dedupe_key IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX fraud_claims_hash_idx ON fraud_claims (content_hash);

-- ─────────────────────── fraud_claim_reviews ───────────────────────
-- APPEND-ONLY immutable audit; one row per state change, in the same
-- transaction as the fraud_claims UPDATE. Migration 0117 installs the
-- UPDATE/DELETE-blocking trigger.
CREATE TABLE fraud_claim_reviews (
    id             BIGSERIAL   PRIMARY KEY,
    claim_id       BIGINT      NOT NULL REFERENCES fraud_claims(id) ON DELETE RESTRICT,
    reviewer       BIGINT,
    reviewer_label TEXT        NOT NULL DEFAULT '',
    action         TEXT        NOT NULL,
    from_state     TEXT,
    to_state       TEXT        NOT NULL,
    note           TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fraud_claim_reviews_action_chk CHECK (action IN (
        'created', 'start_review', 'verify', 'reject', 'request_info',
        'quarantine', 'quarantine_release_vote', 'release',
        'edit_fields', 'match_confirm', 'match_reject', 'promote'))
);
CREATE INDEX fraud_claim_reviews_claim_idx ON fraud_claim_reviews (claim_id, created_at);

-- ─────────────────── fraud_case_match_candidates ───────────────────
-- Candidate existing cases a claim might belong to (org name + amount +
-- region + typology similarity), so an appeal outcome attaches as an event
-- on an existing case rather than becoming a duplicate case. NEVER
-- auto-confirmed; a human decides every attachment.
CREATE TABLE fraud_case_match_candidates (
    id           BIGSERIAL   PRIMARY KEY,
    claim_id     BIGINT      NOT NULL REFERENCES fraud_claims(id) ON DELETE CASCADE,
    case_id      BIGINT      NOT NULL REFERENCES fraud_cases(id) ON DELETE CASCADE,
    match_score  REAL        NOT NULL,
    match_method TEXT        NOT NULL,
    context_note TEXT        NOT NULL DEFAULT '',
    decision     TEXT        NOT NULL DEFAULT 'pending',
    decided_by   BIGINT,
    decided_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fcmc_method_chk CHECK (match_method IN (
        'person_name', 'org_name', 'amount_region', 'typology_window', 'manual')),
    CONSTRAINT fcmc_decision_chk CHECK (decision IN ('pending', 'confirmed', 'rejected')),
    UNIQUE (claim_id, case_id)
);
CREATE INDEX fcmc_claim_idx ON fraud_case_match_candidates (claim_id);
-- At most one confirmed case attachment per claim.
CREATE UNIQUE INDEX fcmc_one_confirmed_uq ON fraud_case_match_candidates (claim_id)
    WHERE decision = 'confirmed';

-- ──────────────────────────── taxonomy seeds ────────────────────────────
-- Typologies: the ~15 real typologies clustered out of the spreadsheet's
-- 98 free-text values. Aliases are lowercased; resolvers must lowercase +
-- trim input before lookup. Unmapped values land on 'other' with the raw
-- text preserved in typology_raw for later alias promotion.

INSERT INTO fraud_typologies (slug, label, aliases, sort) VALUES
('tax-fraud',            'Tax fraud & evasion',
 '{"tax fraud","tax evasion","gst fraud","gst refund fraud","tax refund fraud","income tax evasion"}', 10),
('covid-subsidy-fraud',  'COVID & wage-subsidy fraud',
 '{"covid fraud","covid subsidy fraud","covid-19 fraud","wage subsidy fraud","covid wage subsidy fraud","covid 19 wage subsidy fraud","covid relief fraud","cssp fraud"}', 20),
('benefit-fraud',        'Benefit fraud',
 '{"benefit fraud","welfare fraud","msd fraud","social welfare fraud"}', 30),
('acc-health-fraud',     'ACC & health-claims fraud',
 '{"acc fraud","acc fake travel reimbursement claims","health insurance fraud","medical claims fraud"}', 40),
('employer-theft',       'Theft / fraud against employer',
 '{"theft from employer","employee fraud","theft as a servant","payroll fraud","invoice fraud against employer","embezzlement"}', 50),
('mortgage-property-fraud', 'Mortgage & property fraud',
 '{"mortgage fraud","property fraud","real estate fraud","mortgage application fraud"}', 60),
('investment-fraud',     'Investment & ponzi fraud',
 '{"investment fraud","ponzi scheme","ponzi","investor fraud","forex fraud","cryptocurrency fraud","crypto fraud","share scam"}', 70),
('charity-trust-fraud',  'Charity & trust fraud',
 '{"charitable trust fraud","charity fraud","trust fraud","church fraud","iwi trust fraud"}', 80),
('public-sector-procurement', 'Public-sector & procurement corruption',
 '{"local government procurement fraud","procurement fraud","council fraud","public sector fraud","corruption in public office","misuse of public funds"}', 90),
('bribery-corruption',   'Bribery & secret commissions',
 '{"bribery","bribery, corruption","corruption","secret commissions","bribery of public official","fraud, bribery of public official","kickbacks"}', 100),
('money-laundering',     'Money laundering',
 '{"money laundering","aml breach","laundering"}', 110),
('immigration-fraud',    'Immigration & migrant-exploitation fraud',
 '{"immigration fraud","visa fraud","migrant exploitation","people smuggling","immigration adviser fraud"}', 120),
('cartel-conduct',       'Cartel conduct',
 '{"cartel conduct","price fixing","bid rigging","market allocation"}', 130),
('insider-trading-markets', 'Insider trading & markets offences',
 '{"insider trading","market manipulation","markets offences","fma offences","securities fraud"}', 140),
('general-dishonesty',   'General dishonesty & other fraud',
 '{"fraud","business fraud","company fraud","dishonestly using a document","obtaining by deception","romance scam","online scam","client fraud","client fraud - overcharging","political donations","export fraud","wine labelling","food fraud","identity fraud","insurance fraud","forgery"}', 900);

-- Agencies: ~10 real agencies behind the spreadsheet's 23 spelling variants.
-- Multi-agency strings ("msd/ird") are split by the importer; aliases here
-- cover single-agency variants only.
INSERT INTO fraud_agencies (slug, label, aliases, sort) VALUES
('sfo',       'Serious Fraud Office',            '{"sfo","serious fraud office"}', 10),
('ird',       'Inland Revenue',                  '{"ird","inland revenue","inland revenue department","ir"}', 20),
('nz-police', 'NZ Police',                       '{"police","nz police","new zealand police","police fcu","fcu"}', 30),
('comcom',    'Commerce Commission',             '{"comcom","commerce commission"}', 40),
('fma',       'Financial Markets Authority',     '{"fma","financial markets authority"}', 50),
('msd',       'Ministry of Social Development',  '{"msd","ministry of social development"}', 60),
('dia',       'Department of Internal Affairs',  '{"dia","department of internal affairs"}', 70),
('inz',       'Immigration NZ (MBIE)',           '{"inz","immigration nz","immigration new zealand"}', 80),
('mbie',      'MBIE',                            '{"mbie","ministry of business innovation and employment"}', 90),
('acc',       'ACC',                             '{"acc","accident compensation corporation"}', 100),
('other',     'Other / court-initiated',         '{"court","crown","crown law"}', 900);

-- Regions: standard NZ regions + national/overseas. Aliases map the
-- city-level free text seen in the sheet onto its region.
INSERT INTO fraud_regions (slug, label, aliases, sort) VALUES
('northland',     'Northland',          '{"northland","whangarei","kaitaia"}', 10),
('auckland',      'Auckland',           '{"auckland","manukau","north shore","waitakere","papakura","pukekohe"}', 20),
('waikato',       'Waikato',            '{"waikato","hamilton","cambridge","tainui","te awamutu","tokoroa"}', 30),
('bay-of-plenty', 'Bay of Plenty',      '{"bay of plenty","tauranga","rotorua","whakatane","bop"}', 40),
('gisborne',      'Gisborne',           '{"gisborne","east coast"}', 50),
('hawkes-bay',    'Hawke''s Bay',       '{"hawkes bay","hawke''s bay","napier","hastings"}', 60),
('taranaki',      'Taranaki',           '{"taranaki","new plymouth"}', 70),
('manawatu-whanganui', 'Manawatū–Whanganui', '{"manawatu","palmerston north","whanganui","wanganui","levin"}', 80),
('wellington',    'Wellington',         '{"wellington","lower hutt","upper hutt","porirua","kapiti","hutt valley"}', 90),
('nelson-tasman', 'Nelson–Tasman',      '{"nelson","tasman","motueka"}', 100),
('marlborough',   'Marlborough',        '{"marlborough","blenheim"}', 110),
('west-coast',    'West Coast',         '{"west coast","greymouth","hokitika","westport"}', 120),
('canterbury',    'Canterbury',         '{"canterbury","christchurch","timaru","ashburton","rangiora","selwyn"}', 130),
('otago',         'Otago',              '{"otago","dunedin","queenstown","wanaka","south otago","oamaru","central otago"}', 140),
('southland',     'Southland',          '{"southland","invercargill","gore"}', 150),
('national',      'National / multi-region', '{"national","nationwide","nz wide","new zealand"}', 800),
('overseas',      'Overseas-linked',    '{"overseas","australia","australia, nz","international"}', 810);

-- Risk sectors.
INSERT INTO fraud_sectors (slug, label, aliases, sort) VALUES
('finance',            'Finance & investment',    '{"finance","financial services","banking","investment","insurance"}', 10),
('property',           'Property & construction', '{"property","real estate","construction","building"}', 20),
('health',             'Health',                  '{"health","healthcare","medical","aged care"}', 30),
('public-sector',      'Public sector',           '{"public sector","government","local government","council"}', 40),
('charity',            'Charity & NGO',           '{"charity","charitable","ngo","church","religious"}', 50),
('primary-industries', 'Primary industries',      '{"primary industries","agriculture","farming","horticulture","fishing","forestry","wine","kiwifruit"}', 60),
('retail-hospitality', 'Retail & hospitality',    '{"retail","hospitality","restaurant","food"}', 70),
('transport-logistics','Transport & logistics',   '{"transport","logistics","export","import"}', 80),
('technology',         'Technology & IT',         '{"technology","it","software","telecommunications"}', 90),
('education',          'Education',               '{"education","school","university","training"}', 100),
('other',              'Other / unclassified',    '{}', 900);

-- Profession categories (normalised from 125 free-text values).
INSERT INTO fraud_profession_categories (slug, label, aliases, sort) VALUES
('business-owner-director', 'Business owner / director',
 '{"business owner","company owner","company director","director","businessman","businesswoman","business","sole trader","entrepreneur"}', 10),
('finance-professional', 'Accountant / finance professional',
 '{"accountant","financial adviser","finance manager","bookkeeper","tax agent","financial advisor","banker"}', 20),
('legal-professional',   'Lawyer / legal professional',
 '{"lawyer","barrister","solicitor","conveyancer"}', 30),
('public-servant',       'Public servant / official',
 '{"public servant","council manager","council employee","government employee","official","civil servant"}', 40),
('health-professional',  'Health professional',
 '{"doctor","nurse","dentist","physiotherapist","pharmacist","caregiver","midwife"}', 50),
('real-estate-mortgage', 'Real estate / mortgage professional',
 '{"real estate agent","mortgage broker","property developer","property manager"}', 60),
('it-professional',      'IT professional',
 '{"it contractor","it professional","software developer","it manager"}', 70),
('religious-leader',     'Religious / community leader',
 '{"church leader","pastor","priest","minister","community leader"}', 80),
('employee-manager',     'Employee / manager',
 '{"manager","managers","employee","office manager","administrator","payroll officer","contractor"}', 90),
('beneficiary',          'Beneficiary',
 '{"beneficiary","beneficiaries"}', 100),
('individual',           'Private individual / unspecified',
 '{"individual","individuals","member of the public","unemployed","retiree"}', 900);


-- ════════════════ from alitheia migration 0117_fraud_claim_review_guard.up.sql ════════════════

-- 0117: make fraud_claim_reviews immutable at the DB layer.
--
-- Same rationale as 0114 (conviction claim_reviews): the audit trail for
-- decisions about unverified accusations against named persons is legally
-- load-bearing. Go discipline is necessary but not sufficient — a buggy
-- code path or an ad-hoc psql session must not be able to rewrite history.
-- Isolated in its own migration so the guard is independently reviewable
-- and reversible without touching the table shapes (0116).

CREATE OR REPLACE FUNCTION fraud_claim_reviews_block_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'fraud_claim_reviews is append-only (% blocked)', TG_OP;
END;
$$;

CREATE TRIGGER fraud_claim_reviews_no_update
    BEFORE UPDATE ON fraud_claim_reviews
    FOR EACH ROW EXECUTE FUNCTION fraud_claim_reviews_block_mutation();

CREATE TRIGGER fraud_claim_reviews_no_delete
    BEFORE DELETE ON fraud_claim_reviews
    FOR EACH ROW EXECUTE FUNCTION fraud_claim_reviews_block_mutation();


-- ════════════════ from alitheia migration 0119_fraud_report_snapshots.up.sql ════════════════

-- 0119: weekly fraud-tracker report snapshots.
--
-- Each ISO week is a VERSIONED snapshot: generated from review-gated data
-- at week close, stored immutably (INSERT-only API in Go; corrections are a
-- NEW version with a visible revision note — e.g. a conviction overturned
-- on appeal propagates into already-published weeks as a "Revised" issue,
-- never a silent rewrite). The report page reads snapshots only, never the
-- live tables.
--
-- include_unverified marks dev/preview builds that bypassed the verified
-- gate; the page never serves them to non-admins.

CREATE TABLE fraud_report_snapshots (
    id                 BIGSERIAL   PRIMARY KEY,
    iso_week           TEXT        NOT NULL,             -- e.g. '2026-W23'
    version            INT         NOT NULL DEFAULT 1,
    payload            JSONB       NOT NULL,
    revision_note      TEXT        NOT NULL DEFAULT '',  -- required for version > 1
    include_unverified BOOLEAN     NOT NULL DEFAULT false,
    generated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT frs_week_chk CHECK (iso_week ~ '^\d{4}-W\d{2}$'),
    CONSTRAINT frs_revision_note_chk CHECK (version = 1 OR revision_note <> ''),
    UNIQUE (iso_week, version)
);
CREATE INDEX frs_week_idx ON fraud_report_snapshots (iso_week, version DESC);


-- ════════════════ from alitheia migration 0120_fraud_court_identifiers.up.sql ════════════════

-- 0120: court identifiers on fraud cases.
--
-- NZ has no queryable court registry, but identifiers travel through public
-- documents: CRI numbers (CRI-2024-004-1234), appeal numbers (CA123/2025,
-- SC 45/2025), and neutral citations ([2025] NZHC 123). A case accumulates
-- several across its life (District Court CRI, then a CA number on appeal).
-- They are provenance + matching glue — an exact identifier match is the
-- strongest claim->case signal there is — NOT a foreign key into any
-- external system. Stored normalised (uppercase, collapsed whitespace).
--
-- Privacy note: on org-facing surfaces identifiers follow the naming rule —
-- a citation lets a reader locate the judgment and thereby the name of an
-- otherwise-unnamed case, so they render only where names may.

ALTER TABLE fraud_cases
    ADD COLUMN court_identifiers TEXT[] NOT NULL DEFAULT '{}';

CREATE INDEX fraud_cases_court_ids_idx ON fraud_cases USING GIN (court_identifiers);

-- New match method for identifier hits.
ALTER TABLE fraud_case_match_candidates DROP CONSTRAINT fcmc_method_chk;
ALTER TABLE fraud_case_match_candidates ADD CONSTRAINT fcmc_method_chk CHECK (match_method IN (
    'person_name', 'org_name', 'amount_region', 'typology_window', 'manual', 'court_id'));


-- ════════════════ from alitheia migration 0121_fraud_report_periods.up.sql ════════════════

-- 0121: generalise fraud report snapshots from ISO weeks to periods.
--
-- The weekly report was the first consumer; partner orgs also need annual
-- and quarterly issues (e.g. a calendar-year corruption scan). A period is
-- one of:  YYYY  |  YYYY-Qn  |  YYYY-Www
-- Same immutable-versioned semantics for all kinds; a year and the weeks
-- inside it are independent snapshot series.

ALTER TABLE fraud_report_snapshots RENAME COLUMN iso_week TO period;
ALTER TABLE fraud_report_snapshots DROP CONSTRAINT frs_week_chk;
ALTER TABLE fraud_report_snapshots ADD CONSTRAINT frs_period_chk
    CHECK (period ~ '^\d{4}(-W\d{2}|-Q[1-4])?$');


-- ════════════════ from alitheia migration 0122_fraud_appeal_checks.up.sql ════════════════

-- 0122: appeal-check log for the HITL worklist.
--
-- JDO and NZLII prohibit automated access, so appeal monitoring is manual
-- diligence: a reviewer runs the prefilled searches in their browser and
-- records the outcome here. The worklist excludes cases checked clear
-- within the cooling window, so the queue shrinks as checks happen and the
-- diligence trail (who checked what, when) is auditable.

CREATE TABLE fraud_appeal_checks (
    id         BIGSERIAL   PRIMARY KEY,
    case_id    BIGINT      NOT NULL REFERENCES fraud_cases(id) ON DELETE CASCADE,
    checked_by BIGINT,
    checked_by_label TEXT  NOT NULL DEFAULT '',
    outcome    TEXT        NOT NULL,
    note       TEXT        NOT NULL DEFAULT '',
    checked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fac_outcome_chk CHECK (outcome IN ('no_appeal_found', 'finding_recorded'))
);
CREATE INDEX fac_case_idx ON fraud_appeal_checks (case_id, checked_at DESC);


-- ════════════════ from alitheia migration 0123_fraud_review_action_extraction.up.sql ════════════════

-- 0123: 'extraction_applied' audit action.
--
-- fraud extract-pending retries extraction over needs_manual_extraction
-- claims and writes the result onto the claim in place (review state
-- untouched). That rewrite needs its own append-only audit action so the
-- review log distinguishes machine-recovered data from reviewer edits.

ALTER TABLE fraud_claim_reviews DROP CONSTRAINT fraud_claim_reviews_action_chk;
ALTER TABLE fraud_claim_reviews ADD CONSTRAINT fraud_claim_reviews_action_chk CHECK (action IN (
    'created', 'start_review', 'verify', 'reject', 'request_info',
    'quarantine', 'quarantine_release_vote', 'release',
    'edit_fields', 'match_confirm', 'match_reject', 'promote',
    'extraction_applied'));


-- ════════════════ from alitheia migration 0127_fraud_recode_and_lags.up.sql ════════════════

-- 0127: machine recode pass + offence→court lag view.
-- (0124–0126 are held by the in-flight screening branch — numbered past them.)
--
-- agency_basis records HOW prosecuting_agency_id was determined:
--   ''         legacy/manual coding (importer, reviewer, promote)
--   'stated'   a source explicitly names the prosecuting agency
--   'inferred' derived from the typology→agency prior (tax→IRD, benefit→MSD,
--              cartel→ComCom, ...) because no source states it
-- Inferred values are honest second-class data: internal surfaces show them
-- with a marker; published snapshot artifacts exclude them from agency
-- aggregates.
--
-- recoded_at marks that the recode pass has attempted this case, so the
-- scheduled run only spends LLM calls on new arrivals (NULL) — including
-- cases that stayed uncodable, which would otherwise loop forever.

ALTER TABLE fraud_cases
    ADD COLUMN agency_basis TEXT NOT NULL DEFAULT ''
        CONSTRAINT fraud_cases_agency_basis_chk CHECK (agency_basis IN ('', 'stated', 'inferred')),
    ADD COLUMN recoded_at TIMESTAMPTZ;

-- fraud_case_lags: derived offence→charge→conviction lags, precision-aware.
--
-- The offence end estimate compensates for coarse dates: a year-precision
-- date is stored as Jan 1, which would bias every lag upward by ~6 months,
-- so year precision shifts to mid-year and month precision to mid-month.
-- approximate=true whenever any input date is coarser than day precision —
-- consumers must label such lags as estimates.
--
-- charges_date / conviction_date are the EARLIEST dated events of their kind:
-- the first charge filing and the first finding (verdict or sentencing,
-- whichever was dated) are what the lag question is about.
CREATE VIEW fraud_case_lags AS
WITH ev AS (
    SELECT case_id,
           MIN(event_date) FILTER (WHERE event_type = 'charges_laid')                 AS charges_date,
           MIN(event_date) FILTER (WHERE event_type IN ('verdict', 'sentencing'))    AS conviction_date,
           bool_or(event_type = 'charges_laid'              AND event_date_precision <> 'day') AS charges_approx,
           bool_or(event_type IN ('verdict', 'sentencing')  AND event_date_precision <> 'day') AS conviction_approx
      FROM fraud_case_events
     WHERE event_date IS NOT NULL
     GROUP BY case_id
), base AS (
    SELECT c.id AS case_id,
           CASE c.offending_period_precision
               WHEN 'year'  THEN COALESCE(c.offending_period_end, c.offending_period_start) + 182
               WHEN 'month' THEN COALESCE(c.offending_period_end, c.offending_period_start) + 15
               ELSE COALESCE(c.offending_period_end, c.offending_period_start)
           END                                                       AS offence_end_est,
           COALESCE(c.offending_period_precision <> 'day', false)    AS offence_approx,
           ev.charges_date, ev.conviction_date,
           COALESCE(ev.charges_approx, false)                        AS charges_approx,
           COALESCE(ev.conviction_approx, false)                     AS conviction_approx
      FROM fraud_cases c
      LEFT JOIN ev ON ev.case_id = c.id
)
SELECT case_id,
       offence_end_est,
       charges_date,
       conviction_date,
       CASE WHEN charges_date    >= offence_end_est THEN charges_date    - offence_end_est END AS offence_to_charge_days,
       CASE WHEN conviction_date >= offence_end_est THEN conviction_date - offence_end_est END AS offence_to_conviction_days,
       CASE WHEN conviction_date >= charges_date    THEN conviction_date - charges_date    END AS charge_to_conviction_days,
       (offence_approx OR charges_approx OR conviction_approx)                                 AS approximate
  FROM base;


-- ════════════════ from alitheia migration 0128_fraud_naming_default.up.sql ════════════════

-- 0128: naming on by default.
--
-- Original posture: names render only after a reviewer ticks
-- naming_approved per case (editorial allow-gate). Operator decision
-- 2026-06-12: every tracker surface is org-flag-gated under a signed access
-- agreement — nothing here is public — so the editorial gate inverts from
-- allow-list to block-list: names show by default and a reviewer unticks the
-- flag to block a specific case.
--
-- The LEGAL gates are untouched and still case-level absolute:
--   - pre-verdict cases render unnamed everywhere (template + builder);
--   - any participant suppression_flag or clean_slate_possible blocks
--     naming for the whole case (fraudNamingAllowed);
--   - suppression-signalled claims still auto-quarantine upstream, before
--     a case exists.

ALTER TABLE fraud_cases ALTER COLUMN naming_approved SET DEFAULT true;

UPDATE fraud_cases SET naming_approved = true WHERE NOT naming_approved;


-- ════════════════ from alitheia migration 0136_identity_fraud_typology.up.sql ════════════════

-- 0136: promote identity fraud to a first-class typology.
--
-- "identity fraud" was only an alias of the general-dishonesty catch-all, so it
-- could not be filtered for in the explorer and never showed as its own bucket.
-- This makes it a real typology and removes the alias from the catch-all, so
-- the deterministic resolver maps "identity fraud"/"identity theft" raw text to
-- it on all future ingestion, and the recode pass's closed-set classifier can
-- now choose it.
--
-- Existing cases are NOT re-pointed here. Identity signals live in summaries and
-- charge text, not the typology label, and a naive keyword match mis-fires —
-- e.g. "concealing identity of true donor" is electoral fraud, and cases where
-- a fake identity is merely the method (mortgage/COVID fraud) are correctly
-- coded to their primary typology already. Re-coding the genuine matches is the
-- job of `ingester fraud recode --retry` (LLM, evidence-gated, fills only
-- uncoded cases); any case already mis-filed under general-dishonesty is a
-- human re-point on the admin case page.
INSERT INTO fraud_typologies (slug, label, aliases, sort, active) VALUES
('identity-fraud', 'Identity fraud',
 '{"identity fraud","identity theft","identity crime","identity document fraud","stolen identity","false identity","assumed identity","impersonation","passport fraud","using another person''s identity","using another''s identity"}',
 145, true)
ON CONFLICT (slug) DO NOTHING;

UPDATE fraud_typologies
   SET aliases = array_remove(aliases, 'identity fraud')
 WHERE slug = 'general-dishonesty';


-- ════════════════ from alitheia migration 0144_fraud_case_status_override.up.sql ════════════════

-- Manual status override for fraud cases.
--
-- A case's status is normally DERIVED from its lifecycle events
-- (fraudtracker.DeriveCaseStatus) and recomputed inside every write tx
-- that touches events. Operators curating cases sometimes need to set the
-- status by hand where the event spine is incomplete or ambiguous. This
-- flag marks such a case: when true, recomputeFraudCaseStatusTx leaves the
-- stored status alone so a later event edit / recode can't silently clobber
-- the human value. Clearing the flag ("revert to auto-derived") re-enables
-- derivation. Default false preserves existing behaviour for every row.
ALTER TABLE fraud_cases
    ADD COLUMN status_overridden BOOLEAN NOT NULL DEFAULT FALSE;
