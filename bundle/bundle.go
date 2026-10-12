// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package bundle is the tracker's export bundle, format v1: a
// point-in-time, byte-stable, verifiable copy of the whole dataset that a
// fresh instance can import losslessly. See
// design §10.
//
// A bundle is a gzip-compressed tar holding, in this order: manifest.json,
// settings.json, tables/<table>.ndjson in load order, and optionally
// secrets.enc. Table lines are produced by Postgres (row_to_json) inside one
// REPEATABLE READ, READ ONLY transaction, so two exports of the same data
// have identical table files.
package bundle

// Format identifies a tracker bundle.
const Format = "corruption-tracker-bundle"

// FormatVersion is the container/manifest structure version this package
// writes and reads. It bumps only when that structure changes; table shape
// changes ride on Manifest.SchemaVersion (§10.8).
const FormatVersion = 1

// Source kinds.
const (
	KindAlitheiaEmbedded = "alitheia-embedded"
	KindStandalone       = "standalone"
)

// Entry names inside the archive.
const (
	ManifestFile = "manifest.json"
	SettingsFile = "settings.json"
	SecretsFile  = "secrets.enc"
	tablesDir    = "tables/"
)

// Manifest is manifest.json (§10.3). Field order is the JSON key order.
type Manifest struct {
	Format        string         `json:"format"`
	FormatVersion int            `json:"format_version"`
	SchemaVersion uint           `json:"schema_version"`
	AppVersion    string         `json:"app_version"`
	ExportID      string         `json:"export_id"`
	CreatedAt     string         `json:"created_at"` // RFC 3339, UTC, seconds
	Source        Source         `json:"source"`
	Snapshot      Snapshot       `json:"snapshot"`
	Handoff       *Handoff       `json:"handoff"`
	Options       Options        `json:"options"`
	Tables        []TableEntry   `json:"tables"`
	OmittedTables []OmittedTable `json:"omitted_tables"`
	Settings      FileRef        `json:"settings"`
	Secrets       *FileRef       `json:"secrets"`
}

// Source identifies the exporting instance.
type Source struct {
	InstanceID string `json:"instance_id"`
	Kind       string `json:"kind"` // KindAlitheiaEmbedded | KindStandalone
	BaseURL    string `json:"base_url"`
}

// Snapshot records the transaction the bundle was read in.
type Snapshot struct {
	Isolation       string `json:"isolation"`
	PGSnapshot      string `json:"pg_snapshot"`
	WALLSN          string `json:"wal_lsn,omitempty"` // best-effort, diagnostic only
	PGServerVersion string `json:"pg_server_version"`
}

// Handoff is set only when the exported instance was frozen (§11).
type Handoff struct {
	FreezeID string `json:"freeze_id"`
	FrozenAt string `json:"frozen_at"`
	FrozenBy string `json:"frozen_by"`
}

// Options records what the export included.
type Options struct {
	IncludeSecrets   bool `json:"include_secrets"`
	IncludeAccessLog bool `json:"include_access_log"`
}

// TableEntry describes one tables/<name>.ndjson file.
type TableEntry struct {
	Name    string   `json:"name"`
	File    string   `json:"file"`
	Rows    int64    `json:"rows"`
	SHA256  string   `json:"sha256"`
	PK      []string `json:"pk"`
	MaxID   *int64   `json:"max_id"` // max of the serial column; null when none (or empty)
	Columns []Column `json:"columns"`
}

// Column is one exported column: its name and format_type().
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// OmittedTable is a table of the v1 set that the bundle deliberately does
// not carry, with why. An importer treats any other missing table as an
// error.
type OmittedTable struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// FileRef points at a non-table file and its checksum.
type FileRef struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

// TableSet is the v1 dataset in FK-safe load order (§10.4). Tables a given
// schema version doesn't have yet are listed in Manifest.OmittedTables.
var TableSet = []string{
	"tracker_actors",
	"fraud_typologies", "fraud_agencies", "fraud_regions", "fraud_profession_categories", "fraud_sectors",
	"fraud_persons", "fraud_organisations",
	"fraud_cases",
	"fraud_case_typologies", "fraud_case_agencies", "fraud_case_participants", "fraud_case_organisations",
	"fraud_sources",
	"fraud_case_events",
	"fraud_claims", "fraud_claim_reviews", "fraud_case_match_candidates",
	"fraud_appeal_checks", "fraud_report_snapshots", "fraud_case_revisions",
	"tracker_connector_state", "tracker_news_items", "tracker_job_runs",
}

// AccessLogTable is exported only with Options.IncludeAccessLog.
const AccessLogTable = "tracker_access_log"

// NeverExported are tracker tables that must not leave an instance in a
// bundle (§10.4): credentials, secrets, instance metadata (carried in the
// manifest) and the migration version (carried as SchemaVersion).
var NeverExported = []string{
	"tracker_users", "tracker_sessions", "tracker_settings", "tracker_instance", "tracker_schema_migrations",
}
