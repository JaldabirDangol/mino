package schema

import (
	"fmt"
	"reflect"
	"slices"
)

// ChangeKind classifies a difference between two versions of a content type.
type ChangeKind string

const (
	FieldAdded        ChangeKind = "field_added"
	FieldRemoved      ChangeKind = "field_removed"
	FieldTypeChanged  ChangeKind = "type_changed"
	FieldNowRequired  ChangeKind = "now_required"
	FieldNowUnique    ChangeKind = "now_unique"
	FieldNotUnique    ChangeKind = "not_unique"
	EnumValuesRemoved ChangeKind = "enum_values_removed"
	RelationChanged   ChangeKind = "relation_changed"
	RulesChanged      ChangeKind = "rules_changed"
	KindChanged       ChangeKind = "kind_changed"
	DraftPublishOff   ChangeKind = "draft_publish_off"
)

// Change is one schema difference. Old and New are the field definitions where relevant.
type Change struct {
	Kind    ChangeKind
	Field   string
	Old     *Field
	New     *Field
	Removed []string // enum values that disappeared
}

// safeTypeChanges lists conversions where every existing stored value stays valid
// (subject to the new field's rules, which the service re-checks against data).
var safeTypeChanges = map[[2]FieldType]bool{
	{TypeString, TypeText}:  true,
	{TypeText, TypeString}:  true,
	{TypeEmail, TypeString}: true,
	{TypeEmail, TypeText}:   true,
	{TypeString, TypeEmail}: true,
	{TypeEnum, TypeString}:  true,
	{TypeEnum, TypeText}:    true,
	{TypeString, TypeEnum}:  true,
	{TypeDate, TypeString}:  true,
}

// IsSafeTypeChange reports whether stored values of type from can be reinterpreted as type to.
func IsSafeTypeChange(from, to FieldType) bool { return safeTypeChanges[[2]FieldType{from, to}] }

// Diff lists the changes needed to go from old to new. Fields are matched by name.
func Diff(old, new *ContentType) []Change {
	var out []Change
	if old.Kind != new.Kind {
		out = append(out, Change{Kind: KindChanged})
	}
	if old.DraftPublish && !new.DraftPublish {
		out = append(out, Change{Kind: DraftPublishOff})
	}
	for i := range old.Fields {
		of := &old.Fields[i]
		nf, ok := new.Field(of.Name)
		if !ok {
			out = append(out, Change{Kind: FieldRemoved, Field: of.Name, Old: of})
			continue
		}
		nfp := &nf
		if of.Type != nf.Type {
			out = append(out, Change{Kind: FieldTypeChanged, Field: of.Name, Old: of, New: nfp})
			continue
		}
		if nf.Required && !of.Required {
			out = append(out, Change{Kind: FieldNowRequired, Field: nf.Name, Old: of, New: nfp})
		}
		if nf.Unique && !of.Unique {
			out = append(out, Change{Kind: FieldNowUnique, Field: nf.Name, Old: of, New: nfp})
		}
		if !nf.Unique && of.Unique {
			out = append(out, Change{Kind: FieldNotUnique, Field: nf.Name, Old: of, New: nfp})
		}
		if nf.Type == TypeEnum {
			var removed []string
			for _, v := range of.Enum {
				if !slices.Contains(nf.Enum, v) {
					removed = append(removed, v)
				}
			}
			if len(removed) > 0 {
				out = append(out, Change{Kind: EnumValuesRemoved, Field: nf.Name, Old: of, New: nfp, Removed: removed})
			}
		}
		if nf.Type == TypeRelation && (nf.Target != of.Target || nf.Relation != of.Relation) {
			out = append(out, Change{Kind: RelationChanged, Field: nf.Name, Old: of, New: nfp})
		}
		if nf.Type == TypeMedia && of.Multiple && !nf.Multiple {
			out = append(out, Change{Kind: RelationChanged, Field: nf.Name, Old: of, New: nfp})
		}
		if rulesTightened(of, nfp) {
			out = append(out, Change{Kind: RulesChanged, Field: nf.Name, Old: of, New: nfp})
		}
	}
	for i := range new.Fields {
		nf := &new.Fields[i]
		if _, ok := old.Field(nf.Name); !ok {
			out = append(out, Change{Kind: FieldAdded, Field: nf.Name, New: nf})
		}
	}
	return out
}

// rulesTightened reports whether validation rules changed in a way that may invalidate data.
func rulesTightened(o, n *Field) bool {
	return !reflect.DeepEqual(o.MinLength, n.MinLength) && n.MinLength != nil ||
		!reflect.DeepEqual(o.MaxLength, n.MaxLength) && n.MaxLength != nil ||
		!reflect.DeepEqual(o.Min, n.Min) && n.Min != nil ||
		!reflect.DeepEqual(o.Max, n.Max) && n.Max != nil ||
		o.Regex != n.Regex && n.Regex != "" ||
		o.Format != n.Format && n.Format == "integer"
}

// Describe returns a short human-readable summary of a change.
func (c Change) Describe() string {
	switch c.Kind {
	case FieldAdded:
		return fmt.Sprintf("Add field %q", c.Field)
	case FieldRemoved:
		return fmt.Sprintf("Remove field %q", c.Field)
	case FieldTypeChanged:
		return fmt.Sprintf("Change %q from %s to %s", c.Field, c.Old.Type.Label(), c.New.Type.Label())
	case FieldNowRequired:
		return fmt.Sprintf("Make %q required", c.Field)
	case FieldNowUnique:
		return fmt.Sprintf("Make %q unique", c.Field)
	case FieldNotUnique:
		return fmt.Sprintf("Drop unique constraint on %q", c.Field)
	case EnumValuesRemoved:
		return fmt.Sprintf("Remove values %v from %q", c.Removed, c.Field)
	case RelationChanged:
		return fmt.Sprintf("Change link settings of %q", c.Field)
	case RulesChanged:
		return fmt.Sprintf("Tighten validation rules of %q", c.Field)
	case KindChanged:
		return "Change content type kind"
	case DraftPublishOff:
		return "Disable draft & publish (all entries become published)"
	}
	return string(c.Kind)
}
