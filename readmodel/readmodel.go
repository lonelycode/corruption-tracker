// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package readmodel is the only way reader-facing surfaces (report, case
// page, explorer, digest export) get tracker data. Publishability is decided
// here, per request tier, before anything leaves core: verified-only, the
// naming rule, suppression / Clean Slate withholding, dev snapshots for
// operators only, and the public tier's name stripping. Presentation code
// receives already-redacted DTOs — a withheld name is absent, not a field
// plus a flag a template must honour. See design
// §5.9.
//
// Every call that returns case- or person-level data writes its Privacy-Act
// audit row through host.AccessAuditor before returning, with the surface
// and detail strings the tracker has always written, so no wrapper can
// forget to log.
package readmodel

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/lonelycode/corruption-tracker/host"
	"github.com/lonelycode/corruption-tracker/store"
)

// Tier is what a requester may read. Callers never choose it: For resolves
// it from the host Authorizer.
type Tier int

const (
	// TierNone is a signed-in requester with no tracker entitlement.
	TierNone Tier = iota
	// TierPublic is an anonymous requester — clamped here whatever the
	// Authorizer says. Published snapshots only, names/summaries stripped.
	TierPublic
	// TierViewer reads published snapshots and verified case pages.
	TierViewer
	// TierExport adds the explorer and the digest export.
	TierExport
	// TierOperator adds dev (unverified) snapshots and unverified cases.
	TierOperator
)

// ErrNotFound: the thing does not exist at this tier (never confirms that
// it exists at a higher one). ErrForbidden: the surface is above the tier.
var (
	ErrNotFound  = errors.New("readmodel: not found")
	ErrForbidden = errors.New("readmodel: forbidden at this tier")
)

// Source hands out a per-request View.
type Source interface {
	For(r *http.Request) (View, error)
}

// Surface is the public-report hook (design §5.9.3): a host registers zero
// or more, each mounted at its own prefix with the core Source. It lives
// here rather than in package host because Mount needs Source, and Source
// needs host.Authorizer — host importing readmodel would be a cycle.
type Surface interface {
	Mount(r chi.Router, src Source)
}

// Options wires a Source.
type Options struct {
	Store *store.Store
	Auth  host.Authorizer
	Audit host.AccessAuditor // may be nil (no audit sink)
	Log   *slog.Logger
	// Notice is the suppression / derivative-work disclaimer every reader
	// surface and export carries, already naming the operator.
	Notice string
	// NoticeFunc, when set, builds the notice per request instead (so an
	// operator renamed at runtime is named correctly without a restart).
	NoticeFunc func(r *http.Request) string
	// NamingEnabled, when set and false, withholds every name on every
	// reader surface, whatever the case says (standalone's instance switch,
	// off until an admin turns it on). nil means naming is
	// on, as embedded in alitheia.
	NamingEnabled func(ctx context.Context) bool
}

// New returns the core Source.
func New(o Options) Source {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &source{o: o}
}

type source struct{ o Options }

func (s *source) For(r *http.Request) (View, error) {
	v := &view{o: s.o, rawQuery: r.URL.RawQuery, notice: s.o.Notice}
	if s.o.NoticeFunc != nil {
		v.notice = s.o.NoticeFunc(r)
	}
	p, ok := s.o.Auth.Principal(r)
	if !ok {
		v.tier = TierPublic
		return v, nil
	}
	v.principal, v.signedIn = p, true
	acc, err := s.o.Auth.Access(r.Context(), r)
	if err != nil {
		// A failed entitlement check denies, as the flag checks always have.
		s.o.Log.Error("corruption tracker access check", "err", err)
		v.tier = TierNone
		return v, nil
	}
	v.tier = TierFor(acc)
	return v, nil
}

// TierFor maps a signed-in requester's entitlements to a tier. Operators
// imply export and view; export implies view.
func TierFor(a host.Access) Tier {
	switch {
	case a.Operate || a.Admin:
		return TierOperator
	case a.Export:
		return TierExport
	case a.View:
		return TierViewer
	default:
		return TierNone
	}
}

// View is one requester's window onto the tracker.
type View interface {
	Tier() Tier
	// SignedIn reports whether the requester is authenticated.
	SignedIn() bool
	Periods(ctx context.Context) ([]PeriodRef, error)
	// Snapshot returns the latest visible issue for period (ErrNotFound when
	// there is none) and audits the view whether or not one exists.
	Snapshot(ctx context.Context, period string) (Snapshot, error)
	Case(ctx context.Context, id int64) (Case, error)
	Explorer(ctx context.Context, f ExplorerFilter) (ExplorerPage, error)
	// ExplorerAll is the unpaginated explorer slice for CSV export.
	ExplorerAll(ctx context.Context, f ExplorerFilter) ([]ExplorerRow, error)
	Facets(ctx context.Context) (Facets, error)
	// Digest is the period's verified cases prepared for export; format is
	// recorded in the audit row only.
	Digest(ctx context.Context, period, format string) ([]DigestRow, error)
	Notice() string
}

type view struct {
	o         Options
	tier      Tier
	principal host.Principal
	signedIn  bool
	rawQuery  string // the request's query, for explorer audit detail
	notice    string
}

func (v *view) Tier() Tier          { return v.tier }
func (v *view) SignedIn() bool      { return v.signedIn }
func (v *view) Notice() string      { return v.notice }
func (v *view) operator() bool      { return v.tier >= TierOperator }
func (v *view) atLeast(t Tier) bool { return v.tier >= t }

// namingOn is the instance switch; the per-case naming rule applies on top.
func (v *view) namingOn(ctx context.Context) bool {
	return v.o.NamingEnabled == nil || v.o.NamingEnabled(ctx)
}

// audit writes one Privacy-Act row; failures are logged, never returned.
func (v *view) audit(ctx context.Context, entitlement, surface, detail string) {
	if v.o.Audit == nil || !v.signedIn {
		return
	}
	if err := v.o.Audit.LogAccess(ctx, v.principal, entitlement, surface, detail); err != nil {
		v.o.Log.Warn("corruption tracker access log", "surface", surface, "err", err)
	}
}
