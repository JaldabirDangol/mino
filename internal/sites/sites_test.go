package sites_test

import (
	"context"
	"errors"
	"testing"

	"mino/internal/schema"
	"mino/internal/sites"
	"mino/internal/testutil"
)

func TestValidate(t *testing.T) {
	for slug, ok := range map[string]bool{
		"acme": true, "acme-blog": true, "a": true, "site2": true,
		"": false, "Acme": false, "-acme": false, "acme-": false, "acme_blog": false, "admin": false, "api": false,
	} {
		s := &sites.Site{Name: "X", Slug: slug}
		if err := s.Validate(); (err == nil) != ok {
			t.Errorf("slug %q: valid=%v, err=%v", slug, ok, err)
		}
	}
}

func TestCRUD(t *testing.T) {
	pool := testutil.DB(t)
	svc := sites.NewService(pool)
	ctx := context.Background()

	a, err := svc.Create(ctx, &sites.Site{Name: "Acme Blog", Slug: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	var ve *schema.ValidationError
	if _, err := svc.Create(ctx, &sites.Site{Name: "Other", Slug: "acme"}); !errors.As(err, &ve) || ve.For("slug") == "" {
		t.Fatalf("duplicate slug: want validation error, got %v", err)
	}

	a.Name, a.Slug = "Acme", "acme-site"
	if _, err := svc.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Get(ctx, "acme-site"); err != nil || got.Name != "Acme" {
		t.Fatalf("get after rename: %v %+v", err, got)
	}
	if _, err := svc.Get(ctx, "acme"); !errors.Is(err, sites.ErrNotFound) {
		t.Fatalf("old slug still resolves: %v", err)
	}
}

func TestDeleteRemovesEverythingOwned(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	svc := sites.NewService(pool)
	types := schema.NewService(pool)

	keep, _ := svc.Create(ctx, &sites.Site{Name: "Keep", Slug: "keep"})
	gone, _ := svc.Create(ctx, &sites.Site{Name: "Gone", Slug: "gone"})
	for _, site := range []*sites.Site{keep, gone} {
		ct := &schema.ContentType{SiteID: site.ID, UID: "post", PluralName: "posts", Kind: schema.KindCollection, DisplayName: "Post",
			Fields: []schema.Field{{Name: "slug", Type: schema.TypeString, Unique: true}}}
		created, err := types.Create(ctx, ct, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO entries (content_type_id, data) VALUES ($1, '{"slug":"x"}')`, created.ID); err != nil {
			t.Fatal(err)
		}
	}
	if st, err := svc.Stats(ctx, gone.ID); err != nil || st.ContentTypes != 1 || st.Entries != 1 {
		t.Fatalf("stats: %+v %v", st, err)
	}

	if err := svc.Delete(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := svc.Stats(ctx, keep.ID); st.ContentTypes != 1 || st.Entries != 1 {
		t.Fatalf("other site affected: %+v", st)
	}
	var orphanIndexes int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname LIKE 'entries_ct%_uniq'`).Scan(&orphanIndexes)
	if orphanIndexes != 1 {
		t.Fatalf("want 1 unique index left (site keep), got %d", orphanIndexes)
	}
	if err := svc.Delete(ctx, gone.ID); !errors.Is(err, sites.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}
