package content

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"mino/internal/db"
	"mino/internal/media"
	"mino/internal/schema"
)

// Engine performs every content operation. It is site-safe by construction: entries are always
// addressed through a content type, and content types belong to exactly one site.
type Engine struct {
	pool    *pgxpool.Pool
	schemas *schema.Service
	// MediaURL builds the public URL of a file; set by the media service.
	MediaURL func(*media.File) string
}

// New creates an engine.
func New(pool *pgxpool.Pool, schemas *schema.Service) *Engine {
	return &Engine{pool: pool, schemas: schemas}
}

// WriteOptions control create and update.
type WriteOptions struct {
	UserID int64
	// Status changes the publication state: StatusPublished publishes, StatusDraft unpublishes,
	// StatusAny keeps the current state (new entries start as drafts). Types without
	// draft & publish are always published.
	Status Status
}

// ReadOptions control single-entry reads.
type ReadOptions struct {
	Status   Status   // visibility of the entry and populated relation targets
	Populate []string // link fields to populate
}

// Result is one page of entries.
type Result struct {
	Entries  []*Entry
	Page     int
	PageSize int
	Total    int
}

// PageCount returns the number of pages.
func (r *Result) PageCount() int {
	if r.PageSize == 0 {
		return 0
	}
	return (r.Total + r.PageSize - 1) / r.PageSize
}

const entryCols = `e.id, e.content_type_id, e.data, e.published_at, e.created_by, e.updated_by, e.created_at, e.updated_at`

func (en *Engine) queryEntries(ctx context.Context, q db.DBTX, ct *schema.ContentType, where, tail string, args ...any) ([]*Entry, error) {
	rows, err := q.Query(ctx, `SELECT `+entryCols+` FROM entries e WHERE `+where+" "+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Entry
	for rows.Next() {
		e := &Entry{Type: ct}
		if err := rows.Scan(&e.ID, &e.contentTypeID, &e.Data, &e.PublishedAt, &e.CreatedBy, &e.UpdatedBy, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		if e.Data == nil {
			e.Data = map[string]any{}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func statusSQL(s Status) string {
	switch s {
	case StatusPublished:
		return " AND e.published_at IS NOT NULL"
	case StatusDraft:
		return " AND e.published_at IS NULL"
	}
	return ""
}

func linkFields(ct *schema.ContentType) []string {
	var out []string
	for _, f := range ct.Fields {
		if f.IsLink() {
			out = append(out, f.Name)
		}
	}
	return out
}

// Get returns one entry with the ids of all its links and the requested fields populated.
func (en *Engine) Get(ctx context.Context, ct *schema.ContentType, id int64, opts ReadOptions) (*Entry, error) {
	return en.get(ctx, en.pool, ct, id, opts, "")
}

func (en *Engine) get(ctx context.Context, q db.DBTX, ct *schema.ContentType, id int64, opts ReadOptions, lock string) (*Entry, error) {
	list, err := en.queryEntries(ctx, q, ct, `e.id = $1 AND e.content_type_id = $2`+statusSQL(opts.Status), lock, id, ct.ID)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	if err := en.loadLinks(ctx, q, ct, list, linkFields(ct), opts.Populate, opts.Status); err != nil {
		return nil, err
	}
	return list[0], nil
}

// List returns a page of entries matching q. Only populated link fields are loaded.
func (en *Engine) List(ctx context.Context, ct *schema.ContentType, q Query) (*Result, error) {
	b := &sqlBuilder{}
	where := "e.content_type_id = " + b.arg(ct.ID) + statusSQL(q.Status)
	fsql, err := b.filterSQL(ct, q.Filter)
	if err != nil {
		return nil, err
	}
	if fsql != "" {
		where += " AND " + fsql
	}
	if s := strings.TrimSpace(q.Search); s != "" {
		var ors []string
		p := b.arg(likePattern(s, "%", "%"))
		for _, f := range ct.Fields {
			if f.IsTextual() {
				ors = append(ors, "e.data->>"+lit(f.Name)+" ILIKE "+p)
			}
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			ors = append(ors, "e.id = "+b.arg(n))
		}
		if len(ors) == 0 {
			ors = []string{"FALSE"}
		}
		where += " AND (" + strings.Join(ors, " OR ") + ")"
	}
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = DefaultLimits.DefaultPageSize
	}

	res := &Result{Page: q.Page, PageSize: q.PageSize}
	if err := en.pool.QueryRow(ctx, `SELECT count(*) FROM entries e WHERE `+where, b.args...).Scan(&res.Total); err != nil {
		return nil, err
	}
	if res.Total == 0 || (q.Page-1)*q.PageSize >= res.Total {
		res.Entries = []*Entry{}
		return res, nil
	}
	tail := "ORDER BY " + orderSQL(ct, q.Sort) +
		" LIMIT " + strconv.Itoa(q.PageSize) + " OFFSET " + strconv.Itoa((q.Page-1)*q.PageSize)
	res.Entries, err = en.queryEntries(ctx, en.pool, ct, where, tail, b.args...)
	if err != nil {
		return nil, err
	}
	if err := en.loadLinks(ctx, en.pool, ct, res.Entries, q.Populate, q.Populate, q.Status); err != nil {
		return nil, err
	}
	return res, nil
}

// publishedAt computes the publication timestamp after a write.
func publishedAt(ct *schema.ContentType, current *time.Time, s Status) *time.Time {
	now := time.Now().UTC()
	if !ct.DraftPublish {
		if current != nil {
			return current
		}
		return &now
	}
	switch s {
	case StatusPublished:
		if current != nil {
			return current
		}
		return &now
	case StatusDraft:
		return nil
	}
	return current
}

func nullableID(id int64) *int64 {
	if id <= 0 {
		return nil
	}
	return &id
}

// mapWriteError converts unique index violations into field errors.
func mapWriteError(ct *schema.ContentType, err error, ve *schema.ValidationError) error {
	if name, ok := db.IsUniqueViolation(err); ok {
		for _, f := range ct.Fields {
			if f.Unique && ct.UniqueIndexName(f.Name) == name {
				ve.Add("This value is already used by another entry", f.Name)
				return ve
			}
		}
	}
	return err
}

// Create validates input and stores a new entry. Required fields are enforced for published
// entries only, so incomplete drafts can be saved.
func (en *Engine) Create(ctx context.Context, ct *schema.ContentType, input map[string]any, opts WriteOptions) (*Entry, error) {
	data, links, ve := prepare(ct, input, nil)
	pub := publishedAt(ct, nil, opts.Status)
	if pub != nil {
		counts := map[string]int{}
		for k, v := range links {
			counts[k] = len(v)
		}
		checkRequired(ct, data, counts, ve)
	}
	if err := ve.OrNil(); err != nil {
		return nil, err
	}

	var id int64
	err := pgx.BeginFunc(ctx, en.pool, func(tx pgx.Tx) error {
		if ct.Kind == schema.KindSingle {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, ct.ID); err != nil {
				return err
			}
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM entries WHERE content_type_id = $1)`, ct.ID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return ErrSingleExists
			}
		}
		uid := nullableID(opts.UserID)
		if err := tx.QueryRow(ctx, `INSERT INTO entries (content_type_id, data, published_at, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $4) RETURNING id`, ct.ID, data, pub, uid).Scan(&id); err != nil {
			return err
		}
		if err := en.setLinks(ctx, tx, ct, id, links, ve); err != nil {
			return err
		}
		return ve.OrNil()
	})
	if err != nil {
		return nil, mapWriteError(ct, err, ve)
	}
	return en.Get(ctx, ct, id, ReadOptions{})
}

// Update applies a partial update: only keys present in input change; a nil value clears a field.
func (en *Engine) Update(ctx context.Context, ct *schema.ContentType, id int64, input map[string]any, opts WriteOptions) (*Entry, error) {
	ve := newValidationError()
	err := pgx.BeginFunc(ctx, en.pool, func(tx pgx.Tx) error {
		cur, err := en.get(ctx, tx, ct, id, ReadOptions{}, "FOR UPDATE")
		if err != nil {
			return err
		}
		data, links, pve := prepare(ct, input, cur.Data)
		ve = pve
		pub := publishedAt(ct, cur.PublishedAt, opts.Status)
		if pub != nil {
			counts := map[string]int{}
			for k, v := range cur.Links {
				counts[k] = len(v)
			}
			for k, v := range links {
				counts[k] = len(v)
			}
			checkRequired(ct, data, counts, ve)
		}
		if err := ve.OrNil(); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE entries SET data = $2, published_at = $3, updated_by = $4, updated_at = now() WHERE id = $1`,
			id, data, pub, nullableID(opts.UserID)); err != nil {
			return err
		}
		if err := en.setLinks(ctx, tx, ct, id, links, ve); err != nil {
			return err
		}
		return ve.OrNil()
	})
	if err != nil {
		return nil, mapWriteError(ct, err, ve)
	}
	return en.Get(ctx, ct, id, ReadOptions{})
}

// Publish publishes an entry, enforcing required fields.
func (en *Engine) Publish(ctx context.Context, ct *schema.ContentType, id, userID int64) (*Entry, error) {
	return en.Update(ctx, ct, id, nil, WriteOptions{UserID: userID, Status: StatusPublished})
}

// Unpublish turns an entry back into a draft.
func (en *Engine) Unpublish(ctx context.Context, ct *schema.ContentType, id, userID int64) (*Entry, error) {
	return en.Update(ctx, ct, id, nil, WriteOptions{UserID: userID, Status: StatusDraft})
}

// Delete removes an entry and its links.
func (en *Engine) Delete(ctx context.Context, ct *schema.ContentType, id int64) error {
	tag, err := en.pool.Exec(ctx, `DELETE FROM entries WHERE id = $1 AND content_type_id = $2`, id, ct.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Duplicate copies an entry as a new draft. Unique values are cleared and exclusive relations
// (oneToOne, oneToMany) are not copied, since their targets cannot be shared.
func (en *Engine) Duplicate(ctx context.Context, ct *schema.ContentType, id, userID int64) (*Entry, error) {
	src, err := en.Get(ctx, ct, id, ReadOptions{})
	if err != nil {
		return nil, err
	}
	input := map[string]any{}
	for _, f := range ct.Fields {
		switch {
		case f.Type == schema.TypeRelation && f.Relation.ExclusiveTarget():
		case f.IsLink():
			if ids := src.Links[f.Name]; len(ids) > 0 {
				input[f.Name] = ids
			}
		case f.Unique:
		default:
			if v, ok := src.Data[f.Name]; ok {
				input[f.Name] = v
			}
		}
	}
	return en.Create(ctx, ct, input, WriteOptions{UserID: userID, Status: StatusDraft})
}

// GetSingle returns the entry of a single type.
func (en *Engine) GetSingle(ctx context.Context, ct *schema.ContentType, opts ReadOptions) (*Entry, error) {
	var id int64
	err := en.pool.QueryRow(ctx, `SELECT e.id FROM entries e WHERE e.content_type_id = $1`+statusSQL(opts.Status)+` ORDER BY e.id LIMIT 1`, ct.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return en.Get(ctx, ct, id, opts)
}

// UpsertSingle creates or updates the entry of a single type.
func (en *Engine) UpsertSingle(ctx context.Context, ct *schema.ContentType, input map[string]any, opts WriteOptions) (*Entry, error) {
	for attempt := 0; attempt < 2; attempt++ {
		cur, err := en.GetSingle(ctx, ct, ReadOptions{})
		if errors.Is(err, ErrNotFound) {
			e, err := en.Create(ctx, ct, input, opts)
			if errors.Is(err, ErrSingleExists) {
				continue // lost a race with a concurrent create; update instead
			}
			return e, err
		}
		if err != nil {
			return nil, err
		}
		return en.Update(ctx, ct, cur.ID, input, opts)
	}
	return nil, ErrSingleExists
}

// Recent returns the most recently updated entries of a site, for the dashboard.
func (en *Engine) Recent(ctx context.Context, types []*schema.ContentType, limit int) ([]*Entry, error) {
	if len(types) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(types))
	byID := map[int64]*schema.ContentType{}
	for i, t := range types {
		ids[i] = t.ID
		byID[t.ID] = t
	}
	list, err := en.queryEntries(ctx, en.pool, nil, `e.content_type_id = ANY($1)`, `ORDER BY e.updated_at DESC LIMIT `+strconv.Itoa(limit), ids)
	if err != nil {
		return nil, err
	}
	for _, e := range list {
		e.Type = byID[e.contentTypeID]
	}
	return list, nil
}
