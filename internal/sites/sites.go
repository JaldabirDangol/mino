// Package sites manages websites (projects). Each site owns its content types, entries,
// media, API tokens and public API permissions.
package sites

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"mino/internal/db"
	"mino/internal/schema"
)

// ErrNotFound is returned when a site does not exist.
var ErrNotFound = errors.New("site not found")

// Site is one website managed by this installation.
type Site struct {
	ID          int64
	Slug        string // used in URLs: /api/{slug}/..., /admin/s/{slug}/...
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

var reservedSlugs = []string{"admin", "api", "static", "uploads", "health", "healthz", "login", "logout", "setup", "s"}

// Validate checks name and slug.
func (s *Site) Validate() error {
	ve := &schema.ValidationError{Message: "Invalid site"}
	s.Name = strings.TrimSpace(s.Name)
	s.Slug = strings.TrimSpace(s.Slug)
	s.Description = strings.TrimSpace(s.Description)
	if s.Name == "" {
		ve.Add("Name is required", "name")
	} else if len(s.Name) > 80 {
		ve.Add("Name must be at most 80 characters", "name")
	}
	switch {
	case s.Slug == "":
		ve.Add("Slug is required", "slug")
	case len(s.Slug) > 60:
		ve.Add("Slug must be at most 60 characters", "slug")
	case !slugPattern.MatchString(s.Slug):
		ve.Add("Use lowercase letters, digits and dashes (e.g. acme-blog)", "slug")
	case slices.Contains(reservedSlugs, s.Slug):
		ve.Add(`"`+s.Slug+`" is reserved`, "slug")
	}
	return ve.OrNil()
}

// Service stores sites.
type Service struct {
	pool *pgxpool.Pool
}

// NewService creates a site service.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

const cols = `id, slug, name, description, created_at, updated_at`

func scan(row pgx.Row) (*Site, error) {
	s := &Site{}
	err := row.Scan(&s.ID, &s.Slug, &s.Name, &s.Description, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

func collect(rows pgx.Rows, err error) ([]*Site, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Site
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// List returns every site ordered by name.
func (s *Service) List(ctx context.Context) ([]*Site, error) {
	return collect(s.pool.Query(ctx, `SELECT `+cols+` FROM sites ORDER BY lower(name)`))
}

// ListForMember returns the sites a (non-super-admin) user belongs to.
func (s *Service) ListForMember(ctx context.Context, userID int64) ([]*Site, error) {
	return collect(s.pool.Query(ctx, `SELECT `+cols+` FROM sites
		WHERE id IN (SELECT site_id FROM site_members WHERE user_id = $1) ORDER BY lower(name)`, userID))
}

// Get returns a site by slug.
func (s *Service) Get(ctx context.Context, slug string) (*Site, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM sites WHERE slug = $1`, slug))
}

// GetByID returns a site by id.
func (s *Service) GetByID(ctx context.Context, id int64) (*Site, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM sites WHERE id = $1`, id))
}

func slugTaken(err error) error {
	if _, ok := db.IsUniqueViolation(err); ok {
		return &schema.ValidationError{Message: "Invalid site", Errors: []schema.FieldError{{Path: []string{"slug"}, Message: "Already used by another site"}}}
	}
	return err
}

// Create stores a new site.
func (s *Service) Create(ctx context.Context, site *Site) (*Site, error) {
	if err := site.Validate(); err != nil {
		return nil, err
	}
	out, err := scan(s.pool.QueryRow(ctx, `INSERT INTO sites (slug, name, description) VALUES ($1, $2, $3) RETURNING `+cols,
		site.Slug, site.Name, site.Description))
	if err != nil {
		return nil, slugTaken(err)
	}
	return out, nil
}

// Update changes name, slug and description. Changing the slug changes the site's API URLs.
func (s *Service) Update(ctx context.Context, site *Site) (*Site, error) {
	if err := site.Validate(); err != nil {
		return nil, err
	}
	out, err := scan(s.pool.QueryRow(ctx, `UPDATE sites SET slug = $2, name = $3, description = $4, updated_at = now()
		WHERE id = $1 RETURNING `+cols, site.ID, site.Slug, site.Name, site.Description))
	if err != nil {
		return nil, slugTaken(err)
	}
	return out, nil
}

// Stats summarizes what deleting a site would remove.
type Stats struct {
	ContentTypes int
	Entries      int
	Media        int
}

// Stats counts the content owned by a site.
func (s *Service) Stats(ctx context.Context, id int64) (Stats, error) {
	var st Stats
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM content_types WHERE site_id = $1),
		(SELECT count(*) FROM entries e JOIN content_types ct ON ct.id = e.content_type_id WHERE ct.site_id = $1),
		(SELECT count(*) FROM media WHERE site_id = $1)`, id).Scan(&st.ContentTypes, &st.Entries, &st.Media)
	return st, err
}

// Delete removes a site and everything it owns from the database. Stored media files must
// be removed separately (see media.Service.DeleteSiteFiles) before calling Delete.
func (s *Service) Delete(ctx context.Context, id int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Partial unique indexes reference content type ids and would outlive the cascade.
		rows, err := tx.Query(ctx, `SELECT id, fields FROM content_types WHERE site_id = $1`, id)
		if err != nil {
			return err
		}
		var indexes []string
		var subjects []string
		for rows.Next() {
			ct := &schema.ContentType{}
			var fields []byte
			if err := rows.Scan(&ct.ID, &fields); err != nil {
				rows.Close()
				return err
			}
			_ = json.Unmarshal(fields, &ct.Fields)
			for _, f := range ct.Fields {
				if f.Unique {
					indexes = append(indexes, ct.UniqueIndexName(f.Name))
				}
			}
			subjects = append(subjects, schema.PermissionSubject(ct.ID))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, idx := range indexes {
			if _, err := tx.Exec(ctx, `DROP INDEX IF EXISTS `+pgx.Identifier{idx}.Sanitize()); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM role_permissions WHERE subject = ANY($1)`, subjects); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM sites WHERE id = $1`, id)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return err
	})
}
