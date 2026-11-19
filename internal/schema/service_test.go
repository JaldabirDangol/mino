package schema_test

import (
	"context"
	"errors"
	"testing"

	"mino/internal/db"
	"mino/internal/schema"
	"mino/internal/testutil"
)

func newArticle() *schema.ContentType {
	return &schema.ContentType{
		UID: "article", PluralName: "articles", Kind: schema.KindCollection, DisplayName: "Article", DraftPublish: true,
		Fields: []schema.Field{
			{Name: "title", Type: schema.TypeString, Required: true},
			{Name: "slug", Type: schema.TypeString},
			{Name: "status", Type: schema.TypeEnum, Enum: []string{"a", "b"}},
		},
	}
}

func setup(t *testing.T) (*schema.Service, *schema.ContentType, func(data string)) {
	t.Helper()
	pool := testutil.DB(t)
	svc := schema.NewService(pool)
	a := newArticle()
	a.SiteID = testutil.Site(t, pool, "site-a")
	ct, err := svc.Create(context.Background(), a, 0)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(data string) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), `INSERT INTO entries (content_type_id, data) VALUES ($1, $2)`, ct.ID, data); err != nil {
			t.Fatal(err)
		}
	}
	return svc, ct, insert
}

func planError(t *testing.T, err error) *schema.Plan {
	t.Helper()
	var pe *schema.PlanError
	if !errors.As(err, &pe) {
		t.Fatalf("want PlanError, got %v", err)
	}
	return pe.Plan
}

func TestCreateRejectsRouteCollision(t *testing.T) {
	svc, a, _ := setup(t)
	ct := &schema.ContentType{SiteID: a.SiteID, UID: "articles", PluralName: "article-list", Kind: schema.KindCollection, DisplayName: "X"}
	var ve *schema.ValidationError
	if _, err := svc.Create(context.Background(), ct, 0); !errors.As(err, &ve) || ve.For("uid") == "" {
		t.Fatalf("want uid collision error, got %v", err)
	}
}

func TestRemovingFieldWithDataNeedsConfirmation(t *testing.T) {
	svc, ct, insert := setup(t)
	ctx := context.Background()
	insert(`{"title": "one", "slug": "one"}`)
	insert(`{"title": "two"}`)

	next := newArticle()
	next.Fields = next.Fields[:1] // drop slug and status

	_, _, err := svc.Update(ctx, ct.SiteID, ct.UID, next, false, 0)
	plan := planError(t, err)
	if plan.Blocked() || !plan.NeedsConfirmation() {
		t.Fatalf("expected confirmation-only plan, got %+v", plan.Impacts)
	}
	n := plan.Notable()
	if len(n) != 1 || n[0].Affected != 1 || n[0].Level != schema.ImpactDestructive {
		t.Fatalf("unexpected impacts: %+v", n)
	}

	// Data is untouched until confirmed.
	if got, _ := svc.Get(ctx, ct.SiteID, ct.UID); len(got.Fields) != 3 {
		t.Fatal("schema changed without confirmation")
	}

	updated, _, err := svc.Update(ctx, ct.SiteID, ct.UID, next, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 || len(updated.Fields) != 1 {
		t.Fatalf("unexpected result: version %d, %d fields", updated.Version, len(updated.Fields))
	}
}

func TestTypeChangeBlockedWithIncompatibleData(t *testing.T) {
	svc, ct, insert := setup(t)
	ctx := context.Background()
	insert(`{"title": "one", "slug": "one"}`)

	next := newArticle()
	next.Fields[1].Type = schema.TypeNumber
	_, _, err := svc.Update(ctx, ct.SiteID, ct.UID, next, true, 0)
	if plan := planError(t, err); !plan.Blocked() {
		t.Fatal("number conversion of text data should be blocked even when confirmed")
	}

	// String -> Text is safe.
	next.Fields[1].Type = schema.TypeText
	if _, _, err := svc.Update(ctx, ct.SiteID, ct.UID, next, false, 0); err != nil {
		t.Fatalf("safe change rejected: %v", err)
	}

	// Text -> Email is not in the safe table: blocked while data exists.
	next.Fields[1].Type = schema.TypeEmail
	if _, _, err := svc.Update(ctx, ct.SiteID, ct.UID, next, true, 0); !planError(t, err).Blocked() {
		t.Fatal("text -> email with data should be blocked")
	}
}

func TestUniqueConstraint(t *testing.T) {
	svc, ct, insert := setup(t)
	ctx := context.Background()
	insert(`{"title": "one", "slug": "dup"}`)
	insert(`{"title": "two", "slug": "dup"}`)

	next := newArticle()
	next.Fields[1].Unique = true
	_, _, err := svc.Update(ctx, ct.SiteID, ct.UID, next, true, 0)
	if !planError(t, err).Blocked() {
		t.Fatal("unique with duplicates should be blocked")
	}
}

func TestUniqueIndexEnforced(t *testing.T) {
	pool := testutil.DB(t)
	svc := schema.NewService(pool)
	ctx := context.Background()
	ct := newArticle()
	ct.SiteID = testutil.Site(t, pool, "site-a")
	ct.Fields[1].Unique = true
	created, err := svc.Create(ctx, ct, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO entries (content_type_id, data) VALUES ($1, '{"slug":"x"}')`, created.ID); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO entries (content_type_id, data) VALUES ($1, '{"slug":"x"}')`, created.ID)
	name, ok := db.IsUniqueViolation(err)
	if !ok || name != created.UniqueIndexName("slug") {
		t.Fatalf("want unique violation on %s, got %v", created.UniqueIndexName("slug"), err)
	}

	// Dropping the constraint removes the index.
	next := newArticle()
	if _, _, err := svc.Update(ctx, created.SiteID, created.UID, next, false, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO entries (content_type_id, data) VALUES ($1, '{"slug":"x"}')`, created.ID); err != nil {
		t.Fatalf("index not dropped: %v", err)
	}
}

func TestEnumValueRemovalClearsValuesAfterConfirm(t *testing.T) {
	svc, ct, insert := setup(t)
	ctx := context.Background()
	insert(`{"title": "one", "status": "b"}`)
	insert(`{"title": "two", "status": "a"}`)

	next := newArticle()
	next.Fields[2].Enum = []string{"a"}
	if _, _, err := svc.Update(ctx, ct.SiteID, ct.UID, next, false, 0); planError(t, err).Notable()[0].Affected != 1 {
		t.Fatal("expected one affected entry")
	}
	if _, _, err := svc.Update(ctx, ct.SiteID, ct.UID, next, true, 0); err != nil {
		t.Fatal(err)
	}
}

func TestMakingRequiredWarns(t *testing.T) {
	svc, ct, insert := setup(t)
	insert(`{"title": "one"}`)
	next := newArticle()
	next.Fields[1].Required = true
	plan := planError(t, func() error {
		_, _, err := svc.Update(context.Background(), ct.SiteID, ct.UID, next, false, 0)
		return err
	}())
	if n := plan.Notable(); len(n) != 1 || n[0].Level != schema.ImpactWarning {
		t.Fatalf("unexpected plan: %+v", n)
	}
}

func TestCollectionToSingleBlockedWithManyEntries(t *testing.T) {
	svc, ct, insert := setup(t)
	insert(`{"title": "one"}`)
	insert(`{"title": "two"}`)
	next := newArticle()
	next.Kind = schema.KindSingle
	if _, _, err := svc.Update(context.Background(), ct.SiteID, ct.UID, next, true, 0); !planError(t, err).Blocked() {
		t.Fatal("expected blocked plan")
	}
}

func TestDeleteBlockedByRelations(t *testing.T) {
	svc, a, _ := setup(t)
	ctx := context.Background()
	site := a.SiteID
	tag := &schema.ContentType{SiteID: site, UID: "tag", PluralName: "tags", Kind: schema.KindCollection, DisplayName: "Tag",
		Fields: []schema.Field{{Name: "articles", Type: schema.TypeRelation, Relation: schema.ManyToMany, Target: "article"}}}
	if _, err := svc.Create(ctx, tag, 0); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, site, "article"); err == nil {
		t.Fatal("delete should be refused while tag.articles targets article")
	}
	if err := svc.Delete(ctx, site, "tag"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, site, "article"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, site, "article"); !errors.Is(err, schema.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestGetByRoute(t *testing.T) {
	svc, a, _ := setup(t)
	ctx := context.Background()
	if ct, err := svc.GetByRoute(ctx, a.SiteID, "articles"); err != nil || ct.UID != "article" {
		t.Fatalf("collection route: %v", err)
	}
	if _, err := svc.GetByRoute(ctx, a.SiteID, "article"); !errors.Is(err, schema.ErrNotFound) {
		t.Fatal("singular name of a collection must not resolve")
	}
}

func TestSitesHaveIndependentContentTypes(t *testing.T) {
	pool := testutil.DB(t)
	svc := schema.NewService(pool)
	ctx := context.Background()
	siteA := testutil.Site(t, pool, "site-a")
	siteB := testutil.Site(t, pool, "site-b")

	for _, site := range []int64{siteA, siteB} {
		ct := newArticle()
		ct.SiteID = site
		if _, err := svc.Create(ctx, ct, 0); err != nil {
			t.Fatalf("site %d: same uid in another site must be allowed: %v", site, err)
		}
	}
	// Changing site B's article leaves site A's untouched.
	next := newArticle()
	next.Fields = append(next.Fields, schema.Field{Name: "extra", Type: schema.TypeText})
	if _, _, err := svc.Update(ctx, siteB, "article", next, false, 0); err != nil {
		t.Fatal(err)
	}
	a, _ := svc.Get(ctx, siteA, "article")
	b, _ := svc.Get(ctx, siteB, "article")
	if len(a.Fields) != 3 || len(b.Fields) != 4 {
		t.Fatalf("fields leaked between sites: a=%d b=%d", len(a.Fields), len(b.Fields))
	}
	if list, _ := svc.List(ctx, siteA); len(list) != 1 {
		t.Fatalf("site A lists %d types", len(list))
	}

	// Relations cannot target another site's type.
	tag := &schema.ContentType{SiteID: siteA, UID: "tag", PluralName: "tags", Kind: schema.KindCollection, DisplayName: "Tag",
		Fields: []schema.Field{{Name: "page", Type: schema.TypeRelation, Relation: schema.ManyToOne, Target: "page"}}}
	page := &schema.ContentType{SiteID: siteB, UID: "page", PluralName: "pages", Kind: schema.KindCollection, DisplayName: "Page"}
	if _, err := svc.Create(ctx, page, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, tag, 0); err == nil {
		t.Fatal("relation to another site's content type must be rejected")
	}
}
