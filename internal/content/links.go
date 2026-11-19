package content

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"mino/internal/db"
	"mino/internal/media"
	"mino/internal/schema"
)

// setLinks replaces the links of the given fields, verifying that every target exists in the
// same site (relations: in the target content type; media: in the site's library).
func (en *Engine) setLinks(ctx context.Context, q db.DBTX, ct *schema.ContentType, entryID int64, links map[string][]int64, ve *schema.ValidationError) error {
	for _, name := range slices.Sorted(maps.Keys(links)) {
		ids := links[name]
		f, _ := ct.Field(name)
		switch f.Type {
		case schema.TypeRelation:
			if len(ids) > 0 {
				var found int
				if err := q.QueryRow(ctx, `SELECT count(*) FROM entries e JOIN content_types t ON t.id = e.content_type_id
					WHERE e.id = ANY($1) AND t.site_id = $2 AND t.uid = $3`, ids, ct.SiteID, f.Target).Scan(&found); err != nil {
					return err
				}
				if found != len(ids) {
					ve.Add(fmt.Sprintf("Some related %s entries do not exist", f.Target), name)
					continue
				}
			}
			if _, err := q.Exec(ctx, `DELETE FROM relations WHERE source_id = $1 AND field = $2`, entryID, name); err != nil {
				return err
			}
			if len(ids) == 0 {
				continue
			}
			if f.Relation.ExclusiveTarget() {
				// A target of a oneToOne/oneToMany field belongs to one source: move it here.
				if _, err := q.Exec(ctx, `DELETE FROM relations r USING entries s
					WHERE r.source_id = s.id AND s.content_type_id = $1 AND r.field = $2 AND r.target_id = ANY($3)`,
					ct.ID, name, ids); err != nil {
					return err
				}
			}
			if _, err := q.Exec(ctx, `INSERT INTO relations (source_id, field, target_id, position)
				SELECT $1, $2, t.id, t.ord FROM unnest($3::bigint[]) WITH ORDINALITY AS t(id, ord)`, entryID, name, ids); err != nil {
				return err
			}

		case schema.TypeMedia:
			if len(ids) > 0 {
				rows, err := q.Query(ctx, `SELECT id, mime FROM media WHERE id = ANY($1) AND site_id = $2`, ids, ct.SiteID)
				if err != nil {
					return err
				}
				found := 0
				var badType string
				for rows.Next() {
					var id int64
					var mime string
					if err := rows.Scan(&id, &mime); err != nil {
						rows.Close()
						return err
					}
					found++
					if len(f.AllowedTypes) > 0 && !slices.Contains(f.AllowedTypes, media.Category(mime)) {
						badType = mime
					}
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					return err
				}
				if found != len(ids) {
					ve.Add("Some media files do not exist", name)
					continue
				}
				if badType != "" {
					ve.Add(fmt.Sprintf("File type %s is not allowed here", badType), name)
					continue
				}
			}
			if _, err := q.Exec(ctx, `DELETE FROM media_links WHERE entry_id = $1 AND field = $2`, entryID, name); err != nil {
				return err
			}
			if len(ids) > 0 {
				if _, err := q.Exec(ctx, `INSERT INTO media_links (entry_id, field, media_id, position)
					SELECT $1, $2, t.id, t.ord FROM unnest($3::bigint[]) WITH ORDINALITY AS t(id, ord)`, entryID, name, ids); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// loadLinks fills Links for the given link fields and populates targets for the populate subset.
// It issues at most three queries regardless of how many entries are loaded.
func (en *Engine) loadLinks(ctx context.Context, q db.DBTX, ct *schema.ContentType, entries []*Entry, fields, populate []string, status Status) error {
	if len(entries) == 0 || len(fields) == 0 {
		return nil
	}
	byID := make(map[int64]*Entry, len(entries))
	ids := make([]int64, len(entries))
	for i, e := range entries {
		byID[e.ID] = e
		ids[i] = e.ID
		e.Links = map[string][]int64{}
		e.populated = map[string]bool{}
	}
	var relFields, mediaFields []string
	for _, name := range fields {
		f, ok := ct.Field(name)
		if !ok {
			continue
		}
		switch f.Type {
		case schema.TypeRelation:
			relFields = append(relFields, name)
		case schema.TypeMedia:
			mediaFields = append(mediaFields, name)
		}
	}

	if len(relFields) > 0 {
		rows, err := q.Query(ctx, `SELECT source_id, field, target_id FROM relations
			WHERE source_id = ANY($1) AND field = ANY($2) ORDER BY source_id, field, position`, ids, relFields)
		if err != nil {
			return err
		}
		for rows.Next() {
			var src, target int64
			var field string
			if err := rows.Scan(&src, &field, &target); err != nil {
				rows.Close()
				return err
			}
			byID[src].Links[field] = append(byID[src].Links[field], target)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}

	if len(mediaFields) > 0 {
		rows, err := q.Query(ctx, `SELECT ml.entry_id, ml.field, `+media.Columns+` FROM media_links ml
			JOIN media m ON m.id = ml.media_id
			WHERE ml.entry_id = ANY($1) AND ml.field = ANY($2) ORDER BY ml.entry_id, ml.field, ml.position`, ids, mediaFields)
		if err != nil {
			return err
		}
		for rows.Next() {
			var entryID int64
			var field string
			f, err := media.ScanPrefixed(rows, &entryID, &field)
			if err != nil {
				rows.Close()
				return err
			}
			if en.MediaURL != nil {
				f.URL = en.MediaURL(f)
			}
			e := byID[entryID]
			e.Links[field] = append(e.Links[field], f.ID)
			if slices.Contains(populate, field) {
				if e.Media == nil {
					e.Media = map[string][]*media.File{}
				}
				e.Media[field] = append(e.Media[field], f)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for _, e := range entries {
		for _, name := range populate {
			e.populated[name] = true
		}
	}

	// Populate relation targets, grouped by target content type.
	targetsByType := map[string][]int64{}
	for _, name := range populate {
		f, ok := ct.Field(name)
		if !ok || f.Type != schema.TypeRelation {
			continue
		}
		for _, e := range entries {
			targetsByType[f.Target] = append(targetsByType[f.Target], e.Links[name]...)
		}
	}
	if len(targetsByType) == 0 {
		return nil
	}
	uids := slices.Sorted(maps.Keys(targetsByType))
	types, err := en.schemas.GetMany(ctx, ct.SiteID, uids)
	if err != nil {
		return err
	}
	if slices.Contains(uids, ct.UID) {
		types[ct.UID] = ct // self-relation
	}
	var allTargets []int64
	for _, v := range targetsByType {
		allTargets = append(allTargets, v...)
	}
	where := `e.id = ANY($1)`
	if status == StatusPublished {
		where += ` AND e.published_at IS NOT NULL`
	}
	targets, err := en.queryEntries(ctx, q, nil, where, "", allTargets)
	if err != nil {
		return err
	}
	typeByID := map[int64]*schema.ContentType{}
	for _, t := range types {
		typeByID[t.ID] = t
	}
	targetByID := map[int64]*Entry{}
	for _, t := range targets {
		t.Type = typeByID[t.contentTypeID]
		targetByID[t.ID] = t
	}
	for _, name := range populate {
		f, _ := ct.Field(name)
		if f.Type != schema.TypeRelation {
			continue
		}
		for _, e := range entries {
			if e.Relations == nil {
				e.Relations = map[string][]*Entry{}
			}
			list := []*Entry{}
			for _, id := range e.Links[name] {
				if t, ok := targetByID[id]; ok && t.Type != nil {
					list = append(list, t)
				}
			}
			e.Relations[name] = list
		}
	}
	return nil
}
