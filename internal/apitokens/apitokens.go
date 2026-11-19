// Package apitokens manages per-site API tokens and public (unauthenticated) API permissions,
// and decides what an API caller may do.
package apitokens

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"mino/internal/auth"
	"mino/internal/db"
	"mino/internal/schema"
)

// Token types, as in Strapi.
const (
	ReadOnly   = "read-only"
	FullAccess = "full-access"
	Custom     = "custom"
)

// API actions on a content type. Permissions are written "action:uid", e.g. "find:article".
const (
	Find    = "find"
	FindOne = "findOne"
	Create  = "create"
	Update  = "update"
	Delete  = "delete"
)

// Media library permissions.
const (
	UploadRead  = "upload:read"
	UploadWrite = "upload:write"
)

// CollectionActions and SingleActions list the actions that apply to each kind.
var (
	CollectionActions = []string{Find, FindOne, Create, Update, Delete}
	SingleActions     = []string{Find, Update, Delete}
)

// Perm builds a permission string.
func Perm(action, uid string) string { return action + ":" + uid }

var (
	// ErrInvalid is returned for unknown, expired or malformed tokens.
	ErrInvalid = errors.New("invalid API token")
	// ErrNotFound is returned for unknown tokens in management calls.
	ErrNotFound = errors.New("token not found")
)

// Token is an API credential bound to one site.
type Token struct {
	ID          int64
	SiteID      int64
	Name        string
	Description string
	Type        string
	Prefix      string // first characters, shown to identify the token
	Permissions []string
	ExpiresAt   *time.Time
	LastUsedAt  *time.Time
	CreatedAt   time.Time
}

// Expired reports whether the token can no longer be used.
func (t *Token) Expired() bool { return t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt) }

// Service stores tokens and public permissions.
type Service struct {
	pool *pgxpool.Pool
}

// NewService creates a token service.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

const cols = `id, site_id, name, description, type, token_prefix, permissions, expires_at, last_used_at, created_at`

func scan(row pgx.Row) (*Token, error) {
	t := &Token{}
	err := row.Scan(&t.ID, &t.SiteID, &t.Name, &t.Description, &t.Type, &t.Prefix, &t.Permissions, &t.ExpiresAt, &t.LastUsedAt, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func validPermission(p string) bool {
	if p == UploadRead || p == UploadWrite {
		return true
	}
	action, uid, ok := strings.Cut(p, ":")
	return ok && uid != "" && slices.Contains(CollectionActions, action)
}

func validate(t *Token) error {
	ve := &schema.ValidationError{Message: "Invalid API token"}
	t.Name, t.Description = strings.TrimSpace(t.Name), strings.TrimSpace(t.Description)
	if t.Name == "" {
		ve.Add("Name is required", "name")
	} else if len(t.Name) > 100 {
		ve.Add("Name must be at most 100 characters", "name")
	}
	if !slices.Contains([]string{ReadOnly, FullAccess, Custom}, t.Type) {
		ve.Add("Choose a token type", "type")
	}
	if t.Type != Custom {
		t.Permissions = []string{}
	}
	for _, p := range t.Permissions {
		if !validPermission(p) {
			ve.Add("Unknown permission "+p, "permissions")
		}
	}
	if t.Permissions == nil {
		t.Permissions = []string{}
	}
	return ve.OrNil()
}

func nameTaken(err error) error {
	if _, ok := db.IsUniqueViolation(err); ok {
		return &schema.ValidationError{Message: "Invalid API token", Errors: []schema.FieldError{{Path: []string{"name"}, Message: "Another token has this name"}}}
	}
	return err
}

func newSecret() string { return "mino_" + auth.RandomToken(32) }

// Create stores a token and returns its secret, which is shown once and never stored.
func (s *Service) Create(ctx context.Context, t *Token, userID int64) (string, *Token, error) {
	if err := validate(t); err != nil {
		return "", nil, err
	}
	secret := newSecret()
	var creator *int64
	if userID > 0 {
		creator = &userID
	}
	out, err := scan(s.pool.QueryRow(ctx, `INSERT INTO api_tokens (site_id, name, description, type, token_hash, token_prefix, permissions, expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+cols,
		t.SiteID, t.Name, t.Description, t.Type, auth.HashToken(secret), secret[:12], t.Permissions, t.ExpiresAt, creator))
	if err != nil {
		return "", nil, nameTaken(err)
	}
	return secret, out, nil
}

// Update changes a token's name, description, type and permissions (not its secret or expiry).
func (s *Service) Update(ctx context.Context, t *Token) (*Token, error) {
	if err := validate(t); err != nil {
		return nil, err
	}
	out, err := scan(s.pool.QueryRow(ctx, `UPDATE api_tokens SET name = $3, description = $4, type = $5, permissions = $6
		WHERE id = $1 AND site_id = $2 RETURNING `+cols, t.ID, t.SiteID, t.Name, t.Description, t.Type, t.Permissions))
	if err != nil {
		return nil, nameTaken(err)
	}
	return out, nil
}

// Regenerate replaces a token's secret.
func (s *Service) Regenerate(ctx context.Context, siteID, id int64) (string, error) {
	secret := newSecret()
	tag, err := s.pool.Exec(ctx, `UPDATE api_tokens SET token_hash = $3, token_prefix = $4 WHERE id = $1 AND site_id = $2`,
		id, siteID, auth.HashToken(secret), secret[:12])
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	return secret, nil
}

// List returns a site's tokens.
func (s *Service) List(ctx context.Context, siteID int64) ([]*Token, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM api_tokens WHERE site_id = $1 ORDER BY lower(name)`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Token
	for rows.Next() {
		t, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Get returns one token of a site.
func (s *Service) Get(ctx context.Context, siteID, id int64) (*Token, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM api_tokens WHERE id = $1 AND site_id = $2`, id, siteID))
}

// Delete revokes a token.
func (s *Service) Delete(ctx context.Context, siteID, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM api_tokens WHERE id = $1 AND site_id = $2`, id, siteID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Authenticate resolves a bearer secret. last_used_at is written at most once a minute.
func (s *Service) Authenticate(ctx context.Context, secret string) (*Token, error) {
	if !strings.HasPrefix(secret, "mino_") || len(secret) > 100 {
		return nil, ErrInvalid
	}
	t, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM api_tokens WHERE token_hash = $1`, auth.HashToken(secret)))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, err
	}
	if t.Expired() {
		return nil, ErrInvalid
	}
	if t.LastUsedAt == nil || time.Since(*t.LastUsedAt) > time.Minute {
		_, _ = s.pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = now() WHERE id = $1`, t.ID)
	}
	return t, nil
}

// PublicPermissions returns what unauthenticated callers may do on a site.
func (s *Service) PublicPermissions(ctx context.Context, siteID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT action, subject FROM public_permissions WHERE site_id = $1 ORDER BY 1, 2`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a, sub string
		if err := rows.Scan(&a, &sub); err != nil {
			return nil, err
		}
		out = append(out, Perm(a, sub))
	}
	return out, rows.Err()
}

// SetPublicPermissions replaces a site's public permissions. Only read actions are accepted:
// anonymous writes are too easy to abuse.
func (s *Service) SetPublicPermissions(ctx context.Context, siteID int64, perms []string) error {
	for _, p := range perms {
		action, _, _ := strings.Cut(p, ":")
		if !validPermission(p) || (action != Find && action != FindOne) {
			return &schema.ValidationError{Message: "Public access is limited to find and findOne: " + p}
		}
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM public_permissions WHERE site_id = $1`, siteID); err != nil {
			return err
		}
		for _, p := range perms {
			action, uid, _ := strings.Cut(p, ":")
			if _, err := tx.Exec(ctx, `INSERT INTO public_permissions (site_id, action, subject) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, siteID, action, uid); err != nil {
				return err
			}
		}
		return nil
	})
}

// Principal is an API caller: a token, or the public when Token is nil.
type Principal struct {
	Token  *Token
	Public []string // public permissions of the site, used when Token is nil
}

// Authenticated reports whether a token was presented.
func (p *Principal) Authenticated() bool { return p.Token != nil }

// Allowed reports whether the caller may perform a permission such as "find:article" or "upload:read".
func (p *Principal) Allowed(perm string) bool {
	if p.Token == nil {
		return slices.Contains(p.Public, perm)
	}
	switch p.Token.Type {
	case FullAccess:
		return true
	case ReadOnly:
		action, _, _ := strings.Cut(perm, ":")
		return action == Find || action == FindOne || perm == UploadRead
	}
	return slices.Contains(p.Token.Permissions, perm)
}
