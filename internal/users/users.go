// Package users manages admin users, roles, per-site memberships and permission checks.
package users

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"mino/internal/auth"
	"mino/internal/db"
	"mino/internal/schema"
)

var (
	// ErrNotFound is returned for unknown users or roles.
	ErrNotFound = errors.New("not found")
	// ErrInvalidCredentials is returned by Authenticate for any login failure.
	ErrInvalidCredentials = errors.New("invalid email or password")
	// ErrLastSuperAdmin prevents locking everyone out.
	ErrLastSuperAdmin = errors.New("at least one active super admin is required")
)

// User is an admin panel account.
type User struct {
	ID          int64
	Email       string
	Name        string
	SuperAdmin  bool
	Active      bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LastLoginAt *time.Time
}

// Service stores users, roles and memberships.
type Service struct {
	pool *pgxpool.Pool
}

// NewService creates a user service.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

const userCols = `id, email, name, super_admin, active, created_at, updated_at, last_login_at`

func scanUser(row pgx.Row, extra ...any) (*User, error) {
	u := &User{}
	err := row.Scan(append([]any{&u.ID, &u.Email, &u.Name, &u.SuperAdmin, &u.Active, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt}, extra...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// Input holds editable user attributes. Password is only used when non-empty.
type Input struct {
	Email      string
	Name       string
	Password   string
	SuperAdmin bool
	Active     bool
}

func (in *Input) validate(requirePassword bool) *schema.ValidationError {
	ve := &schema.ValidationError{Message: "Invalid user"}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.Name = strings.TrimSpace(in.Name)
	if a, err := mail.ParseAddress(in.Email); err != nil || a.Address != in.Email {
		ve.Add("Enter a valid email address", "email")
	}
	if in.Name == "" {
		ve.Add("Name is required", "name")
	} else if len(in.Name) > 100 {
		ve.Add("Name must be at most 100 characters", "name")
	}
	if requirePassword || in.Password != "" {
		if err := auth.ValidatePassword(in.Password); err != nil {
			ve.Add("Password "+err.Error(), "password")
		}
	}
	return ve
}

func emailTaken(err error) error {
	if _, ok := db.IsUniqueViolation(err); ok {
		return &schema.ValidationError{Message: "Invalid user", Errors: []schema.FieldError{{Path: []string{"email"}, Message: "Another user has this email"}}}
	}
	return err
}

// Count returns the number of users (zero means first-run setup is needed).
func (s *Service) Count(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM admin_users`).Scan(&n)
	return n, err
}

// Create adds a user.
func (s *Service) Create(ctx context.Context, in Input) (*User, error) {
	if ve := in.validate(true); len(ve.Errors) > 0 {
		return nil, ve
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	u, err := scanUser(s.pool.QueryRow(ctx, `INSERT INTO admin_users (email, name, password_hash, super_admin, active)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+userCols, in.Email, in.Name, hash, in.SuperAdmin, in.Active))
	if err != nil {
		return nil, emailTaken(err)
	}
	return u, nil
}

// CreateFirst creates the initial super admin, failing if any user already exists.
func (s *Service) CreateFirst(ctx context.Context, in Input) (*User, error) {
	in.SuperAdmin, in.Active = true, true
	if ve := in.validate(true); len(ve.Errors) > 0 {
		return nil, ve
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	var u *User
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE admin_users IN EXCLUSIVE MODE`); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM admin_users`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return errors.New("setup already completed")
		}
		u, err = scanUser(tx.QueryRow(ctx, `INSERT INTO admin_users (email, name, password_hash, super_admin, active)
			VALUES ($1, $2, $3, true, true) RETURNING `+userCols, in.Email, in.Name, hash))
		return err
	})
	return u, err
}

// Authenticate checks credentials of an active user and records the login time.
func (s *Service) Authenticate(ctx context.Context, email, password string) (*User, error) {
	var hash string
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+`, password_hash FROM admin_users
		WHERE lower(email) = lower($1)`, strings.TrimSpace(email)), &hash)
	if errors.Is(err, ErrNotFound) {
		auth.CheckDummy(password)
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if err := auth.CheckPassword(password, hash); err != nil || !u.Active {
		return nil, ErrInvalidCredentials
	}
	_, _ = s.pool.Exec(ctx, `UPDATE admin_users SET last_login_at = now() WHERE id = $1`, u.ID)
	return u, nil
}

// Get returns a user.
func (s *Service) Get(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM admin_users WHERE id = $1`, id))
}

// List returns a page of users matching search.
func (s *Service) List(ctx context.Context, search string, page, pageSize int) ([]*User, int, error) {
	where, args := "TRUE", []any{}
	if search = strings.TrimSpace(search); search != "" {
		args = append(args, "%"+search+"%")
		where = "(email ILIKE $1 OR name ILIKE $1)"
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM admin_users WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM admin_users WHERE `+where+
		fmt.Sprintf(` ORDER BY lower(name) LIMIT %d OFFSET %d`, pageSize, (max(page, 1)-1)*pageSize), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// guardSuperAdmins fails if no active super admin would remain.
func guardSuperAdmins(ctx context.Context, tx pgx.Tx) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM admin_users WHERE super_admin AND active`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrLastSuperAdmin
	}
	return nil
}

// Update changes a user's profile, flags and (optionally) password.
func (s *Service) Update(ctx context.Context, id int64, in Input) (*User, error) {
	if ve := in.validate(false); len(ve.Errors) > 0 {
		return nil, ve
	}
	var u *User
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		u, err = scanUser(tx.QueryRow(ctx, `UPDATE admin_users SET email = $2, name = $3, super_admin = $4, active = $5, updated_at = now()
			WHERE id = $1 RETURNING `+userCols, id, in.Email, in.Name, in.SuperAdmin, in.Active))
		if err != nil {
			return emailTaken(err)
		}
		if in.Password != "" {
			hash, err := auth.HashPassword(in.Password)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE admin_users SET password_hash = $2 WHERE id = $1`, id, hash); err != nil {
				return err
			}
		}
		if !in.Active {
			if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, id); err != nil {
				return err
			}
		}
		return guardSuperAdmins(ctx, tx)
	})
	return u, err
}

// ChangePassword verifies the current password before setting a new one.
func (s *Service) ChangePassword(ctx context.Context, id int64, current, next string) error {
	var hash string
	if err := s.pool.QueryRow(ctx, `SELECT password_hash FROM admin_users WHERE id = $1`, id).Scan(&hash); err != nil {
		return err
	}
	ve := &schema.ValidationError{Message: "Invalid password"}
	if auth.CheckPassword(current, hash) != nil {
		ve.Add("Current password is incorrect", "currentPassword")
	}
	if err := auth.ValidatePassword(next); err != nil {
		ve.Add("Password "+err.Error(), "password")
	}
	if err := ve.OrNil(); err != nil {
		return err
	}
	newHash, err := auth.HashPassword(next)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE admin_users SET password_hash = $2, updated_at = now() WHERE id = $1`, id, newHash)
	return err
}

// Delete removes a user (and their sessions and memberships).
func (s *Service) Delete(ctx context.Context, id int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM admin_users WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return guardSuperAdmins(ctx, tx)
	})
}

// Membership is a user's role on one site.
type Membership struct {
	SiteID   int64
	SiteSlug string
	SiteName string
	RoleID   int64
	RoleName string
}

// Memberships lists a user's site roles.
func (s *Service) Memberships(ctx context.Context, userID int64) ([]Membership, error) {
	rows, err := s.pool.Query(ctx, `SELECT s.id, s.slug, s.name, r.id, r.name FROM site_members m
		JOIN sites s ON s.id = m.site_id JOIN roles r ON r.id = m.role_id
		WHERE m.user_id = $1 ORDER BY lower(s.name)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Membership
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.SiteID, &m.SiteSlug, &m.SiteName, &m.RoleID, &m.RoleName); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMemberships replaces a user's site roles with roleBySite (site id -> role id).
func (s *Service) SetMemberships(ctx context.Context, userID int64, roleBySite map[int64]int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM site_members WHERE user_id = $1`, userID); err != nil {
			return err
		}
		for site, role := range roleBySite {
			if _, err := tx.Exec(ctx, `INSERT INTO site_members (site_id, user_id, role_id) VALUES ($1, $2, $3)`, site, userID, role); err != nil {
				if db.IsForeignKeyViolation(err) {
					return &schema.ValidationError{Message: "Unknown site or role"}
				}
				return err
			}
		}
		return nil
	})
}
