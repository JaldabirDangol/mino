// Package schema defines content types and their fields, validates schema definitions,
// computes the impact of schema changes, and coerces field values.
//
// It has no database or HTTP dependencies; the Service in service.go persists definitions.
package schema

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Kind distinguishes collections (many entries) from singletons.
type Kind string

const (
	KindCollection Kind = "collection"
	KindSingle     Kind = "single"
)

// FieldType is the data type of a field.
type FieldType string

const (
	TypeString   FieldType = "string"   // Short Text
	TypeText     FieldType = "text"     // Long Text
	TypeRichText FieldType = "richtext" // Sanitized HTML
	TypeNumber   FieldType = "number"
	TypeBoolean  FieldType = "boolean"
	TypeDate     FieldType = "date"     // YYYY-MM-DD
	TypeDateTime FieldType = "datetime" // RFC 3339, stored in UTC
	TypeEmail    FieldType = "email"
	TypeEnum     FieldType = "enumeration"
	TypeJSON     FieldType = "json"
	TypeMedia    FieldType = "media"
	TypeRelation FieldType = "relation"
)

// FieldTypes lists every supported type in the order the builder shows them.
var FieldTypes = []FieldType{
	TypeString, TypeText, TypeRichText, TypeNumber, TypeBoolean, TypeDate, TypeDateTime,
	TypeEmail, TypeEnum, TypeJSON, TypeMedia, TypeRelation,
}

// FieldTypeInfo is display metadata for the Content-Type Builder.
type FieldTypeInfo struct {
	Type        FieldType
	Label       string
	Description string
}

// TypeInfo returns display metadata for every field type.
func TypeInfo() []FieldTypeInfo {
	return []FieldTypeInfo{
		{TypeString, "Short Text", "Titles, names, slugs"},
		{TypeText, "Long Text", "Descriptions, plain paragraphs"},
		{TypeRichText, "Rich Text", "Formatted content with headings, links, lists"},
		{TypeNumber, "Number", "Integers or decimals"},
		{TypeBoolean, "Boolean", "Yes or no, on or off"},
		{TypeDate, "Date", "A calendar date"},
		{TypeDateTime, "Date & Time", "A date with a time"},
		{TypeEmail, "Email", "Validated email address"},
		{TypeEnum, "Enumeration", "One value from a fixed list"},
		{TypeJSON, "JSON", "Arbitrary JSON data"},
		{TypeMedia, "Media", "Images, videos or files"},
		{TypeRelation, "Relation", "Link to entries of another type"},
	}
}

// Label returns the human-readable name of a field type.
func (t FieldType) Label() string {
	for _, i := range TypeInfo() {
		if i.Type == t {
			return i.Label
		}
	}
	return string(t)
}

// RelationKind describes the cardinality of a relation, seen from the owning type.
type RelationKind string

const (
	OneToOne   RelationKind = "oneToOne"   // this entry has one target; a target belongs to one entry
	OneToMany  RelationKind = "oneToMany"  // this entry has many targets; a target belongs to one entry
	ManyToOne  RelationKind = "manyToOne"  // this entry has one target; a target can be shared
	ManyToMany RelationKind = "manyToMany" // many targets, freely shared
)

// RelationKinds lists the supported relation kinds.
var RelationKinds = []RelationKind{OneToOne, OneToMany, ManyToOne, ManyToMany}

// Multiple reports whether the owning entry may link to more than one target.
func (k RelationKind) Multiple() bool { return k == OneToMany || k == ManyToMany }

// ExclusiveTarget reports whether a target may be linked from at most one source entry.
func (k RelationKind) ExclusiveTarget() bool { return k == OneToOne || k == OneToMany }

// Media categories accepted by Field.AllowedTypes.
var MediaCategories = []string{"images", "videos", "audios", "files"}

// Field is one attribute of a content type.
type Field struct {
	Name        string    `json:"name"`
	Label       string    `json:"label,omitempty"`
	Type        FieldType `json:"type"`
	Description string    `json:"description,omitempty"`
	Required    bool      `json:"required,omitempty"`
	Unique      bool      `json:"unique,omitempty"`
	Private     bool      `json:"private,omitempty"`  // hidden from the REST API
	ReadOnly    bool      `json:"readOnly,omitempty"` // not editable in the admin form
	Default     any       `json:"default,omitempty"`

	// Validation rules.
	MinLength *int     `json:"minLength,omitempty"`
	MaxLength *int     `json:"maxLength,omitempty"`
	Min       *float64 `json:"min,omitempty"`
	Max       *float64 `json:"max,omitempty"`
	Regex     string   `json:"regex,omitempty"`

	// Type-specific options.
	Format       string       `json:"format,omitempty"`       // number: "integer" | "decimal"
	Enum         []string     `json:"enum,omitempty"`         // enumeration values
	Multiple     bool         `json:"multiple,omitempty"`     // media: allow several files
	AllowedTypes []string     `json:"allowedTypes,omitempty"` // media: subset of MediaCategories
	Relation     RelationKind `json:"relation,omitempty"`     // relation kind
	Target       string       `json:"target,omitempty"`       // relation target content type uid
}

// DisplayLabel returns the label or, if empty, the field name.
func (f Field) DisplayLabel() string {
	if f.Label != "" {
		return f.Label
	}
	return f.Name
}

// IsLink reports whether values live in link tables rather than in the JSONB document.
func (f Field) IsLink() bool { return f.Type == TypeMedia || f.Type == TypeRelation }

// IsMultiple reports whether a link field holds several values.
func (f Field) IsMultiple() bool {
	switch f.Type {
	case TypeMedia:
		return f.Multiple
	case TypeRelation:
		return f.Relation.Multiple()
	}
	return false
}

// IsTextual reports whether the field holds free text (used for admin search).
func (f Field) IsTextual() bool {
	switch f.Type {
	case TypeString, TypeText, TypeEmail, TypeRichText, TypeEnum:
		return true
	}
	return false
}

// Sortable reports whether entries can be ordered by this field.
func (f Field) Sortable() bool {
	switch f.Type {
	case TypeString, TypeText, TypeNumber, TypeBoolean, TypeDate, TypeDateTime, TypeEmail, TypeEnum:
		return true
	}
	return false
}

// ContentType is a user-defined schema.
type ContentType struct {
	ID           int64     `json:"id"`
	SiteID       int64     `json:"siteId"`
	UID          string    `json:"uid"`        // singular API id, e.g. "article"
	PluralName   string    `json:"pluralName"` // collection route, e.g. "articles"
	Kind         Kind      `json:"kind"`
	DisplayName  string    `json:"displayName"`
	Description  string    `json:"description,omitempty"`
	DraftPublish bool      `json:"draftAndPublish"`
	Fields       []Field   `json:"fields"`
	Version      int       `json:"version"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Field returns the field with the given name.
func (ct *ContentType) Field(name string) (Field, bool) {
	for _, f := range ct.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// RouteName is the REST path segment: the plural name for collections, the uid for single types.
func (ct *ContentType) RouteName() string {
	if ct.Kind == KindSingle {
		return ct.UID
	}
	return ct.PluralName
}

// PermissionSubject is the role permission subject that targets one content type.
func PermissionSubject(contentTypeID int64) string { return fmt.Sprintf("ct:%d", contentTypeID) }

// UniqueIndexName is the name of the partial unique index enforcing a unique field.
func (ct *ContentType) UniqueIndexName(field string) string {
	return fmt.Sprintf("entries_ct%d_%s_uniq", ct.ID, strings.ToLower(field))
}

// TitleField picks the field used to label entries in lists and relation pickers.
func (ct *ContentType) TitleField() string {
	for _, pref := range []string{"title", "name", "label", "slug"} {
		if f, ok := ct.Field(pref); ok && f.Type == TypeString {
			return f.Name
		}
	}
	for _, f := range ct.Fields {
		if f.Type == TypeString || f.Type == TypeEmail {
			return f.Name
		}
	}
	return ""
}

// SystemFields are attributes every entry has; they cannot be used as field names.
var SystemFields = []string{"id", "createdAt", "updatedAt", "publishedAt", "createdBy", "updatedBy"}

// reservedFieldNames would collide with keys the engine adds to every entry in responses.
var reservedFieldNames = append(slices.Clone(SystemFields), "documentId", "locale")

var reservedRouteNames = []string{"upload", "uploads", "admin", "auth", "users", "roles", "permissions", "tokens", "settings", "health", "_health", "content-types", "api", "static", "connect"}

var (
	apiIDPattern     = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)
	fieldNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
)

// FieldError is a validation error at a path such as ["fields", "2", "name"].
type FieldError struct {
	Path    []string `json:"path"`
	Message string   `json:"message"`
}

// ValidationError aggregates one or more field errors.
type ValidationError struct {
	Message string       `json:"message"`
	Errors  []FieldError `json:"errors"`
}

func (e *ValidationError) Error() string {
	if len(e.Errors) == 0 {
		return e.Message
	}
	if len(e.Errors) == 1 {
		return strings.Join(e.Errors[0].Path, ".") + ": " + e.Errors[0].Message
	}
	return fmt.Sprintf("%s (%d errors)", e.Message, len(e.Errors))
}

// Add records an error at path.
func (e *ValidationError) Add(msg string, path ...string) {
	e.Errors = append(e.Errors, FieldError{Path: path, Message: msg})
}

// For returns the first message whose path starts with the given segments.
func (e *ValidationError) For(path ...string) string {
	if e == nil {
		return ""
	}
outer:
	for _, fe := range e.Errors {
		if len(fe.Path) < len(path) {
			continue
		}
		for i := range path {
			if fe.Path[i] != path[i] {
				continue outer
			}
		}
		return fe.Message
	}
	return ""
}

// OrNil returns nil when there are no errors.
func (e *ValidationError) OrNil() error {
	if len(e.Errors) == 0 {
		return nil
	}
	return e
}

// Lookup resolves other content types by uid (needed to validate relation targets).
type Lookup func(uid string) (*ContentType, bool)

// Normalize trims and fills defaults in place so equivalent definitions compare equal.
func (ct *ContentType) Normalize() {
	ct.UID = strings.TrimSpace(ct.UID)
	ct.PluralName = strings.TrimSpace(ct.PluralName)
	ct.DisplayName = strings.TrimSpace(ct.DisplayName)
	ct.Description = strings.TrimSpace(ct.Description)
	if ct.Fields == nil {
		ct.Fields = []Field{}
	}
	for i := range ct.Fields {
		f := &ct.Fields[i]
		f.Name = strings.TrimSpace(f.Name)
		f.Label = strings.TrimSpace(f.Label)
		f.Description = strings.TrimSpace(f.Description)
		if f.Default == "" {
			f.Default = nil
		}
		if f.Type == TypeNumber && f.Format == "" {
			f.Format = "decimal"
		}
		if f.Type != TypeNumber {
			f.Format = ""
		}
		if f.Type != TypeEnum {
			f.Enum = nil
		} else {
			vals := f.Enum[:0]
			for _, v := range f.Enum {
				if v = strings.TrimSpace(v); v != "" {
					vals = append(vals, v)
				}
			}
			f.Enum = vals
		}
		if f.Type != TypeMedia {
			f.Multiple = false
			f.AllowedTypes = nil
		}
		if f.Type != TypeRelation {
			f.Relation = ""
			f.Target = ""
		}
		if f.IsLink() {
			// Defaults, uniqueness and length rules do not apply to links.
			f.Default = nil
			f.Unique = false
		}
		if !hasLength(f.Type) {
			f.MinLength, f.MaxLength = nil, nil
		}
		if f.Type != TypeString {
			f.Regex = ""
		}
		if f.Type != TypeNumber {
			f.Min, f.Max = nil, nil
		}
	}
}

func hasLength(t FieldType) bool { return t == TypeString || t == TypeText || t == TypeEmail }

// CanBeUnique reports whether a unique constraint is supported for the type.
func CanBeUnique(t FieldType) bool {
	switch t {
	case TypeString, TypeEmail, TypeNumber, TypeDate, TypeDateTime, TypeText:
		return true
	}
	return false
}

// Validate checks a definition. lookup resolves relation targets; the content type itself
// is always resolvable so self-relations work even before it is first saved.
func (ct *ContentType) Validate(lookup Lookup) error {
	ve := &ValidationError{Message: "Invalid content type"}

	if ct.DisplayName == "" {
		ve.Add("Display name is required", "displayName")
	} else if len(ct.DisplayName) > 80 {
		ve.Add("Display name must be at most 80 characters", "displayName")
	}
	checkAPIID := func(name, path string) {
		switch {
		case name == "":
			ve.Add("Required", path)
		case len(name) > 60:
			ve.Add("Must be at most 60 characters", path)
		case !apiIDPattern.MatchString(name):
			ve.Add("Use lowercase letters, digits and dashes, starting with a letter (e.g. blog-post)", path)
		case slices.Contains(reservedRouteNames, name):
			ve.Add(fmt.Sprintf("%q is reserved", name), path)
		}
	}
	checkAPIID(ct.UID, "uid")
	checkAPIID(ct.PluralName, "pluralName")
	if ct.UID != "" && ct.UID == ct.PluralName {
		ve.Add("Singular and plural names must differ", "pluralName")
	}
	if ct.Kind != KindCollection && ct.Kind != KindSingle {
		ve.Add("Kind must be collection or single", "kind")
	}

	seen := map[string]bool{}
	for i, f := range ct.Fields {
		p := func(s string) []string { return []string{"fields", fmt.Sprint(i), s} }
		lname := strings.ToLower(f.Name)
		switch {
		case f.Name == "":
			ve.Add("Name is required", p("name")...)
		case len(f.Name) > 60:
			ve.Add("Name must be at most 60 characters", p("name")...)
		case !fieldNamePattern.MatchString(f.Name):
			ve.Add("Use letters, digits and underscores, starting with a letter", p("name")...)
		case slices.ContainsFunc(reservedFieldNames, func(r string) bool { return strings.EqualFold(r, f.Name) }):
			ve.Add(fmt.Sprintf("%q is reserved", f.Name), p("name")...)
		case seen[lname]:
			ve.Add("Duplicate field name", p("name")...)
		}
		seen[lname] = true

		if !slices.Contains(FieldTypes, f.Type) {
			ve.Add("Unknown field type", p("type")...)
			continue
		}
		if f.Unique && !CanBeUnique(f.Type) {
			ve.Add(fmt.Sprintf("%s fields cannot be unique", f.Type.Label()), p("unique")...)
		}
		if f.MinLength != nil && *f.MinLength < 0 {
			ve.Add("Must be zero or more", p("minLength")...)
		}
		if f.MaxLength != nil && *f.MaxLength < 1 {
			ve.Add("Must be at least 1", p("maxLength")...)
		}
		if f.MinLength != nil && f.MaxLength != nil && *f.MinLength > *f.MaxLength {
			ve.Add("Minimum length exceeds maximum length", p("minLength")...)
		}
		if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
			ve.Add("Minimum exceeds maximum", p("min")...)
		}
		if f.Regex != "" {
			if _, err := regexp.Compile(f.Regex); err != nil {
				ve.Add("Invalid regular expression", p("regex")...)
			}
		}
		switch f.Type {
		case TypeNumber:
			if f.Format != "integer" && f.Format != "decimal" {
				ve.Add("Format must be integer or decimal", p("format")...)
			}
		case TypeEnum:
			if len(f.Enum) == 0 {
				ve.Add("Add at least one value", p("enum")...)
			}
			vals := map[string]bool{}
			for _, v := range f.Enum {
				if vals[v] {
					ve.Add(fmt.Sprintf("Duplicate value %q", v), p("enum")...)
				}
				vals[v] = true
			}
		case TypeMedia:
			for _, t := range f.AllowedTypes {
				if !slices.Contains(MediaCategories, t) {
					ve.Add(fmt.Sprintf("Unknown media type %q", t), p("allowedTypes")...)
				}
			}
		case TypeRelation:
			if !slices.Contains(RelationKinds, f.Relation) {
				ve.Add("Choose a relation type", p("relation")...)
			}
			if f.Target == "" {
				ve.Add("Choose a target content type", p("target")...)
			} else if f.Target != ct.UID {
				target, ok := lookup(f.Target)
				if !ok {
					ve.Add("Target content type does not exist", p("target")...)
				} else if target.Kind == KindSingle {
					ve.Add("Relations can only target collection types", p("target")...)
				}
			} else if ct.Kind == KindSingle {
				ve.Add("Relations can only target collection types", p("target")...)
			}
		}
		if f.Default != nil {
			if _, err := CoerceValue(f, f.Default); err != nil {
				ve.Add("Default value: "+err.Error(), p("default")...)
			}
		}
	}
	return ve.OrNil()
}
