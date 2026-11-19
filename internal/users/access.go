package users

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"mino/internal/schema"
)

// ErrNoAccess is returned when a user is not a member of a site.
var ErrNoAccess = errors.New("no access to this site")

// Access is what one user may do on one site. Super admins may do everything.
type Access struct {
	User     *User
	SiteID   int64
	RoleName string
	perms    []Permission
}

// SiteAccess resolves a user's permissions on a site.
func (s *Service) SiteAccess(ctx context.Context, u *User, siteID int64) (*Access, error) {
	a := &Access{User: u, SiteID: siteID}
	if u.SuperAdmin {
		a.RoleName = "Super Admin"
		return a, nil
	}
	var roleID int64
	err := s.pool.QueryRow(ctx, `SELECT r.id, r.name FROM site_members m JOIN roles r ON r.id = m.role_id
		WHERE m.site_id = $1 AND m.user_id = $2`, siteID, u.ID).Scan(&roleID, &a.RoleName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoAccess
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT action, subject, own_only FROM role_permissions WHERE role_id = $1`, roleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Permission
		if err := rows.Scan(&p.Action, &p.Subject, &p.OwnOnly); err != nil {
			return nil, err
		}
		a.perms = append(a.perms, p)
	}
	return a, rows.Err()
}

// Super reports whether the user is a super admin.
func (a *Access) Super() bool { return a.User.SuperAdmin }

// Can reports whether a site-wide action (media, schema, tokens) is allowed.
func (a *Access) Can(action string) bool {
	if a.Super() {
		return true
	}
	for _, p := range a.perms {
		if p.Action == action && p.Subject == "" {
			return true
		}
	}
	return false
}

// content returns (allowed at all, allowed for every entry) for a content action.
func (a *Access) content(action string, ct *schema.ContentType) (some, all bool) {
	if a.Super() {
		return true, true
	}
	subject := schema.PermissionSubject(ct.ID)
	for _, p := range a.perms {
		if p.Action == action && (p.Subject == "*" || p.Subject == subject) {
			some = true
			if !p.OwnOnly {
				all = true
			}
		}
	}
	return some, all
}

// CanContent reports whether the action is allowed on at least some entries of the type.
func (a *Access) CanContent(action string, ct *schema.ContentType) bool {
	ok, _ := a.content(action, ct)
	return ok
}

// OwnOnly reports whether the action is limited to the user's own entries.
func (a *Access) OwnOnly(action string, ct *schema.ContentType) bool {
	ok, all := a.content(action, ct)
	return ok && !all
}

// CanEntry reports whether the action is allowed on an entry created by createdBy.
func (a *Access) CanEntry(action string, ct *schema.ContentType, createdBy *int64) bool {
	ok, all := a.content(action, ct)
	if all {
		return true
	}
	return ok && createdBy != nil && *createdBy == a.User.ID
}
