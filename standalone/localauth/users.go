// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package localauth is the standalone server's identity: local accounts
// with argon2id passwords, database sessions, CSRF, login rate limiting,
// the first-run setup wizard, admin user management and the
// `user reset-password` CLI. It implements host.Authorizer, so the tracker's
// web and reader surfaces gate exactly as they do inside alitheia.
//
// Roles (design §2 D3): one role per user,
// strictly nested admin ⊃ reviewer ⊃ viewer, plus a per-user export grant
// that only matters for viewers (reviewers and admins imply it). Every
// grant needs an access-basis note, recorded in tracker_access_log. Users
// are disabled, never deleted; the last active admin can be neither
// demoted nor disabled. Each user is one tracker_actors row; actors imported
// from another instance have no login until an admin claims them.
package localauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lonelycode/corruption-tracker/host"
)

// Role is a user's single role.
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleReviewer Role = "reviewer"
	RoleViewer   Role = "viewer"
)

// Valid reports whether r is one of the three roles.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleReviewer || r == RoleViewer }

// CanReview reports whether the role may operate the back office (and so
// cast quarantine-release votes).
func (r Role) CanReview() bool { return r == RoleAdmin || r == RoleReviewer }

var (
	ErrInvalidLogin  = errors.New("unknown user or wrong password")
	ErrSetupDone     = errors.New("setup has already been completed")
	ErrLastAdmin     = errors.New("this is the last active admin: promote another admin first")
	ErrNoteRequired  = errors.New("a grant requires an access-basis note (e.g. the access agreement reference)")
	ErrUsernameTaken = errors.New("that username is already taken")
	ErrNotFound      = errors.New("user not found")
	ErrNotClaimable  = errors.New("that actor cannot be claimed: it already has a login or is not a person")
)

// User is one local account joined with its actor.
type User struct {
	ID          int64
	ActorID     int64
	ExternalRef string // the actor's external_ref ("local:<id>", or a claimed legacy ref)
	Username    string
	Role        Role
	CanExport   bool
	Disabled    bool
	AccessBasis string
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

// Access maps the user to the tracker's entitlements.
func (u User) Access() host.Access {
	if u.Disabled {
		return host.Access{}
	}
	switch u.Role {
	case RoleAdmin:
		return host.Access{View: true, Export: true, Operate: true, Admin: true}
	case RoleReviewer:
		return host.Access{View: true, Export: true, Operate: true}
	case RoleViewer:
		return host.Access{View: true, Export: u.CanExport}
	}
	return host.Access{}
}

// Principal is the user as the tracker's ports see them.
func (u User) Principal() host.Principal {
	return host.Principal{ExternalRef: u.ExternalRef, Label: u.Username}
}

// Service owns the account tables.
type Service struct {
	Pool *pgxpool.Pool
}

// NewService returns a Service over the tracker database.
func NewService(pool *pgxpool.Pool) *Service { return &Service{Pool: pool} }

const userCols = `u.id, u.actor_id, a.external_ref, u.username, u.role, u.can_export, u.disabled,
	u.access_basis, u.created_at, u.last_login_at`

const userFrom = ` FROM tracker_users u JOIN tracker_actors a ON a.id = u.actor_id`

type rowScanner interface{ Scan(dest ...any) error }

// scanUser reads userCols plus any extra trailing columns into extra.
func scanUser(row rowScanner, extra ...any) (User, error) {
	var u User
	var role string
	dest := append([]any{&u.ID, &u.ActorID, &u.ExternalRef, &u.Username, &role, &u.CanExport, &u.Disabled,
		&u.AccessBasis, &u.CreatedAt, &u.LastLoginAt}, extra...)
	err := row.Scan(dest...)
	u.Role = Role(role)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// User returns one user by id.
func (s *Service) User(ctx context.Context, id int64) (User, error) {
	return scanUser(s.Pool.QueryRow(ctx, `SELECT `+userCols+userFrom+` WHERE u.id = $1`, id))
}

// UserByName returns one user by username (case-insensitive).
func (s *Service) UserByName(ctx context.Context, username string) (User, error) {
	return scanUser(s.Pool.QueryRow(ctx, `SELECT `+userCols+userFrom+` WHERE lower(u.username) = lower($1)`,
		strings.TrimSpace(username)))
}

// Users lists every account, active first, then by name.
func (s *Service) Users(ctx context.Context) ([]User, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+userCols+userFrom+` ORDER BY u.disabled, lower(u.username)`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetupNeeded reports whether the instance has no active admin yet, in
// which case everything redirects to the setup wizard.
func (s *Service) SetupNeeded(ctx context.Context) (bool, error) {
	var n int
	err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM tracker_users WHERE role = 'admin' AND NOT disabled`).Scan(&n)
	return n == 0, err
}

// ReviewerCount is how many active users can review (reviewer or admin).
// Quarantine release needs two distinct ones.
func (s *Service) ReviewerCount(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM tracker_users WHERE role IN ('admin','reviewer') AND NOT disabled`).Scan(&n)
	return n, err
}

// ReviewerWarning is the warning to show while fewer than two active users
// can review; "" when there are enough.
func (s *Service) ReviewerWarning(ctx context.Context) (string, error) {
	n, err := s.ReviewerCount(ctx)
	if err != nil || n >= 2 {
		return "", err
	}
	return fmt.Sprintf("Only %d active user can review. Releasing a quarantined claim needs votes "+
		"from two different reviewers, so add another reviewer or admin.", n), nil
}

// Authenticate checks a username and password. Disabled users and wrong
// passwords get the same error.
func (s *Service) Authenticate(ctx context.Context, username, password string) (User, error) {
	var hash string
	u, err := scanUser(s.Pool.QueryRow(ctx, `SELECT `+userCols+`, u.password_hash`+userFrom+
		` WHERE lower(u.username) = lower($1)`, strings.TrimSpace(username)), &hash)
	if errors.Is(err, ErrNotFound) {
		// Spend the same time as a real check so usernames can't be probed.
		_, _ = VerifyPassword(dummyHash, password)
		return User{}, ErrInvalidLogin
	}
	if err != nil {
		return User{}, err
	}
	ok, err := VerifyPassword(hash, password)
	if err != nil {
		return User{}, err
	}
	if !ok || u.Disabled {
		return User{}, ErrInvalidLogin
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE tracker_users SET last_login_at = now() WHERE id = $1`, u.ID); err != nil {
		return User{}, err
	}
	return u, nil
}

// dummyHash is verified against when the username doesn't exist.
var dummyHash = func() string {
	h, err := HashPassword("not-a-real-password")
	if err != nil {
		panic(err)
	}
	return h
}()

// NewUser is a create request.
type NewUser struct {
	Username  string
	Password  string
	Role      Role
	CanExport bool
	Note      string // access-basis note (required)
}

func (n NewUser) validate() error {
	n.Username = strings.TrimSpace(n.Username)
	if n.Username == "" || len(n.Username) > 100 {
		return errors.New("username is required (at most 100 characters)")
	}
	if !n.Role.Valid() {
		return fmt.Errorf("unknown role %q", n.Role)
	}
	if strings.TrimSpace(n.Note) == "" {
		return ErrNoteRequired
	}
	return ValidatePassword(n.Password)
}

// Setup creates the first admin. It refuses once any active admin exists,
// so the wizard cannot be re-run; concurrent attempts are serialised.
func (s *Service) Setup(ctx context.Context, username, password string) (User, error) {
	n := NewUser{Username: username, Password: password, Role: RoleAdmin,
		Note: "first-run setup: initial administrator"}
	if err := n.validate(); err != nil {
		return User{}, err
	}
	var out User
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// Serialise concurrent wizard submissions: only one can create
		// the first admin.
		if _, err := tx.Exec(ctx, `LOCK TABLE tracker_users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		var admins int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM tracker_users WHERE role = 'admin' AND NOT disabled`).Scan(&admins); err != nil {
			return err
		}
		if admins > 0 {
			return ErrSetupDone
		}
		id, err := createUserTx(ctx, tx, n, 0)
		if err != nil {
			return err
		}
		if err := logTx(ctx, tx, nil, n.Username, "user_setup",
			fmt.Sprintf("created initial admin %q", n.Username)); err != nil {
			return err
		}
		out.ID = id
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return s.User(ctx, out.ID)
}

// CreateUser adds an account. by is the acting admin (for the audit row).
func (s *Service) CreateUser(ctx context.Context, by User, n NewUser) (User, error) {
	if err := n.validate(); err != nil {
		return User{}, err
	}
	var id int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if id, err = createUserTx(ctx, tx, n, 0); err != nil {
			return err
		}
		return logTx(ctx, tx, &by.ActorID, by.Username, "user_create",
			fmt.Sprintf("created %q as %s%s; basis: %s", n.Username, n.Role, exportSuffix(n.CanExport), n.Note))
	})
	if err != nil {
		return User{}, err
	}
	return s.User(ctx, id)
}

// ClaimActor gives a legacy actor (imported from another instance, so with
// no login) a local account, keeping its audit history attached.
func (s *Service) ClaimActor(ctx context.Context, by User, actorID int64, n NewUser) (User, error) {
	if err := n.validate(); err != nil {
		return User{}, err
	}
	var id int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var label string
		err := tx.QueryRow(ctx, `
			SELECT a.display_label FROM tracker_actors a
			 WHERE a.id = $1 AND a.kind = 'user'
			   AND NOT EXISTS (SELECT 1 FROM tracker_users u WHERE u.actor_id = a.id)
			   FOR UPDATE`, actorID).Scan(&label)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotClaimable
		}
		if err != nil {
			return err
		}
		if id, err = createUserTx(ctx, tx, n, actorID); err != nil {
			return err
		}
		return logTx(ctx, tx, &by.ActorID, by.Username, "user_claim",
			fmt.Sprintf("claimed actor #%d (%s) as %q, %s%s; basis: %s",
				actorID, label, n.Username, n.Role, exportSuffix(n.CanExport), n.Note))
	})
	if err != nil {
		return User{}, err
	}
	return s.User(ctx, id)
}

// ClaimableActor is a legacy actor nobody can sign in as yet.
type ClaimableActor struct {
	ID          int64
	ExternalRef string
	Label       string
}

// ClaimableActors lists person actors with no local account.
func (s *Service) ClaimableActors(ctx context.Context) ([]ClaimableActor, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT a.id, a.external_ref, a.display_label FROM tracker_actors a
		 WHERE a.kind = 'user' AND NOT EXISTS (SELECT 1 FROM tracker_users u WHERE u.actor_id = a.id)
		 ORDER BY lower(a.display_label), a.id`)
	if err != nil {
		return nil, fmt.Errorf("list claimable actors: %w", err)
	}
	defer rows.Close()
	var out []ClaimableActor
	for rows.Next() {
		var a ClaimableActor
		if err := rows.Scan(&a.ID, &a.ExternalRef, &a.Label); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// createUserTx inserts the user (and, unless actorID is given, its new
// local actor) and returns the user id.
func createUserTx(ctx context.Context, tx pgx.Tx, n NewUser, actorID int64) (int64, error) {
	username := strings.TrimSpace(n.Username)
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tracker_users WHERE lower(username) = lower($1))`,
		username).Scan(&taken); err != nil {
		return 0, err
	}
	if taken {
		return 0, ErrUsernameTaken
	}
	hash, err := HashPassword(n.Password)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow(ctx, `SELECT nextval(pg_get_serial_sequence('tracker_users', 'id'))`).Scan(&id); err != nil {
		return 0, err
	}
	if actorID == 0 {
		if err := tx.QueryRow(ctx, `
			INSERT INTO tracker_actors (external_ref, display_label, kind)
			VALUES ($1, $2, 'user') RETURNING id`, fmt.Sprintf("local:%d", id), username).Scan(&actorID); err != nil {
			return 0, fmt.Errorf("create actor: %w", err)
		}
	}
	canExport := n.CanExport && n.Role == RoleViewer // reviewers and admins imply it
	if _, err := tx.Exec(ctx, `
		INSERT INTO tracker_users (id, actor_id, username, password_hash, role, can_export, access_basis)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, actorID, username, hash, string(n.Role), canExport, strings.TrimSpace(n.Note)); err != nil {
		return 0, fmt.Errorf("create user: %w", err)
	}
	return id, nil
}

// SetRole changes a user's role. A note is required; the last active admin
// cannot be demoted.
func (s *Service) SetRole(ctx context.Context, by User, userID int64, role Role, note string) error {
	if !role.Valid() {
		return fmt.Errorf("unknown role %q", role)
	}
	if strings.TrimSpace(note) == "" {
		return ErrNoteRequired
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		target, err := lockUserTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		if target.Role == RoleAdmin && role != RoleAdmin && !target.Disabled {
			if err := requireOtherAdminTx(ctx, tx, userID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE tracker_users SET role = $2, can_export = can_export AND $2 = 'viewer',
			       access_basis = $3, updated_at = now() WHERE id = $1`,
			userID, string(role), strings.TrimSpace(note)); err != nil {
			return err
		}
		return logTx(ctx, tx, &by.ActorID, by.Username, "user_role",
			fmt.Sprintf("%q: %s -> %s; basis: %s", target.Username, target.Role, role, note))
	})
}

// SetExport grants or revokes a viewer's export permission. Granting needs
// a note; it is meaningless for reviewers and admins, which imply it.
func (s *Service) SetExport(ctx context.Context, by User, userID int64, canExport bool, note string) error {
	if canExport && strings.TrimSpace(note) == "" {
		return ErrNoteRequired
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		target, err := lockUserTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		if target.Role != RoleViewer {
			return fmt.Errorf("%s implies export; the permission is only set on viewers", target.Role)
		}
		basis := target.AccessBasis
		if canExport {
			basis = strings.TrimSpace(note)
		}
		if _, err := tx.Exec(ctx, `UPDATE tracker_users SET can_export = $2, access_basis = $3, updated_at = now()
			WHERE id = $1`, userID, canExport, basis); err != nil {
			return err
		}
		verb := "revoked"
		if canExport {
			verb = "granted"
		}
		detail := fmt.Sprintf("%q: export %s", target.Username, verb)
		if note != "" {
			detail += "; basis: " + note
		}
		return logTx(ctx, tx, &by.ActorID, by.Username, "user_export", detail)
	})
}

// SetDisabled disables or re-enables a user. Disabling ends their sessions;
// the last active admin cannot be disabled.
func (s *Service) SetDisabled(ctx context.Context, by User, userID int64, disabled bool, note string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		target, err := lockUserTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		if disabled && target.Role == RoleAdmin && !target.Disabled {
			if err := requireOtherAdminTx(ctx, tx, userID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE tracker_users SET disabled = $2, updated_at = now() WHERE id = $1`,
			userID, disabled); err != nil {
			return err
		}
		surface := "user_enable"
		if disabled {
			surface = "user_disable"
			if _, err := tx.Exec(ctx, `DELETE FROM tracker_sessions WHERE user_id = $1`, userID); err != nil {
				return err
			}
		}
		detail := fmt.Sprintf("%q", target.Username)
		if strings.TrimSpace(note) != "" {
			detail += "; note: " + strings.TrimSpace(note)
		}
		return logTx(ctx, tx, &by.ActorID, by.Username, surface, detail)
	})
}

// ResetPassword sets a new password and ends the user's sessions. by may be
// the zero User (the CLI, run by whoever operates the server).
func (s *Service) ResetPassword(ctx context.Context, by User, userID int64, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		target, err := lockUserTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tracker_users SET password_hash = $2, updated_at = now() WHERE id = $1`,
			userID, hash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM tracker_sessions WHERE user_id = $1`, userID); err != nil {
			return err
		}
		var actor *int64
		label := "cli"
		if by.ActorID != 0 {
			actor, label = &by.ActorID, by.Username
		}
		return logTx(ctx, tx, actor, label, "user_password_reset", fmt.Sprintf("%q", target.Username))
	})
}

func lockUserTx(ctx context.Context, tx pgx.Tx, id int64) (User, error) {
	// Admin-affecting changes serialise on the table so two concurrent
	// demotions can't both pass the last-admin check.
	if _, err := tx.Exec(ctx, `LOCK TABLE tracker_users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return User{}, err
	}
	return scanUser(tx.QueryRow(ctx, `SELECT `+userCols+userFrom+` WHERE u.id = $1`, id))
}

func requireOtherAdminTx(ctx context.Context, tx pgx.Tx, exceptID int64) error {
	var others int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tracker_users
		WHERE role = 'admin' AND NOT disabled AND id <> $1`, exceptID).Scan(&others); err != nil {
		return err
	}
	if others == 0 {
		return ErrLastAdmin
	}
	return nil
}

func logTx(ctx context.Context, tx pgx.Tx, actorID *int64, label, surface, detail string) error {
	_, err := tx.Exec(ctx, `INSERT INTO tracker_access_log (actor_id, actor_label, surface, detail)
		VALUES ($1, $2, $3, $4)`, actorID, label, surface, detail)
	if err != nil {
		return fmt.Errorf("access log: %w", err)
	}
	return nil
}

func exportSuffix(canExport bool) string {
	if canExport {
		return " with export"
	}
	return ""
}

func (s *Service) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
