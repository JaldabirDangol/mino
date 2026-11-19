package users

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"mino/internal/db"
	"mino/internal/schema"
)

// Content actions apply to a subject: "*" (every content type) or schema.PermissionSubject(id).
const (
	ContentRead    = "content.read"
	ContentCreate  = "content.create"
	ContentUpdate  = "content.update"
	ContentDelete  = "content.delete"
	ContentPublish = "content.publish"
)

// Site actions apply to the whole site (empty subject).
const (
	MediaRead    = "media.read"
	MediaUpload  = "media.upload"
	MediaUpdate  = "media.update"
	MediaDelete  = "media.delete"
	SchemaManage = "schema.manage" // Content-Type Builder
	TokensManage = "tokens.manage" // API tokens and public API permissions
)

// ContentActions lists content actions in display order.
var ContentActions = []string{ContentRead, ContentCreate, ContentUpdate, ContentDelete, ContentPublish}

// SiteActions lists site-wide actions in display order.
var SiteActions = []string{MediaRead, MediaUpload, MediaUpdate, MediaDelete, SchemaManage, TokensManage}

// ActionLabel returns a short human label.
func ActionLabel(a string) string {
	return map[string]string{
		ContentRead: "Read", ContentCreate: "Create", ContentUpdate: "Update", ContentDelete: "Delete", ContentPublish: "Publish",
		MediaRead: "Browse media", MediaUpload: "Upload media", MediaUpdate: "Edit media details", MediaDelete: "Delete media",
		SchemaManage: "Manage content types", TokensManage: "Manage API access",
	}[a]
}

// Permission grants an action on a subject. OwnOnly restricts content actions to entries the
// user created.
type Permission struct {
	Action  string
	Subject string
	OwnOnly bool
}

// Role is a reusable set of permissions assigned to users per site.
type Role struct {
	ID          int64
	Name        string
	Description string
	Permissions []Permission
	Members     int
	CreatedAt   time.Time
}

// Has reports whether the role contains exactly this grant (used by the permission matrix).
func (r *Role) Has(action, subject string) (granted, ownOnly bool) {
	for _, p := range r.Permissions {
		if p.Action == action && p.Subject == subject {
			return true, p.OwnOnly
		}
	}
	return false, false
}

func validatePermissions(perms []Permission) error {
	for _, p := range perms {
		switch {
		case slices.Contains(ContentActions, p.Action):
			if p.Subject != "*" && !strings.HasPrefix(p.Subject, "ct:") {
				return &schema.ValidationError{Message: "Invalid permission subject " + p.Subject}
			}
		case slices.Contains(SiteActions, p.Action):
			if p.Subject != "" || p.OwnOnly {
				return &schema.ValidationError{Message: "Site permissions take no subject"}
			}
		default:
			return &schema.ValidationError{Message: "Unknown action " + p.Action}
		}
	}
	return nil
}

func (s *Service) loadPermissions(ctx context.Context, roles []*Role) error {
	if len(roles) == 0 {
		return nil
	}
	byID := map[int64]*Role{}
	ids := make([]int64, len(roles))
	for i, r := range roles {
		byID[r.ID] = r
		ids[i] = r.ID
	}
	rows, err := s.pool.Query(ctx, `SELECT role_id, action, subject, own_only FROM role_permissions WHERE role_id = ANY($1) ORDER BY action, subject`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var p Permission
		if err := rows.Scan(&id, &p.Action, &p.Subject, &p.OwnOnly); err != nil {
			return err
		}
		byID[id].Permissions = append(byID[id].Permissions, p)
	}
	return rows.Err()
}

// ListRoles returns every role with permissions and member counts.
func (s *Service) ListRoles(ctx context.Context) ([]*Role, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.id, r.name, r.description, r.created_at,
		(SELECT count(*) FROM site_members m WHERE m.role_id = r.id) FROM roles r ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	var out []*Role
	for rows.Next() {
		r := &Role{}
		if err := rows.Scan(&r.ID, &r.Name, &r.Description, &r.CreatedAt, &r.Members); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.loadPermissions(ctx, out)
}

// GetRole returns one role.
func (s *Service) GetRole(ctx context.Context, id int64) (*Role, error) {
	r := &Role{}
	err := s.pool.QueryRow(ctx, `SELECT r.id, r.name, r.description, r.created_at,
		(SELECT count(*) FROM site_members m WHERE m.role_id = r.id) FROM roles r WHERE r.id = $1`, id).
		Scan(&r.ID, &r.Name, &r.Description, &r.CreatedAt, &r.Members)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r, s.loadPermissions(ctx, []*Role{r})
}

func validateRole(name string) error {
	if strings.TrimSpace(name) == "" {
		return &schema.ValidationError{Message: "Invalid role", Errors: []schema.FieldError{{Path: []string{"name"}, Message: "Name is required"}}}
	}
	return nil
}

func nameTaken(err error) error {
	if _, ok := db.IsUniqueViolation(err); ok {
		return &schema.ValidationError{Message: "Invalid role", Errors: []schema.FieldError{{Path: []string{"name"}, Message: "Another role has this name"}}}
	}
	return err
}

func writePermissions(ctx context.Context, tx pgx.Tx, roleID int64, perms []Permission) error {
	if _, err := tx.Exec(ctx, `DELETE FROM role_permissions WHERE role_id = $1`, roleID); err != nil {
		return err
	}
	for _, p := range perms {
		if _, err := tx.Exec(ctx, `INSERT INTO role_permissions (role_id, action, subject, own_only) VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING`, roleID, p.Action, p.Subject, p.OwnOnly); err != nil {
			return err
		}
	}
	return nil
}

// SaveRole creates (ID == 0) or updates a role, replacing its permissions.
func (s *Service) SaveRole(ctx context.Context, r *Role) (*Role, error) {
	r.Name, r.Description = strings.TrimSpace(r.Name), strings.TrimSpace(r.Description)
	if err := validateRole(r.Name); err != nil {
		return nil, err
	}
	if err := validatePermissions(r.Permissions); err != nil {
		return nil, err
	}
	id := r.ID
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if id == 0 {
			err = tx.QueryRow(ctx, `INSERT INTO roles (name, description) VALUES ($1, $2) RETURNING id`, r.Name, r.Description).Scan(&id)
		} else {
			err = tx.QueryRow(ctx, `UPDATE roles SET name = $2, description = $3 WHERE id = $1 RETURNING id`, id, r.Name, r.Description).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
		}
		if err != nil {
			return nameTaken(err)
		}
		return writePermissions(ctx, tx, id, r.Permissions)
	})
	if err != nil {
		return nil, err
	}
	return s.GetRole(ctx, id)
}

// DeleteRole removes a role that no membership uses.
func (s *Service) DeleteRole(ctx context.Context, id int64) error {
	r, err := s.GetRole(ctx, id)
	if err != nil {
		return err
	}
	if r.Members > 0 {
		return &schema.ValidationError{Message: "Role is assigned to users; reassign them first"}
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM roles WHERE id = $1`, id)
	return err
}
