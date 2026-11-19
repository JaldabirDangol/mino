package schema

import (
	"errors"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func noLookup(string) (*ContentType, bool) { return nil, false }

func article() *ContentType {
	return &ContentType{
		UID: "article", PluralName: "articles", Kind: KindCollection, DisplayName: "Article", DraftPublish: true,
		Fields: []Field{
			{Name: "title", Type: TypeString, Required: true},
			{Name: "slug", Type: TypeString, Unique: true},
			{Name: "body", Type: TypeRichText},
			{Name: "views", Type: TypeNumber, Format: "integer"},
		},
	}
}

func TestValidateAcceptsValidType(t *testing.T) {
	ct := article()
	ct.Normalize()
	if err := ct.Validate(noLookup); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ContentType)
		path   []string
	}{
		{"missing display name", func(c *ContentType) { c.DisplayName = "" }, []string{"displayName"}},
		{"bad uid", func(c *ContentType) { c.UID = "Article" }, []string{"uid"}},
		{"reserved route", func(c *ContentType) { c.PluralName = "upload" }, []string{"pluralName"}},
		{"same singular and plural", func(c *ContentType) { c.PluralName = "article" }, []string{"pluralName"}},
		{"bad kind", func(c *ContentType) { c.Kind = "thing" }, []string{"kind"}},
		{"reserved field", func(c *ContentType) { c.Fields[0].Name = "createdAt" }, []string{"fields", "0", "name"}},
		{"duplicate field", func(c *ContentType) { c.Fields[1].Name = "Title" }, []string{"fields", "1", "name"}},
		{"bad field name", func(c *ContentType) { c.Fields[0].Name = "my-title" }, []string{"fields", "0", "name"}},
		{"unknown type", func(c *ContentType) { c.Fields[0].Type = "color" }, []string{"fields", "0", "type"}},
		{"unique boolean", func(c *ContentType) {
			c.Fields = append(c.Fields, Field{Name: "flag", Type: TypeBoolean, Unique: true})
		}, []string{"fields", "4", "unique"}},
		{"min > max length", func(c *ContentType) { c.Fields[0].MinLength, c.Fields[0].MaxLength = ptr(10), ptr(5) }, []string{"fields", "0", "minLength"}},
		{"min > max", func(c *ContentType) { c.Fields[3].Min, c.Fields[3].Max = ptr(10.0), ptr(1.0) }, []string{"fields", "3", "min"}},
		{"bad regex", func(c *ContentType) { c.Fields[0].Regex = "([" }, []string{"fields", "0", "regex"}},
		{"empty enum", func(c *ContentType) { c.Fields = append(c.Fields, Field{Name: "kind", Type: TypeEnum}) }, []string{"fields", "4", "enum"}},
		{"duplicate enum", func(c *ContentType) {
			c.Fields = append(c.Fields, Field{Name: "kind", Type: TypeEnum, Enum: []string{"a", "a"}})
		}, []string{"fields", "4", "enum"}},
		{"bad default", func(c *ContentType) { c.Fields[3].Default = 1.5 }, []string{"fields", "3", "default"}},
		{"relation without target", func(c *ContentType) {
			c.Fields = append(c.Fields, Field{Name: "author", Type: TypeRelation, Relation: ManyToOne})
		}, []string{"fields", "4", "target"}},
		{"relation to unknown", func(c *ContentType) {
			c.Fields = append(c.Fields, Field{Name: "author", Type: TypeRelation, Relation: ManyToOne, Target: "nope"})
		}, []string{"fields", "4", "target"}},
		{"relation bad kind", func(c *ContentType) {
			c.Fields = append(c.Fields, Field{Name: "self", Type: TypeRelation, Relation: "lots", Target: "article"})
		}, []string{"fields", "4", "relation"}},
		{"media bad category", func(c *ContentType) {
			c.Fields = append(c.Fields, Field{Name: "cover", Type: TypeMedia, AllowedTypes: []string{"pdfs"}})
		}, []string{"fields", "4", "allowedTypes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct := article()
			tc.mutate(ct)
			ct.Normalize()
			err := ct.Validate(noLookup)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want ValidationError, got %v", err)
			}
			if ve.For(tc.path...) == "" {
				t.Fatalf("no error at %v; got %+v", tc.path, ve.Errors)
			}
		})
	}
}

func TestValidateRelationTargets(t *testing.T) {
	author := &ContentType{UID: "author", PluralName: "authors", Kind: KindCollection, DisplayName: "Author"}
	home := &ContentType{UID: "homepage", PluralName: "homepages", Kind: KindSingle, DisplayName: "Homepage"}
	lookup := func(uid string) (*ContentType, bool) {
		switch uid {
		case "author":
			return author, true
		case "homepage":
			return home, true
		}
		return nil, false
	}
	ct := article()
	ct.Fields = append(ct.Fields,
		Field{Name: "author", Type: TypeRelation, Relation: ManyToOne, Target: "author"},
		Field{Name: "related", Type: TypeRelation, Relation: ManyToMany, Target: "article"}, // self relation
	)
	if err := ct.Validate(lookup); err != nil {
		t.Fatalf("valid relations rejected: %v", err)
	}
	ct.Fields = append(ct.Fields, Field{Name: "home", Type: TypeRelation, Relation: ManyToOne, Target: "homepage"})
	var ve *ValidationError
	if err := ct.Validate(lookup); !errors.As(err, &ve) || ve.For("fields", "6", "target") == "" {
		t.Fatalf("relation to single type should fail, got %v", err)
	}
}

func TestNormalizeStripsIrrelevantOptions(t *testing.T) {
	ct := article()
	ct.Fields[0].Enum = []string{"x"}
	ct.Fields[0].Min = ptr(1.0)
	ct.Fields = append(ct.Fields, Field{Name: "cover", Type: TypeMedia, Unique: true, Default: "x", Regex: "a"})
	ct.Normalize()
	if ct.Fields[0].Enum != nil || ct.Fields[0].Min != nil {
		t.Fatal("string field kept enum/min options")
	}
	c := ct.Fields[4]
	if c.Unique || c.Default != nil || c.Regex != "" {
		t.Fatalf("media field kept scalar options: %+v", c)
	}
	if ct.Fields[3].Format != "integer" {
		t.Fatal("explicit number format lost")
	}
}

func TestTitleField(t *testing.T) {
	ct := article()
	if got := ct.TitleField(); got != "title" {
		t.Fatalf("TitleField = %q", got)
	}
	ct.Fields = []Field{{Name: "views", Type: TypeNumber}, {Name: "headline", Type: TypeString}}
	if got := ct.TitleField(); got != "headline" {
		t.Fatalf("TitleField = %q", got)
	}
}

func TestRouteName(t *testing.T) {
	ct := article()
	if ct.RouteName() != "articles" {
		t.Fatal(ct.RouteName())
	}
	ct.Kind = KindSingle
	if ct.RouteName() != "article" {
		t.Fatal(ct.RouteName())
	}
}
