// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package settings is the standalone tracker's configuration: operator
// settings live in tracker_settings and are edited from the admin Settings
// page, so a non-technical operator needs no config files. Secrets (API
// keys, mailbox passwords) are encrypted at rest. An environment variable,
// when set, overrides its setting and locks the field in the UI — the
// escape hatch for advanced deployments. See
// design §9.1.
//
// alitheia does not use this package: embedded, the tracker reads alitheia's
// own environment configuration through its host.Settings adapter.
package settings

import "github.com/lonelycode/corruption-tracker/ingest/newsfeed"

// Group orders the fields on the Settings page.
type Group string

const (
	GroupSite    Group = "Site"
	GroupLLM     Group = "LLM"
	GroupMailbox Group = "Court-alert mailbox (IMAP)"
	GroupCrawler Group = "Crawler identity"
	GroupNews    Group = "News feeds"
	GroupNaming  Group = "Naming"
)

// Key describes one setting.
type Key struct {
	Name    string // tracker_settings.key
	Group   Group
	Label   string
	Help    string
	Env     string   // environment variable that overrides and locks it
	Secret  bool     // encrypted at rest, never rendered back
	Default string   // used when neither env nor DB sets it
	Options []string // when non-empty, the only accepted values
	// Multiline values are edited in a text area, one entry per line.
	Multiline bool
	// Local settings belong to this instance alone: a bundle import never
	// carries them over (the receiving admin decides).
	Local bool
}

// Setting names.
const (
	SiteName       = "site.name"
	SiteLogoURL    = "site.logo_url"
	SiteAccent     = "site.accent"
	SiteSourceURL  = "site.source_url"
	SiteTimezone   = "site.timezone"
	LLMProvider    = "llm.provider"
	LLMAPIKey      = "llm.api_key"
	LLMModel       = "llm.model"
	LLMBaseURL     = "llm.base_url"
	IMAPAddr       = "imap.addr"
	IMAPUser       = "imap.user"
	IMAPPassword   = "imap.password"
	IMAPFolder     = "imap.folder"
	CrawlerName    = "crawler.name"
	CrawlerContact = "crawler.contact_url"
	CrawlerPurpose = "crawler.purpose"
	NewsFeeds      = "news.feeds"
	NamingEnabled  = "naming.enabled"
)

// LLM providers.
const (
	ProviderAnthropic        = "anthropic"
	ProviderOpenAICompatible = "openai-compatible"
)

// Keys is every setting, in page order.
var Keys = []Key{
	{Name: SiteName, Group: GroupSite, Label: "Site name", Env: "TRACKER_OPERATOR",
		Help: "Shown in the header, page titles and the suppression notice on reports, as soon as you save."},
	{Name: SiteLogoURL, Group: GroupSite, Label: "Logo URL", Env: "TRACKER_LOGO_URL",
		Help: "Optional. An https URL or a path on this site."},
	{Name: SiteAccent, Group: GroupSite, Label: "Accent colour", Env: "TRACKER_ACCENT",
		Help: "Optional, as #rrggbb."},
	{Name: SiteSourceURL, Group: GroupSite, Label: "Source code URL", Env: "TRACKER_SOURCE_URL",
		Help: "Where visitors get this server's source (AGPL). Change it if you run a modified version. Takes effect after a restart."},
	{Name: SiteTimezone, Group: GroupSite, Label: "Time zone", Env: "TRACKER_TIMEZONE", Default: "Pacific/Auckland",
		Help: "IANA zone the job schedule runs in, e.g. Pacific/Auckland. Takes effect after a restart."},

	{Name: LLMProvider, Group: GroupLLM, Label: "Provider", Env: "TRACKER_LLM_PROVIDER", Default: ProviderAnthropic,
		Options: []string{ProviderAnthropic, ProviderOpenAICompatible},
		Help:    "Anthropic, or any OpenAI-compatible endpoint (set the base URL)."},
	{Name: LLMAPIKey, Group: GroupLLM, Label: "API key", Env: "TRACKER_LLM_API_KEY", Secret: true,
		Help: "Needed by the jobs that read sources: poll, classify-news, mail-ingest, extract-pending, recode."},
	{Name: LLMModel, Group: GroupLLM, Label: "Model", Env: "TRACKER_LLM_MODEL",
		Help: "Leave blank for the provider default."},
	{Name: LLMBaseURL, Group: GroupLLM, Label: "Base URL", Env: "TRACKER_LLM_BASE_URL",
		Help: "OpenAI-compatible provider only, e.g. https://llm.example.org/v1."},

	{Name: IMAPAddr, Group: GroupMailbox, Label: "Server", Env: "TRACKER_IMAP_ADDR",
		Help: "host or host:port (993 when no port is given). Leave blank to switch mail ingestion off."},
	{Name: IMAPUser, Group: GroupMailbox, Label: "Username", Env: "TRACKER_IMAP_USER"},
	{Name: IMAPPassword, Group: GroupMailbox, Label: "Password", Env: "TRACKER_IMAP_PASSWORD", Secret: true,
		Help: "An app password for a mailbox used only for court alerts."},
	{Name: IMAPFolder, Group: GroupMailbox, Label: "Folder", Env: "TRACKER_IMAP_FOLDER", Default: "INBOX"},

	{Name: CrawlerName, Group: GroupCrawler, Label: "Crawler name", Env: "TRACKER_CRAWLER_NAME",
		Help: "Sent as the User-Agent when fetching agency releases, e.g. ExampleTrackerBot. Blank uses a neutral default."},
	{Name: CrawlerContact, Group: GroupCrawler, Label: "Contact URL", Env: "TRACKER_CRAWLER_CONTACT",
		Help: "Where site owners can reach you about the crawler."},
	{Name: CrawlerPurpose, Group: GroupCrawler, Label: "Purpose", Env: "TRACKER_CRAWLER_PURPOSE",
		Help: "Optional short description in the User-Agent, e.g. public-interest research."},

	{Name: NewsFeeds, Group: GroupNews, Label: "Feeds", Env: "TRACKER_NEWS_FEEDS", Multiline: true,
		Default: newsfeed.DefaultFeeds,
		Help: "RSS or Atom feeds that classify-news reads, one per line: a short name, the URL, and optionally " +
			"\"commentary\". Each feed's robots.txt is checked before every poll. Empty the box to restore the " +
			"defaults; to read no feeds, leave a line starting with #."},

	// No env var: turning naming on must be a recorded admin action,
	// so the page records who did it and when.
	{Name: NamingEnabled, Group: GroupNaming, Label: "Show participants' names", Default: "off", Local: true,
		Options: []string{"off", "on"},
		Help: "Off: no reader page, export or report shows anyone's name, whatever each case says. " +
			"On: a verified case shows names when it passes the naming rule (no reviewer block, no suppression or " +
			"Clean Slate flag, past verdict or named by the prosecuting agency). Turning it on needs the confirmation below."},
}

// KeyByName returns the setting with that name.
func KeyByName(name string) (Key, bool) {
	for _, k := range Keys {
		if k.Name == name {
			return k, true
		}
	}
	return Key{}, false
}
