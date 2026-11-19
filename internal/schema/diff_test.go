package schema

import "testing"

func kinds(cs []Change) map[ChangeKind]string {
	out := map[ChangeKind]string{}
	for _, c := range cs {
		out[c.Kind] = c.Field
	}
	return out
}

func TestDiff(t *testing.T) {
	old := article()
	old.Fields = append(old.Fields, Field{Name: "status", Type: TypeEnum, Enum: []string{"a", "b", "c"}})
	next := article()
	next.Fields = []Field{
		{Name: "title", Type: TypeText, Required: true},                      // type change
		{Name: "slug", Type: TypeString},                                     // unique dropped
		{Name: "views", Type: TypeNumber, Format: "integer", Required: true}, // now required; body removed
		{Name: "status", Type: TypeEnum, Enum: []string{"a"}, Unique: false}, // enum values removed
		{Name: "summary", Type: TypeText},                                    // added
	}
	got := kinds(Diff(old, next))
	want := map[ChangeKind]string{
		FieldTypeChanged:  "title",
		FieldNotUnique:    "slug",
		FieldRemoved:      "body",
		FieldNowRequired:  "views",
		EnumValuesRemoved: "status",
		FieldAdded:        "summary",
	}
	for k, f := range want {
		if got[k] != f {
			t.Errorf("change %s: got field %q, want %q (all: %v)", k, got[k], f, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected extra changes: %v", got)
	}
}

func TestDiffNoChanges(t *testing.T) {
	if cs := Diff(article(), article()); len(cs) != 0 {
		t.Fatalf("identical schemas produced changes: %v", cs)
	}
}

func TestDiffRulesTightened(t *testing.T) {
	old, next := article(), article()
	next.Fields[0].MaxLength = ptr(10)
	if got := kinds(Diff(old, next)); got[RulesChanged] != "title" {
		t.Fatalf("max length not detected: %v", got)
	}
	// Loosening rules is not reported.
	old.Fields[0].MaxLength = ptr(10)
	next.Fields[0].MaxLength = nil
	if got := kinds(Diff(old, next)); len(got) != 0 {
		t.Fatalf("loosening reported: %v", got)
	}
}

func TestSafeTypeChanges(t *testing.T) {
	if !IsSafeTypeChange(TypeString, TypeText) || IsSafeTypeChange(TypeText, TypeNumber) {
		t.Fatal("unexpected safe type change table")
	}
}
