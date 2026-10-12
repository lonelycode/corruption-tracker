// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/ingest/newsfeed"
	"github.com/lonelycode/corruption-tracker/llm"
	sitelayout "github.com/lonelycode/corruption-tracker/standalone/layout"
)

// DataDirEnv names the data volume (the generated secret key lives there).
const DataDirEnv = "TRACKER_DATA_DIR"

// DefaultDataDir is the compose stack's data volume.
const DefaultDataDir = "/data"

// Options configure a Service.
type Options struct {
	// LookupEnv resolves overrides (default os.LookupEnv).
	LookupEnv func(string) (string, bool)
	// DataDir holds secret.key (default $TRACKER_DATA_DIR, else /data).
	DataDir string
	Log     *slog.Logger
	// NewLLM builds the completer for the current LLM settings (default
	// NewCompleter); tests substitute a fake.
	NewLLM func(LLMConfig) llm.Completer
}

// Source says where a setting's effective value comes from.
type Source string

const (
	SourceDefault Source = "default"
	SourceStored  Source = "stored"
	SourceEnv     Source = "env"
)

type entry struct {
	value         string
	source        Source
	undecryptable bool
	updatedAt     time.Time
	updatedBy     string
}

// Service is the settings store with an in-memory snapshot. Reads are
// lock-free of the database (host.Settings has no context); Update and
// Reload refresh the snapshot, so a save applies without a restart.
type Service struct {
	pool      *pgxpool.Pool
	dataDir   string
	lookupEnv func(string) (string, bool)
	log       *slog.Logger
	newLLM    func(LLMConfig) llm.Completer

	mu   sync.RWMutex
	snap map[string]entry

	keyMu  sync.Mutex
	sealer *sealer // nil until the first secret is read or written
}

var (
	_ host.Settings             = (*Service)(nil)
	_ host.CrawlerSettings      = (*Service)(nil)
	_ sitelayout.BrandingSource = (*Service)(nil)
)

// Open loads the stored settings. The instance key is loaded (or, the
// first time a secret is saved, created) only when a secret needs it, so
// an instance with no stored secrets never touches the data directory.
func Open(ctx context.Context, pool *pgxpool.Pool, o Options) (*Service, error) {
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.NewLLM == nil {
		o.NewLLM = NewCompleter
	}
	o.DataDir = ResolveDataDir(o.LookupEnv, o.DataDir)
	if v, ok := o.LookupEnv(SecretEnv); ok && strings.TrimSpace(v) == "" {
		return nil, fmt.Errorf("%s is set but blank", SecretEnv)
	}
	s := &Service{pool: pool, dataDir: o.DataDir, lookupEnv: o.LookupEnv, log: o.Log, newLLM: o.NewLLM}
	if err := s.Reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// ResolveDataDir is the data volume: dir when set, else $TRACKER_DATA_DIR,
// else /data. Backups live under it too.
func ResolveDataDir(lookupEnv func(string) (string, bool), dir string) string {
	if dir != "" {
		return dir
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if v, ok := lookupEnv(DataDirEnv); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return DefaultDataDir
}

// DataDir is the data volume this instance uses.
func (s *Service) DataDir() string { return s.dataDir }

// box returns the sealer, loading the instance key on first use. A failure
// is not cached, so fixing the data volume needs no restart.
func (s *Service) box() (*sealer, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if s.sealer != nil {
		return s.sealer, nil
	}
	key, err := loadKey(s.lookupEnv, s.dataDir)
	if err != nil {
		return nil, err
	}
	sl, err := newSealer(key)
	if err != nil {
		return nil, err
	}
	s.sealer = sl
	return sl, nil
}

// Reload rebuilds the snapshot from the environment and tracker_settings.
func (s *Service) Reload(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT st.key, st.value, st.secret, st.ciphertext, st.updated_at, COALESCE(a.display_label, '')
		  FROM tracker_settings st
		  LEFT JOIN tracker_actors a ON a.id = st.updated_by`)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	stored := map[string]entry{}
	for rows.Next() {
		var k, v, by string
		var secret bool
		var ct []byte
		var at time.Time
		if err := rows.Scan(&k, &v, &secret, &ct, &at, &by); err != nil {
			rows.Close()
			return err
		}
		e := entry{value: v, source: SourceStored, updatedAt: at, updatedBy: by}
		if secret {
			box, err := s.box()
			if err != nil {
				rows.Close()
				return err
			}
			pt, err := box.open(k, ct)
			if err != nil {
				e = entry{source: SourceStored, undecryptable: true, updatedAt: at, updatedBy: by}
				s.log.Warn("stored secret setting cannot be decrypted; re-enter it", "setting", k)
			} else {
				e.value = pt
			}
		}
		stored[k] = e
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	snap := map[string]entry{}
	for _, k := range Keys {
		if v, ok := s.lookupEnv(k.Env); ok && k.Env != "" {
			snap[k.Name] = entry{value: strings.TrimSpace(v), source: SourceEnv}
			continue
		}
		if e, ok := stored[k.Name]; ok {
			snap[k.Name] = e
			continue
		}
		snap[k.Name] = entry{value: k.Default, source: SourceDefault}
	}
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
	return nil
}

// Value is a setting's effective value ("" when unset or undecryptable).
func (s *Service) Value(name string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap[name].value
}

// Field is one setting as the Settings page shows it. Secret values are
// never included, only whether one is set and its last characters.
type Field struct {
	Key
	Value         string // non-secret settings only
	IsSet         bool
	Hint          string // secrets: "…last4"
	Locked        bool   // overridden by Env
	Source        Source
	Undecryptable bool
	UpdatedAt     time.Time
	UpdatedBy     string
}

// Fields returns every setting in page order.
func (s *Service) Fields() []Field {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Field, 0, len(Keys))
	for _, k := range Keys {
		e := s.snap[k.Name]
		f := Field{Key: k, Source: e.source, Locked: e.source == SourceEnv,
			Undecryptable: e.undecryptable, UpdatedAt: e.updatedAt, UpdatedBy: e.updatedBy}
		f.IsSet = e.value != "" || e.undecryptable
		if k.Secret {
			f.Hint = hint(e.value)
		} else {
			f.Value = e.value
		}
		out = append(out, f)
	}
	return out
}

// hint shows enough of a secret to recognise it, never enough to use it.
func hint(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 8 {
		return "set"
	}
	return "…" + v[len(v)-4:]
}

// ErrLocked rejects a change to a setting an environment variable controls.
var ErrLocked = errors.New("this setting is set by an environment variable")

// Change is one edit from the Settings page. For a secret, an empty Value
// keeps the stored one unless Clear is set.
type Change struct {
	Name  string
	Value string
	Clear bool
}

// Update validates and stores changes in one transaction, then reloads.
// actorID is the admin's tracker_actors id (nil when unknown).
func (s *Service) Update(ctx context.Context, actorID *int64, changes []Change) error {
	for _, c := range changes {
		k, ok := KeyByName(c.Name)
		if !ok {
			return fmt.Errorf("unknown setting %q", c.Name)
		}
		v := normalize(k, c.Value)
		if s.locked(k) {
			// The page never submits locked fields; anything that would
			// change one is refused rather than silently ignored.
			if c.Clear || (k.Secret && v != "") || (!k.Secret && v != s.Value(k.Name)) {
				return fmt.Errorf("%s: %w (%s)", k.Label, ErrLocked, k.Env)
			}
			continue
		}
		if err := validate(k, v); err != nil {
			return fmt.Errorf("%s: %w", k.Label, err)
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, c := range changes {
		k, _ := KeyByName(c.Name)
		if s.locked(k) {
			continue
		}
		v := normalize(k, c.Value)
		switch {
		case c.Clear || (!k.Secret && (v == "" || v == k.Default)):
			if _, err := tx.Exec(ctx, `DELETE FROM tracker_settings WHERE key = $1`, k.Name); err != nil {
				return err
			}
		case k.Secret && v == "":
			// keep the stored secret
		case k.Secret:
			box, err := s.box()
			if err != nil {
				return err
			}
			ct, err := box.seal(k.Name, v)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO tracker_settings (key, value, secret, ciphertext, updated_by, updated_at)
				VALUES ($1, '', true, $2, $3, now())
				ON CONFLICT (key) DO UPDATE SET value = '', secret = true, ciphertext = EXCLUDED.ciphertext,
				       updated_by = EXCLUDED.updated_by, updated_at = now()`, k.Name, ct, actorID); err != nil {
				return err
			}
		default:
			if _, err := tx.Exec(ctx, `
				INSERT INTO tracker_settings (key, value, secret, ciphertext, updated_by, updated_at)
				VALUES ($1, $2, false, NULL, $3, now())
				ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, secret = false, ciphertext = NULL,
				       updated_by = EXCLUDED.updated_by, updated_at = now()`, k.Name, v, actorID); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return s.Reload(ctx)
}

func (s *Service) locked(k Key) bool {
	if k.Env == "" {
		return false
	}
	_, ok := s.lookupEnv(k.Env)
	return ok
}

var accentRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// normalize trims a submitted value; a multi-line one is trimmed line by
// line with Windows line endings removed, so an unchanged text area still
// equals the default.
func normalize(k Key, v string) string {
	if !k.Multiline {
		return strings.TrimSpace(v)
	}
	lines := strings.Split(strings.ReplaceAll(v, "\r\n", "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func validate(k Key, v string) error {
	if v == "" {
		return nil
	}
	if len(k.Options) > 0 {
		for _, o := range k.Options {
			if v == o {
				return nil
			}
		}
		return fmt.Errorf("must be one of %s", strings.Join(k.Options, ", "))
	}
	switch k.Name {
	case SiteTimezone:
		if _, err := time.LoadLocation(v); err != nil {
			return fmt.Errorf("unknown time zone %q", v)
		}
	case SiteAccent:
		if !accentRE.MatchString(v) {
			return errors.New("use #rrggbb")
		}
	case SiteSourceURL, CrawlerContact, LLMBaseURL:
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errors.New("must be an http(s) URL")
		}
	case NewsFeeds:
		_, err := newsfeed.ParseFeeds(v)
		return err
	case SiteLogoURL:
		if strings.HasPrefix(v, "/") {
			return nil
		}
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("must be an https URL or a path starting with /")
		}
	}
	if !k.Multiline && strings.ContainsAny(v, "\r\n") {
		return errors.New("must be a single line")
	}
	return nil
}

// ---- host.Settings, host.CrawlerSettings, sitelayout.BrandingSource ----

// IMAP returns the court-alert mailbox; false when no server is set.
func (s *Service) IMAP() (host.IMAPConfig, bool) {
	addr := s.Value(IMAPAddr)
	if addr == "" {
		return host.IMAPConfig{}, false
	}
	return host.IMAPConfig{
		Addr:     addr,
		Username: s.Value(IMAPUser),
		Password: s.Value(IMAPPassword),
		Folder:   s.Value(IMAPFolder),
	}, true
}

// LLMConfigured reports whether jobs needing an LLM can run.
func (s *Service) LLMConfigured() bool {
	_, ok := s.LLMConfig()
	return ok
}

// Timezone is the schedule's zone (nil: the scheduler default).
func (s *Service) Timezone() *time.Location {
	loc, err := time.LoadLocation(s.Value(SiteTimezone))
	if err != nil || s.Value(SiteTimezone) == "" {
		return nil
	}
	return loc
}

// Crawler is the operator's crawler identity (zero: the neutral default).
func (s *Service) Crawler() host.Crawler {
	return host.Crawler{
		Name:       s.Value(CrawlerName),
		ContactURL: s.Value(CrawlerContact),
		Purpose:    s.Value(CrawlerPurpose),
	}
}

// NamingOn reports whether an admin has turned naming on for this instance
// (readmodel.Options.NamingEnabled). Off is the standalone default.
func (s *Service) NamingOn(context.Context) bool { return s.Value(NamingEnabled) == "on" }

// NewsFeeds is the feed list for the standalone news corpus. An invalid
// list can only come from the environment (the page validates), and is an
// error rather than a silently shorter list.
func (s *Service) NewsFeeds() ([]newsfeed.Feed, error) {
	feeds, err := newsfeed.ParseFeeds(s.Value(NewsFeeds))
	if err != nil {
		return nil, fmt.Errorf("news feeds setting: %w", err)
	}
	return feeds, nil
}

// Branding is the current site name, logo and accent.
func (s *Service) Branding(context.Context) sitelayout.Branding {
	name := s.Value(SiteName)
	if name == "" {
		name = sitelayout.DefaultName
	}
	return sitelayout.Branding{Name: name, LogoURL: s.Value(SiteLogoURL), Accent: s.Value(SiteAccent)}
}

// SiteName is the operator's name for the instance ("" when unset).
func (s *Service) SiteName() string { return s.Value(SiteName) }

// SourceURL is the stored AGPL source URL setting (version.ResolveSourceURL
// applies the env override and the build default).
func (s *Service) SourceURL() string { return s.Value(SiteSourceURL) }

// LLMConfig is the current LLM configuration; false when incomplete.
func (s *Service) LLMConfig() (LLMConfig, bool) {
	c := LLMConfig{
		Provider: s.Value(LLMProvider),
		APIKey:   s.Value(LLMAPIKey),
		Model:    s.Value(LLMModel),
		BaseURL:  s.Value(LLMBaseURL),
	}
	switch c.Provider {
	case ProviderAnthropic, "":
		c.Provider = ProviderAnthropic
		return c, c.APIKey != ""
	case ProviderOpenAICompatible:
		return c, c.BaseURL != ""
	}
	return c, false
}

// LLM builds a completer from the current settings; nil when not
// configured (jobs that need one are then skipped as not configured).
func (s *Service) LLM() llm.Completer {
	c, ok := s.LLMConfig()
	if !ok {
		return nil
	}
	return s.newLLM(c)
}

// OpenSecret decrypts a stored secret's ciphertext (bundle.SecretOpener:
// the exporter re-seals secrets under the bundle passphrase).
func (s *Service) OpenSecret(name string, ciphertext []byte) (string, error) {
	box, err := s.box()
	if err != nil {
		return "", err
	}
	return box.open(name, ciphertext)
}
