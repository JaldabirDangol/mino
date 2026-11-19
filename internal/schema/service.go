package schema

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"mino/internal/db"
)

// entries formats a count of entries ("1 entry", "3 entries").
func hasHave(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

func entries(n int) string {
	if n == 1 {
		return "1 entry"
	}
	return fmt.Sprintf("%d entries", n)
}

// ErrNotFound is returned when a content type does not exist.
var ErrNotFound = errors.New("content type not found")

// ImpactLevel ranks how a change affects existing content.
type ImpactLevel int

const (
	ImpactNone        ImpactLevel = iota // safe, applied silently
	ImpactWarning                        // existing entries become invalid until edited; needs confirmation
	ImpactDestructive                    // existing data will be deleted; needs confirmation
	ImpactBlocked                        // cannot be applied while data exists
)

// Impact is a change together with its effect on stored entries.
type Impact struct {
	Change   Change
	Level    ImpactLevel
	Affected int
	Message  string
}

// Plan lists the impact of every change in a schema update.
type Plan struct {
	Impacts []Impact
}

// Blocked reports whether any change cannot be applied.
func (p *Plan) Blocked() bool {
	for _, i := range p.Impacts {
		if i.Level == ImpactBlocked {
			return true
		}
	}
	return false
}

// NeedsConfirmation reports whether the update affects existing data.
func (p *Plan) NeedsConfirmation() bool {
	for _, i := range p.Impacts {
		if i.Level == ImpactWarning || i.Level == ImpactDestructive {
			return true
		}
	}
	return false
}

// Notable returns impacts that affect data.
func (p *Plan) Notable() []Impact {
	var out []Impact
	for _, i := range p.Impacts {
		if i.Level != ImpactNone {
			out = append(out, i)
		}
	}
	return out
}

// PlanError is returned when an update is blocked or requires confirmation.
type PlanError struct{ Plan *Plan }

func (e *PlanError) Error() string {
	if e.Plan.Blocked() {
		return "schema change blocked by existing content"
	}
	return "schema change affects existing content and must be confirmed"
}

// Service stores content type definitions.
type Service struct {
	pool *pgxpool.Pool
}

// NewService creates a schema service.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

const selectCols = `id, site_id, uid, plural_name, kind, display_name, description, draft_publish, fields, version, created_at, updated_at`

func scanType(row pgx.Row) (*ContentType, error) {
	ct := &ContentType{}
	var fields []byte
	err := row.Scan(&ct.ID, &ct.SiteID, &ct.UID, &ct.PluralName, &ct.Kind, &ct.DisplayName, &ct.Description,
		&ct.DraftPublish, &fields, &ct.Version, &ct.CreatedAt, &ct.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(fields, &ct.Fields); err != nil {
		return nil, fmt.Errorf("decode fields of %s: %w", ct.UID, err)
	}
	return ct, nil
}

// List returns the content types of a site ordered by display name.
func (s *Service) List(ctx context.Context, siteID int64) ([]*ContentType, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+selectCols+` FROM content_types WHERE site_id = $1 ORDER BY lower(display_name)`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ContentType
	for rows.Next() {
		ct, err := scanType(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ct)
	}
	return out, rows.Err()
}

// Get returns a site's content type by uid.
func (s *Service) Get(ctx context.Context, siteID int64, uid string) (*ContentType, error) {
	return scanType(s.pool.QueryRow(ctx, `SELECT `+selectCols+` FROM content_types WHERE site_id = $1 AND uid = $2`, siteID, uid))
}

// GetByID returns a content type by id.
func (s *Service) GetByID(ctx context.Context, id int64) (*ContentType, error) {
	return scanType(s.pool.QueryRow(ctx, `SELECT `+selectCols+` FROM content_types WHERE id = $1`, id))
}

// GetByRoute resolves a REST path segment: a collection's plural name or a single type's uid.
func (s *Service) GetByRoute(ctx context.Context, siteID int64, name string) (*ContentType, error) {
	return scanType(s.pool.QueryRow(ctx, `SELECT `+selectCols+` FROM content_types
		WHERE site_id = $1 AND ((kind = 'collection' AND plural_name = $2) OR (kind = 'single' AND uid = $2))`, siteID, name))
}

// GetMany returns a site's content types with the given uids, keyed by uid.
func (s *Service) GetMany(ctx context.Context, siteID int64, uids []string) (map[string]*ContentType, error) {
	out := map[string]*ContentType{}
	if len(uids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+selectCols+` FROM content_types WHERE site_id = $1 AND uid = ANY($2)`, siteID, uids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		ct, err := scanType(rows)
		if err != nil {
			return nil, err
		}
		out[ct.UID] = ct
	}
	return out, rows.Err()
}

func (s *Service) lookup(ctx context.Context, siteID int64) (Lookup, []*ContentType, error) {
	all, err := s.List(ctx, siteID)
	if err != nil {
		return nil, nil, err
	}
	return func(uid string) (*ContentType, bool) {
		for _, ct := range all {
			if ct.UID == uid {
				return ct, true
			}
		}
		return nil, false
	}, all, nil
}

// checkNames ensures API ids do not collide with another content type. Route names share one
// namespace: a collection plural must not equal any other type's singular name and vice versa.
func checkNames(ct *ContentType, all []*ContentType) error {
	ve := &ValidationError{Message: "Invalid content type"}
	for _, o := range all {
		if o.ID == ct.ID {
			continue
		}
		taken := []string{o.UID, o.PluralName}
		for _, n := range taken {
			if n == ct.UID {
				ve.Add("Already used by "+o.DisplayName, "uid")
			}
			if n == ct.PluralName {
				ve.Add("Already used by "+o.DisplayName, "pluralName")
			}
		}
	}
	return ve.OrNil()
}

// Create validates and stores a new content type in ct.SiteID.
func (s *Service) Create(ctx context.Context, ct *ContentType, userID int64) (*ContentType, error) {
	if ct.SiteID == 0 {
		return nil, errors.New("content type has no site")
	}
	ct.Normalize()
	lookup, all, err := s.lookup(ctx, ct.SiteID)
	if err != nil {
		return nil, err
	}
	if err := ct.Validate(lookup); err != nil {
		return nil, err
	}
	if err := checkNames(ct, all); err != nil {
		return nil, err
	}
	fields, _ := json.Marshal(ct.Fields)
	var created *ContentType
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		created, err = scanType(tx.QueryRow(ctx, `INSERT INTO content_types
			(site_id, uid, plural_name, kind, display_name, description, draft_publish, fields)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+selectCols,
			ct.SiteID, ct.UID, ct.PluralName, ct.Kind, ct.DisplayName, ct.Description, ct.DraftPublish, fields))
		if err != nil {
			return err
		}
		for _, f := range created.Fields {
			if f.Unique {
				if err := createUniqueIndex(ctx, tx, created, f.Name); err != nil {
					return err
				}
			}
		}
		return recordHistory(ctx, tx, created, userID)
	})
	if name, ok := db.IsUniqueViolation(err); ok {
		ve := &ValidationError{Message: "Invalid content type"}
		if strings.Contains(name, "plural") {
			ve.Add("Already in use", "pluralName")
		} else {
			ve.Add("Already in use", "uid")
		}
		return nil, ve
	}
	return created, err
}

// Plan computes the impact of replacing the stored definition with next.
func (s *Service) Plan(ctx context.Context, current, next *ContentType) (*Plan, error) {
	plan := &Plan{}
	for _, c := range Diff(current, next) {
		imp, err := s.assess(ctx, current, c)
		if err != nil {
			return nil, err
		}
		plan.Impacts = append(plan.Impacts, imp)
	}
	return plan, nil
}

func (s *Service) countEntries(ctx context.Context, ctID int64, where string, args ...any) (int, error) {
	var n int
	q := `SELECT count(*) FROM entries WHERE content_type_id = $1`
	if where != "" {
		q += " AND (" + where + ")"
	}
	err := s.pool.QueryRow(ctx, q, append([]any{ctID}, args...)...).Scan(&n)
	return n, err
}

// countWithValue counts entries holding a value for a field, in the document or in link tables.
func (s *Service) countWithValue(ctx context.Context, ct *ContentType, f *Field) (int, error) {
	switch f.Type {
	case TypeRelation:
		return s.countEntries(ctx, ct.ID, `EXISTS (SELECT 1 FROM relations r WHERE r.source_id = entries.id AND r.field = $2)`, f.Name)
	case TypeMedia:
		return s.countEntries(ctx, ct.ID, `EXISTS (SELECT 1 FROM media_links m WHERE m.entry_id = entries.id AND m.field = $2)`, f.Name)
	}
	return s.countEntries(ctx, ct.ID, `data ? $2`, f.Name)
}

// countInvalid counts stored values that fail validation under field definition nf.
func (s *Service) countInvalid(ctx context.Context, ct *ContentType, name string, nf *Field) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT data->$2 FROM entries WHERE content_type_id = $1 AND data ? $2`, ct.ID, name)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			return 0, err
		}
		if _, err := CoerceValue(*nf, v); err != nil {
			n++
		}
	}
	return n, rows.Err()
}

func (s *Service) assess(ctx context.Context, ct *ContentType, c Change) (Impact, error) {
	imp := Impact{Change: c}
	var err error
	switch c.Kind {
	case FieldAdded, FieldNotUnique:
		// Always safe.
	case FieldRemoved:
		imp.Affected, err = s.countWithValue(ctx, ct, c.Old)
		if imp.Affected > 0 {
			imp.Level = ImpactDestructive
			imp.Message = fmt.Sprintf("%s %s a value for %q. Those values will be permanently deleted.", entries(imp.Affected), hasHave(imp.Affected), c.Field)
		}
	case FieldTypeChanged:
		imp.Affected, err = s.countWithValue(ctx, ct, c.Old)
		if err != nil || imp.Affected == 0 {
			break
		}
		if c.Old.IsLink() || c.New.IsLink() || !IsSafeTypeChange(c.Old.Type, c.New.Type) {
			imp.Level = ImpactBlocked
			imp.Message = fmt.Sprintf("%s %s a value for %q. %s cannot be converted to %s; remove the field and add a new one instead.",
				entries(imp.Affected), hasHave(imp.Affected), c.Field, c.Old.Type.Label(), c.New.Type.Label())
			break
		}
		var bad int
		bad, err = s.countInvalid(ctx, ct, c.Field, c.New)
		if bad > 0 {
			imp.Level = ImpactBlocked
			imp.Affected = bad
			imp.Message = fmt.Sprintf("%d existing values of %q are not valid %s values. Fix them before changing the type.", bad, c.Field, c.New.Type.Label())
		}
	case FieldNowRequired:
		var with, total int
		if with, err = s.countWithValue(ctx, ct, c.New); err != nil {
			break
		}
		if total, err = s.countEntries(ctx, ct.ID, ""); err != nil {
			break
		}
		if missing := total - with; missing > 0 {
			imp.Level = ImpactWarning
			imp.Affected = missing
			imp.Message = fmt.Sprintf("%s %s no value for %q and must be completed the next time they are published.", entries(missing), hasHave(missing), c.Field)
		}
	case FieldNowUnique:
		err = s.pool.QueryRow(ctx, `SELECT count(*) FROM (
			SELECT data->>$2 FROM entries WHERE content_type_id = $1 AND data ? $2 GROUP BY 1 HAVING count(*) > 1) d`,
			ct.ID, c.Field).Scan(&imp.Affected)
		if imp.Affected > 0 {
			imp.Level = ImpactBlocked
			imp.Message = fmt.Sprintf("%q has %d duplicated values. Make them unique before adding the constraint.", c.Field, imp.Affected)
		}
	case EnumValuesRemoved:
		imp.Affected, err = s.countEntries(ctx, ct.ID, `data->>$2 = ANY($3)`, c.Field, c.Removed)
		if imp.Affected > 0 {
			imp.Level = ImpactDestructive
			imp.Message = fmt.Sprintf("%s %s a removed value of %q. Those values will be cleared.", entries(imp.Affected), map[bool]string{true: "uses", false: "use"}[imp.Affected == 1], c.Field)
		}
	case RelationChanged:
		imp.Affected, err = s.countWithValue(ctx, ct, c.Old)
		if imp.Affected > 0 {
			imp.Level = ImpactDestructive
			imp.Message = fmt.Sprintf("%s %s links in %q. Changing its target or cardinality removes those links.", entries(imp.Affected), hasHave(imp.Affected), c.Field)
		}
	case RulesChanged:
		imp.Affected, err = s.countInvalid(ctx, ct, c.Field, c.New)
		if imp.Affected > 0 {
			imp.Level = ImpactWarning
			imp.Message = fmt.Sprintf("%d values of %q do not satisfy the new rules and must be fixed the next time they are saved.", imp.Affected, c.Field)
		}
	case KindChanged:
		imp.Affected, err = s.countEntries(ctx, ct.ID, "")
		if ct.Kind == KindCollection && imp.Affected > 1 {
			imp.Level = ImpactBlocked
			imp.Message = fmt.Sprintf("A single type holds one entry, but this collection has %d.", imp.Affected)
		}
	case DraftPublishOff:
		imp.Affected, err = s.countEntries(ctx, ct.ID, "published_at IS NULL")
		if imp.Affected > 0 {
			imp.Level = ImpactWarning
			imp.Message = fmt.Sprintf("%s in draft will be published.", entries(imp.Affected))
		}
	}
	return imp, err
}

// Update replaces the definition of an existing content type. The uid cannot change.
// If the change affects stored entries, a *PlanError is returned unless confirm is true;
// blocked plans are never applied.
func (s *Service) Update(ctx context.Context, siteID int64, uid string, next *ContentType, confirm bool, userID int64) (*ContentType, *Plan, error) {
	current, err := s.Get(ctx, siteID, uid)
	if err != nil {
		return nil, nil, err
	}
	next.ID = current.ID
	next.SiteID = current.SiteID
	next.UID = current.UID
	next.Normalize()
	lookup, all, err := s.lookup(ctx, siteID)
	if err != nil {
		return nil, nil, err
	}
	if err := next.Validate(lookup); err != nil {
		return nil, nil, err
	}
	if err := checkNames(next, all); err != nil {
		return nil, nil, err
	}
	if next.Kind == KindSingle {
		for _, o := range all {
			for _, f := range o.Fields {
				if f.Type == TypeRelation && f.Target == current.UID && o.ID != current.ID {
					return nil, nil, &ValidationError{Message: "Invalid content type", Errors: []FieldError{{
						Path: []string{"kind"}, Message: fmt.Sprintf("%s has relations to this type; single types cannot be relation targets", o.DisplayName)}}}
				}
			}
		}
	}
	plan, err := s.Plan(ctx, current, next)
	if err != nil {
		return nil, nil, err
	}
	if plan.Blocked() || (plan.NeedsConfirmation() && !confirm) {
		return nil, plan, &PlanError{Plan: plan}
	}

	fields, _ := json.Marshal(next.Fields)
	var updated *ContentType
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Serialize concurrent schema updates of the same type.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM content_types WHERE id = $1 FOR UPDATE`, current.ID); err != nil {
			return err
		}
		for _, imp := range plan.Impacts {
			if err := applyChange(ctx, tx, current, imp.Change); err != nil {
				return err
			}
		}
		var err error
		updated, err = scanType(tx.QueryRow(ctx, `UPDATE content_types SET
			plural_name = $2, kind = $3, display_name = $4, description = $5, draft_publish = $6,
			fields = $7, version = version + 1, updated_at = now()
			WHERE id = $1 RETURNING `+selectCols,
			current.ID, next.PluralName, next.Kind, next.DisplayName, next.Description, next.DraftPublish, fields))
		if err != nil {
			return err
		}
		return recordHistory(ctx, tx, updated, userID)
	})
	if err != nil {
		if _, ok := db.IsUniqueViolation(err); ok {
			return nil, plan, &ValidationError{Message: "Invalid content type", Errors: []FieldError{{Path: []string{"pluralName"}, Message: "Already in use"}}}
		}
		return nil, plan, err
	}
	return updated, plan, nil
}

func applyChange(ctx context.Context, tx pgx.Tx, ct *ContentType, c Change) error {
	purge := func(f *Field) error {
		var err error
		switch f.Type {
		case TypeRelation:
			_, err = tx.Exec(ctx, `DELETE FROM relations r USING entries e WHERE r.source_id = e.id AND e.content_type_id = $1 AND r.field = $2`, ct.ID, f.Name)
		case TypeMedia:
			_, err = tx.Exec(ctx, `DELETE FROM media_links m USING entries e WHERE m.entry_id = e.id AND e.content_type_id = $1 AND m.field = $2`, ct.ID, f.Name)
		default:
			_, err = tx.Exec(ctx, `UPDATE entries SET data = data - $2 WHERE content_type_id = $1 AND data ? $2`, ct.ID, f.Name)
		}
		return err
	}
	switch c.Kind {
	case FieldRemoved:
		if c.Old.Unique {
			if err := dropUniqueIndex(ctx, tx, ct, c.Field); err != nil {
				return err
			}
		}
		return purge(c.Old)
	case FieldTypeChanged:
		if c.Old.Unique && !c.New.Unique {
			if err := dropUniqueIndex(ctx, tx, ct, c.Field); err != nil {
				return err
			}
		}
		if c.New.Unique && !c.Old.Unique {
			return createUniqueIndex(ctx, tx, ct, c.Field)
		}
		// Only empty fields or safe conversions reach this point; link data of an empty field is a no-op.
		return nil
	case FieldNowUnique:
		return createUniqueIndex(ctx, tx, ct, c.Field)
	case FieldNotUnique:
		return dropUniqueIndex(ctx, tx, ct, c.Field)
	case EnumValuesRemoved:
		_, err := tx.Exec(ctx, `UPDATE entries SET data = data - $2 WHERE content_type_id = $1 AND data->>$2 = ANY($3)`, ct.ID, c.Field, c.Removed)
		return err
	case RelationChanged:
		return purge(c.Old)
	case DraftPublishOff:
		_, err := tx.Exec(ctx, `UPDATE entries SET published_at = now() WHERE content_type_id = $1 AND published_at IS NULL`, ct.ID)
		return err
	}
	return nil
}

func createUniqueIndex(ctx context.Context, tx pgx.Tx, ct *ContentType, field string) error {
	// Field names are validated against ^[a-zA-Z][a-zA-Z0-9_]*$ and ids are integers, so the
	// interpolated identifiers below cannot inject SQL. DDL does not accept bind parameters.
	if !fieldNamePattern.MatchString(field) {
		return fmt.Errorf("invalid field name %q", field)
	}
	_, err := tx.Exec(ctx, fmt.Sprintf(`CREATE UNIQUE INDEX %s ON entries ((data->>'%s')) WHERE content_type_id = %d`,
		pgx.Identifier{ct.UniqueIndexName(field)}.Sanitize(), field, ct.ID))
	return err
}

func dropUniqueIndex(ctx context.Context, tx pgx.Tx, ct *ContentType, field string) error {
	_, err := tx.Exec(ctx, `DROP INDEX IF EXISTS `+pgx.Identifier{ct.UniqueIndexName(field)}.Sanitize())
	return err
}

func recordHistory(ctx context.Context, tx pgx.Tx, ct *ContentType, userID int64) error {
	def, _ := json.Marshal(ct)
	var uid *int64
	if userID > 0 {
		uid = &userID
	}
	_, err := tx.Exec(ctx, `INSERT INTO content_type_history (content_type_id, version, definition, changed_by) VALUES ($1, $2, $3, $4)`,
		ct.ID, ct.Version, def, uid)
	return err
}

// DeleteImpact returns the number of entries that deleting the type would remove and the
// display names of content types whose relations target it (which block deletion).
func (s *Service) DeleteImpact(ctx context.Context, siteID int64, uid string) (entries int, referencedBy []string, err error) {
	ct, err := s.Get(ctx, siteID, uid)
	if err != nil {
		return 0, nil, err
	}
	all, err := s.List(ctx, siteID)
	if err != nil {
		return 0, nil, err
	}
	for _, o := range all {
		if o.ID == ct.ID {
			continue
		}
		for _, f := range o.Fields {
			if f.Type == TypeRelation && f.Target == uid {
				referencedBy = append(referencedBy, o.DisplayName+"."+f.Name)
			}
		}
	}
	entries, err = s.countEntries(ctx, ct.ID, "")
	return entries, referencedBy, err
}

// Delete removes a content type and all of its entries. It refuses while other types relate to it.
func (s *Service) Delete(ctx context.Context, siteID int64, uid string) error {
	ct, err := s.Get(ctx, siteID, uid)
	if err != nil {
		return err
	}
	_, refs, err := s.DeleteImpact(ctx, siteID, uid)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return &ValidationError{Message: "Content type is referenced by relations: " + strings.Join(refs, ", ")}
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, f := range ct.Fields {
			if f.Unique {
				if err := dropUniqueIndex(ctx, tx, ct, f.Name); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM role_permissions WHERE subject = $1`, PermissionSubject(ct.ID)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM public_permissions WHERE site_id = $1 AND subject = $2`, siteID, uid); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM content_types WHERE id = $1`, ct.ID)
		return err
	})
}

// Count returns the number of entries per content type id of a site.
func (s *Service) Count(ctx context.Context, siteID int64) (map[int64]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT e.content_type_id, count(*) FROM entries e
		JOIN content_types ct ON ct.id = e.content_type_id WHERE ct.site_id = $1 GROUP BY 1`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
