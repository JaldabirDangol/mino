// Package content is the schema-driven content engine. It validates, stores, queries and
// serializes entries of any content type. Both the admin panel and the REST API use it.
package content

import (
	"errors"
	"slices"
	"strconv"
	"time"

	"mino/internal/media"
	"mino/internal/schema"
)

var (
	// ErrNotFound is returned when an entry does not exist (or is not visible in the requested status).
	ErrNotFound = errors.New("entry not found")
	// ErrSingleExists is returned when creating a second entry for a single type.
	ErrSingleExists = errors.New("single type already has an entry")
)

// Entry is one stored record.
type Entry struct {
	ID          int64
	Type        *schema.ContentType
	Data        map[string]any // scalar field values; absent key = no value
	PublishedAt *time.Time
	CreatedBy   *int64
	UpdatedBy   *int64
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// Links holds related entry ids or media ids per link field, in order. Only loaded fields are present.
	Links map[string][]int64
	// Relations and Media hold populated link targets.
	Relations map[string][]*Entry
	Media     map[string][]*media.File
	populated map[string]bool

	contentTypeID int64
}

// Published reports whether the entry is live.
func (e *Entry) Published() bool { return e.PublishedAt != nil }

// Title returns a display label for admin lists and relation pickers.
func (e *Entry) Title() string {
	if e.Type != nil {
		if f := e.Type.TitleField(); f != "" {
			if s, ok := e.Data[f].(string); ok && s != "" {
				return s
			}
		}
	}
	return "#" + strconv.FormatInt(e.ID, 10)
}

// Populated reports whether a link field's targets were loaded.
func (e *Entry) Populated(field string) bool { return e.populated[field] }

// SerializeOptions controls the JSON shape of an entry.
type SerializeOptions struct {
	Fields         []string // restrict scalar fields; nil = all
	IncludePrivate bool     // admin preview shows private fields
}

// Serialize returns the public JSON representation of an entry. Unpopulated link fields are
// omitted; populated single links are an object or null, multiple links an array.
func Serialize(e *Entry, opts SerializeOptions) map[string]any {
	out := map[string]any{
		"id":          e.ID,
		"createdAt":   e.CreatedAt,
		"updatedAt":   e.UpdatedAt,
		"publishedAt": e.PublishedAt,
	}
	if e.Type == nil {
		return out
	}
	for _, f := range e.Type.Fields {
		if f.Private && !opts.IncludePrivate {
			continue
		}
		if f.IsLink() {
			if !e.populated[f.Name] {
				continue
			}
			var items []any
			if f.Type == schema.TypeMedia {
				for _, m := range e.Media[f.Name] {
					items = append(items, m.JSON())
				}
			} else {
				for _, r := range e.Relations[f.Name] {
					items = append(items, Serialize(r, SerializeOptions{IncludePrivate: opts.IncludePrivate}))
				}
			}
			if f.IsMultiple() {
				if items == nil {
					items = []any{}
				}
				out[f.Name] = items
			} else if len(items) > 0 {
				out[f.Name] = items[0]
			} else {
				out[f.Name] = nil
			}
			continue
		}
		if opts.Fields != nil && !slices.Contains(opts.Fields, f.Name) {
			continue
		}
		out[f.Name] = e.Data[f.Name] // nil when absent, so every field is always present
	}
	if opts.Fields != nil {
		for _, sys := range []string{"createdAt", "updatedAt", "publishedAt"} {
			if !slices.Contains(opts.Fields, sys) {
				delete(out, sys)
			}
		}
	}
	return out
}
