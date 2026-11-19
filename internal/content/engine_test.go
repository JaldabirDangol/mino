package content_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"mino/internal/content"
	"mino/internal/schema"
	"mino/internal/testutil"
)

type fixture struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	schemas *schema.Service
	engine  *content.Engine
	site    int64
	queries *testutil.QueryCounter
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	qc := &testutil.QueryCounter{}
	pool := testutil.DBWith(t, func(c *pgxpool.Config) { c.ConnConfig.Tracer = qc })
	schemas := schema.NewService(pool)
	return &fixture{t: t, ctx: context.Background(), pool: pool, schemas: schemas,
		engine: content.New(pool, schemas), site: testutil.Site(t, pool, "site-a"), queries: qc}
}

func (fx *fixture) createType(siteID int64, ct *schema.ContentType) *schema.ContentType {
	fx.t.Helper()
	ct.SiteID = siteID
	out, err := fx.schemas.Create(fx.ctx, ct, 0)
	if err != nil {
		fx.t.Fatalf("create type %s: %v", ct.UID, err)
	}
	return out
}

func (fx *fixture) create(ct *schema.ContentType, data map[string]any, status content.Status) *content.Entry {
	fx.t.Helper()
	e, err := fx.engine.Create(fx.ctx, ct, data, content.WriteOptions{Status: status})
	if err != nil {
		fx.t.Fatalf("create %s: %v", ct.UID, err)
	}
	return e
}

func (fx *fixture) media(siteID int64, mime string) int64 {
	fx.t.Helper()
	var id int64
	err := fx.pool.QueryRow(fx.ctx, `INSERT INTO media (site_id, name, mime, size, storage, storage_key)
		VALUES ($1, 'f', $2, 1, 'local', gen_random_uuid()::text) RETURNING id`, siteID, mime).Scan(&id)
	if err != nil {
		fx.t.Fatal(err)
	}
	return id
}

func validationAt(t *testing.T, err error, field string) {
	t.Helper()
	var ve *schema.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError at %q, got %v", field, err)
	}
	if ve.For(field) == "" {
		t.Fatalf("no error at %q: %+v", field, ve.Errors)
	}
}

func articleType() *schema.ContentType {
	return &schema.ContentType{
		UID: "article", PluralName: "articles", Kind: schema.KindCollection, DisplayName: "Article", DraftPublish: true,
		Fields: []schema.Field{
			{Name: "title", Type: schema.TypeString, Required: true},
			{Name: "slug", Type: schema.TypeString, Unique: true},
			{Name: "views", Type: schema.TypeNumber, Format: "integer", Default: 0.0},
			{Name: "featured", Type: schema.TypeBoolean},
			{Name: "publishOn", Type: schema.TypeDate},
			{Name: "category", Type: schema.TypeEnum, Enum: []string{"news", "guide"}},
			{Name: "secret", Type: schema.TypeString, Private: true},
		},
	}
}

func TestCreateGetUpdateDelete(t *testing.T) {
	fx := newFixture(t)
	ct := fx.createType(fx.site, articleType())

	e := fx.create(ct, map[string]any{"title": "Hello", "slug": "hello"}, content.StatusAny)
	if e.Data["views"] != 0.0 {
		t.Fatalf("default not applied: %v", e.Data)
	}
	if e.Published() {
		t.Fatal("new entries of draft & publish types start as drafts")
	}

	upd, err := fx.engine.Update(fx.ctx, ct, e.ID, map[string]any{"views": 5.0, "slug": nil}, content.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Data["title"] != "Hello" || upd.Data["views"] != 5.0 {
		t.Fatalf("partial update lost data: %v", upd.Data)
	}
	if _, ok := upd.Data["slug"]; ok {
		t.Fatal("nil should clear the field")
	}

	_, err = fx.engine.Update(fx.ctx, ct, e.ID, map[string]any{"views": 1.5}, content.WriteOptions{})
	validationAt(t, err, "views")
	_, err = fx.engine.Update(fx.ctx, ct, e.ID, map[string]any{"nope": 1}, content.WriteOptions{})
	validationAt(t, err, "nope")

	if err := fx.engine.Delete(fx.ctx, ct, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.engine.Get(fx.ctx, ct, e.ID, content.ReadOptions{}); !errors.Is(err, content.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := fx.engine.Delete(fx.ctx, ct, e.ID); !errors.Is(err, content.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestDraftsSkipRequiredUntilPublished(t *testing.T) {
	fx := newFixture(t)
	ct := fx.createType(fx.site, articleType())

	draft := fx.create(ct, map[string]any{"slug": "no-title"}, content.StatusAny)
	_, err := fx.engine.Publish(fx.ctx, ct, draft.ID, 0)
	validationAt(t, err, "title")

	if _, err := fx.engine.Update(fx.ctx, ct, draft.ID, map[string]any{"title": "Now"}, content.WriteOptions{Status: content.StatusPublished}); err != nil {
		t.Fatal(err)
	}
	_, err = fx.engine.Create(fx.ctx, ct, map[string]any{"slug": "x"}, content.WriteOptions{Status: content.StatusPublished})
	validationAt(t, err, "title")

	fx.create(ct, map[string]any{"title": "Draft"}, content.StatusAny)
	count := func(s content.Status) int {
		res, err := fx.engine.List(fx.ctx, ct, content.Query{Status: s, PageSize: 10, Page: 1})
		if err != nil {
			t.Fatal(err)
		}
		return res.Total
	}
	if count(content.StatusPublished) != 1 || count(content.StatusDraft) != 1 || count(content.StatusAny) != 2 {
		t.Fatalf("status counts wrong: pub=%d draft=%d any=%d", count(content.StatusPublished), count(content.StatusDraft), count(content.StatusAny))
	}
	if _, err := fx.engine.Get(fx.ctx, ct, draft.ID, content.ReadOptions{Status: content.StatusDraft}); !errors.Is(err, content.ErrNotFound) {
		t.Fatal("published entry visible as draft")
	}
	if _, err := fx.engine.Unpublish(fx.ctx, ct, draft.ID, 0); err != nil {
		t.Fatal(err)
	}
	if count(content.StatusPublished) != 0 {
		t.Fatal("unpublish did not take effect")
	}
}

func TestTypesWithoutDraftPublishAreAlwaysPublished(t *testing.T) {
	fx := newFixture(t)
	ct := articleType()
	ct.DraftPublish = false
	ct = fx.createType(fx.site, ct)
	_, err := fx.engine.Create(fx.ctx, ct, map[string]any{"slug": "x"}, content.WriteOptions{})
	validationAt(t, err, "title")
	e := fx.create(ct, map[string]any{"title": "x"}, content.StatusAny)
	if !e.Published() {
		t.Fatal("entry should be published")
	}
}

func TestUniqueViolationIsFieldError(t *testing.T) {
	fx := newFixture(t)
	ct := fx.createType(fx.site, articleType())
	fx.create(ct, map[string]any{"title": "A", "slug": "same"}, content.StatusAny)
	_, err := fx.engine.Create(fx.ctx, ct, map[string]any{"title": "B", "slug": "same"}, content.WriteOptions{})
	validationAt(t, err, "slug")

	// Another site may reuse the value.
	other := testutil.Site(t, fx.pool, "site-b")
	ct2 := fx.createType(other, articleType())
	fx.create(ct2, map[string]any{"title": "A", "slug": "same"}, content.StatusAny)
}

func relationTypes(fx *fixture, site int64) (author, tag, post *schema.ContentType) {
	author = fx.createType(site, &schema.ContentType{UID: "author", PluralName: "authors", Kind: schema.KindCollection, DisplayName: "Author", DraftPublish: true,
		Fields: []schema.Field{{Name: "name", Type: schema.TypeString}}})
	tag = fx.createType(site, &schema.ContentType{UID: "tag", PluralName: "tags", Kind: schema.KindCollection, DisplayName: "Tag",
		Fields: []schema.Field{{Name: "name", Type: schema.TypeString}}})
	post = fx.createType(site, &schema.ContentType{UID: "post", PluralName: "posts", Kind: schema.KindCollection, DisplayName: "Post",
		Fields: []schema.Field{
			{Name: "title", Type: schema.TypeString},
			{Name: "author", Type: schema.TypeRelation, Relation: schema.ManyToOne, Target: "author", Required: true},
			{Name: "tags", Type: schema.TypeRelation, Relation: schema.ManyToMany, Target: "tag"},
			{Name: "featuredTag", Type: schema.TypeRelation, Relation: schema.OneToOne, Target: "tag"},
			{Name: "pinned", Type: schema.TypeRelation, Relation: schema.OneToMany, Target: "tag"},
			{Name: "related", Type: schema.TypeRelation, Relation: schema.ManyToMany, Target: "post"},
			{Name: "cover", Type: schema.TypeMedia, AllowedTypes: []string{"images"}},
			{Name: "gallery", Type: schema.TypeMedia, Multiple: true},
		}})
	return
}

func TestRelations(t *testing.T) {
	fx := newFixture(t)
	authorT, tagT, postT := relationTypes(fx, fx.site)
	alice := fx.create(authorT, map[string]any{"name": "Alice"}, content.StatusPublished)
	bob := fx.create(authorT, map[string]any{"name": "Bob"}, content.StatusAny) // draft
	t1 := fx.create(tagT, map[string]any{"name": "go"}, content.StatusAny)
	t2 := fx.create(tagT, map[string]any{"name": "cms"}, content.StatusAny)
	t3 := fx.create(tagT, map[string]any{"name": "web"}, content.StatusAny)

	// Required relation enforced (post has no draft & publish).
	_, err := fx.engine.Create(fx.ctx, postT, map[string]any{"title": "x"}, content.WriteOptions{})
	validationAt(t, err, "author")
	// Single-valued relation rejects arrays of several ids.
	_, err = fx.engine.Create(fx.ctx, postT, map[string]any{"author": []any{float64(alice.ID), float64(bob.ID)}}, content.WriteOptions{})
	validationAt(t, err, "author")
	// Target must be of the right type.
	_, err = fx.engine.Create(fx.ctx, postT, map[string]any{"author": float64(t1.ID)}, content.WriteOptions{})
	validationAt(t, err, "author")

	p1 := fx.create(postT, map[string]any{
		"title": "One", "author": float64(alice.ID),
		"tags":        []any{float64(t3.ID), map[string]any{"id": float64(t1.ID)}}, // order kept, {"id"} accepted
		"featuredTag": float64(t1.ID),
		"pinned":      []any{float64(t1.ID), float64(t2.ID)},
	}, content.StatusAny)
	if got := p1.Links["tags"]; len(got) != 2 || got[0] != t3.ID || got[1] != t1.ID {
		t.Fatalf("tags order: %v", got)
	}

	// oneToOne / oneToMany targets belong to one source: assigning them elsewhere moves them.
	p2 := fx.create(postT, map[string]any{"title": "Two", "author": float64(bob.ID), "featuredTag": float64(t1.ID),
		"pinned": []any{float64(t2.ID)}, "tags": []any{float64(t1.ID)}, "related": []any{float64(p1.ID)}}, content.StatusAny)
	p1, _ = fx.engine.Get(fx.ctx, postT, p1.ID, content.ReadOptions{})
	if len(p1.Links["featuredTag"]) != 0 {
		t.Fatal("oneToOne target should have moved to p2")
	}
	if got := p1.Links["pinned"]; len(got) != 1 || got[0] != t1.ID {
		t.Fatalf("oneToMany: p1 should keep only t1, got %v", got)
	}
	if len(p1.Links["tags"]) != 2 {
		t.Fatal("manyToMany targets must be shareable")
	}

	// Populate in published mode hides draft targets.
	got, err := fx.engine.Get(fx.ctx, postT, p2.ID, content.ReadOptions{Status: content.StatusPublished, Populate: []string{"author", "related"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Relations["author"]) != 0 {
		t.Fatal("draft author populated in published mode")
	}
	if len(got.Relations["related"]) != 1 || got.Relations["related"][0].Data["title"] != "One" {
		t.Fatalf("self relation not populated: %+v", got.Relations["related"])
	}
	js := content.Serialize(got, content.SerializeOptions{})
	if js["author"] != nil {
		t.Fatalf("missing single relation must serialize as null, got %v", js["author"])
	}
	if _, ok := js["tags"]; ok {
		t.Fatal("unpopulated relation must be omitted")
	}

	// Filter by relation id.
	q, err := content.ParseQuery(postT, url.Values{"filters[tags][id][$eq]": {fmt.Sprint(t3.ID)}}, content.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	q.Status = content.StatusAny
	res, err := fx.engine.List(fx.ctx, postT, q)
	if err != nil || res.Total != 1 || res.Entries[0].ID != p1.ID {
		t.Fatalf("relation filter: %v %+v", err, res)
	}

	// Deleting a target removes the link.
	if err := fx.engine.Delete(fx.ctx, tagT, t3.ID); err != nil {
		t.Fatal(err)
	}
	p1, _ = fx.engine.Get(fx.ctx, postT, p1.ID, content.ReadOptions{})
	if len(p1.Links["tags"]) != 1 {
		t.Fatalf("link to deleted entry remains: %v", p1.Links["tags"])
	}
}

func TestRelationsAndMediaStayWithinSite(t *testing.T) {
	fx := newFixture(t)
	authorA, _, postA := relationTypes(fx, fx.site)
	siteB := testutil.Site(t, fx.pool, "site-b")
	authorB, _, _ := relationTypes(fx, siteB)
	alice := fx.create(authorA, map[string]any{"name": "Alice"}, content.StatusAny)
	foreign := fx.create(authorB, map[string]any{"name": "Foreign"}, content.StatusAny)

	_, err := fx.engine.Create(fx.ctx, postA, map[string]any{"author": float64(foreign.ID)}, content.WriteOptions{})
	validationAt(t, err, "author")

	foreignImg := fx.media(siteB, "image/png")
	_, err = fx.engine.Create(fx.ctx, postA, map[string]any{"author": float64(alice.ID), "cover": float64(foreignImg)}, content.WriteOptions{})
	validationAt(t, err, "cover")
}

func TestMediaLinks(t *testing.T) {
	fx := newFixture(t)
	authorT, _, postT := relationTypes(fx, fx.site)
	alice := fx.create(authorT, map[string]any{"name": "Alice"}, content.StatusAny)
	img := fx.media(fx.site, "image/png")
	pdf := fx.media(fx.site, "application/pdf")

	_, err := fx.engine.Create(fx.ctx, postT, map[string]any{"author": float64(alice.ID), "cover": float64(pdf)}, content.WriteOptions{})
	validationAt(t, err, "cover") // images only

	p := fx.create(postT, map[string]any{"author": float64(alice.ID), "cover": float64(img), "gallery": []any{float64(pdf), float64(img)}}, content.StatusAny)
	got, err := fx.engine.Get(fx.ctx, postT, p.ID, content.ReadOptions{Populate: []string{"cover", "gallery"}})
	if err != nil {
		t.Fatal(err)
	}
	js := content.Serialize(got, content.SerializeOptions{})
	if cover, ok := js["cover"].(map[string]any); !ok || cover["id"] != img {
		t.Fatalf("cover: %#v", js["cover"])
	}
	if gal, ok := js["gallery"].([]any); !ok || len(gal) != 2 || gal[0].(map[string]any)["id"] != pdf {
		t.Fatalf("gallery: %#v", js["gallery"])
	}
	if _, err := fx.pool.Exec(fx.ctx, `DELETE FROM media WHERE id = $1`, img); err != nil {
		t.Fatal(err)
	}
	got, _ = fx.engine.Get(fx.ctx, postT, p.ID, content.ReadOptions{})
	if len(got.Links["cover"]) != 0 || len(got.Links["gallery"]) != 1 {
		t.Fatalf("media links not removed with media: %v", got.Links)
	}
}

func TestListDoesNotIssueQueriesPerEntry(t *testing.T) {
	fx := newFixture(t)
	authorT, tagT, postT := relationTypes(fx, fx.site)
	a := fx.create(authorT, map[string]any{"name": "A"}, content.StatusAny)
	tg := fx.create(tagT, map[string]any{"name": "t"}, content.StatusAny)
	img := fx.media(fx.site, "image/png")
	for i := range 30 {
		fx.create(postT, map[string]any{"title": fmt.Sprint(i), "author": float64(a.ID), "tags": []any{float64(tg.ID)}, "cover": float64(img)}, content.StatusAny)
	}
	q := content.Query{Page: 1, PageSize: 30, Populate: []string{"author", "tags", "cover", "gallery"}}
	fx.queries.Reset()
	res, err := fx.engine.List(fx.ctx, postT, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 30 || len(res.Entries[29].Relations["author"]) != 1 {
		t.Fatalf("unexpected result: %d entries", len(res.Entries))
	}
	// count + page + relations + media + target types + targets = 6
	if n := fx.queries.Count(); n > 6 {
		t.Fatalf("List issued %d queries for 30 entries; expected a constant number", n)
	}
}

func TestFiltersSortPagination(t *testing.T) {
	fx := newFixture(t)
	ct := fx.createType(fx.site, articleType())
	rows := []map[string]any{
		{"title": "Alpha Guide", "slug": "a", "views": 10.0, "featured": true, "publishOn": "2024-01-10", "category": "guide"},
		{"title": "beta news", "slug": "b", "views": 50.0, "featured": false, "publishOn": "2024-03-01", "category": "news"},
		{"title": "Gamma", "slug": "c", "views": 5.0, "publishOn": "2023-12-31"},
		{"title": "Delta 100%", "slug": "d", "views": 100.0, "featured": true, "category": "news", "secret": "x"},
	}
	for _, r := range rows {
		fx.create(ct, r, content.StatusPublished)
	}

	cases := []struct {
		name  string
		query string
		want  []string // slugs in order
	}{
		{"eq string", "filters[title][$eq]=Gamma", []string{"c"}},
		{"implicit eq", "filters[category]=news&sort=slug", []string{"b", "d"}},
		{"eq number", "filters[views][$eq]=50", []string{"b"}},
		{"eq boolean", "filters[featured][$eq]=true&sort=slug", []string{"a", "d"}},
		{"ne", "filters[category][$ne]=news&sort=slug", []string{"a", "c"}},
		{"gt number", "filters[views][$gt]=9&sort=views:desc", []string{"d", "b", "a"}},
		{"lte number", "filters[views][$lte]=10&sort=views", []string{"c", "a"}},
		{"between", "filters[views][$between]=6&filters[views][$between]=60&sort=slug", []string{"a", "b"}},
		{"date range", "filters[publishOn][$gte]=2024-01-01&sort=publishOn", []string{"a", "b"}},
		{"containsi", "filters[title][$containsi]=GUIDE", []string{"a"}},
		{"contains is case sensitive", "filters[title][$contains]=guide", nil},
		{"contains escapes wildcards", "filters[title][$contains]=100%25", []string{"d"}},
		{"startsWith", "filters[title][$startsWith]=be", []string{"b"}},
		{"in", "filters[slug][$in][0]=a&filters[slug][$in][1]=c&sort=slug", []string{"a", "c"}},
		{"notIn", "filters[slug][$notIn]=a&filters[slug][$notIn]=b&sort=slug", []string{"c", "d"}},
		{"null", "filters[category][$null]=true", []string{"c"}},
		{"notNull", "filters[publishOn][$notNull]=true&sort=slug", []string{"a", "b", "c"}},
		{"or", "filters[$or][0][slug][$eq]=a&filters[$or][1][views][$gt]=60&sort=slug", []string{"a", "d"}},
		{"not", "filters[$not][featured][$eq]=true&sort=slug", []string{"b", "c"}},
		{"id in", "filters[id][$in]=1&filters[id][$in]=2&sort=id", []string{"a", "b"}},
		{"sort desc", "sort=title:desc", []string{"c", "d", "b", "a"}},
		{"page 2", "sort=slug&pagination[page]=2&pagination[pageSize]=3", []string{"d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, _ := url.ParseQuery(tc.query)
			q, err := content.ParseQuery(ct, v, content.DefaultLimits)
			if err != nil {
				t.Fatal(err)
			}
			res, err := fx.engine.List(fx.ctx, ct, q)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, e := range res.Entries {
				got = append(got, e.Data["slug"].(string))
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	res, _ := fx.engine.List(fx.ctx, ct, content.Query{Page: 1, PageSize: 3, Sort: []content.Sort{{Field: "slug"}}})
	if res.Total != 4 || res.PageCount() != 2 {
		t.Fatalf("pagination meta: total=%d pages=%d", res.Total, res.PageCount())
	}
	res, _ = fx.engine.List(fx.ctx, ct, content.Query{Page: 1, PageSize: 10, Search: "delta"})
	if res.Total != 1 {
		t.Fatalf("search: %d", res.Total)
	}
}

func TestParseQueryRejects(t *testing.T) {
	ct := articleType()
	for _, q := range []string{
		"filters[unknown][$eq]=1",
		"filters[title][$regex]=x",
		"filters[secret][$eq]=x", // private fields cannot be filtered
		"filters[views][$gt]=abc",
		"sort=title:sideways",
		"sort=nope",
		"fields=nope",
		"populate=title",
		"status=deleted",
		"pagination[page]=0",
		"pagination[pageSize]=x",
		"filters[title]][x]=1",
	} {
		v, _ := url.ParseQuery(q)
		parsed, err := content.ParseQuery(ct, v, content.DefaultLimits)
		if err == nil {
			// Value conversion errors surface when SQL is built.
			_, err = content.BuildWhereForTest(ct, parsed)
		}
		var qe *content.QueryError
		if !errors.As(err, &qe) {
			t.Errorf("%s: want QueryError, got %v", q, err)
		}
	}
}

func TestParseQueryDefaults(t *testing.T) {
	ct := articleType()
	v, _ := url.ParseQuery("pagination[pageSize]=1000&fields[0]=title&fields[1]=slug&populate=*")
	q, err := content.ParseQuery(ct, v, content.Limits{DefaultPageSize: 10, MaxPageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if q.PageSize != 50 || q.Page != 1 || q.Status != content.StatusPublished {
		t.Fatalf("defaults: %+v", q)
	}
	if fmt.Sprint(q.Fields) != "[title slug]" {
		t.Fatalf("fields: %v", q.Fields)
	}
}

func TestSerializeFieldsAndPrivate(t *testing.T) {
	fx := newFixture(t)
	ct := fx.createType(fx.site, articleType())
	e := fx.create(ct, map[string]any{"title": "T", "secret": "s"}, content.StatusAny)
	js := content.Serialize(e, content.SerializeOptions{})
	if _, ok := js["secret"]; ok {
		t.Fatal("private field exposed")
	}
	if v, ok := js["slug"]; !ok || v != nil {
		t.Fatal("absent fields should be present as null")
	}
	js = content.Serialize(e, content.SerializeOptions{Fields: []string{"title"}})
	if len(js) != 2 || js["title"] != "T" {
		t.Fatalf("fields selection: %v", js)
	}
	if js = content.Serialize(e, content.SerializeOptions{IncludePrivate: true}); js["secret"] != "s" {
		t.Fatal("admin view must include private fields")
	}
}

func TestSingleTypes(t *testing.T) {
	fx := newFixture(t)
	ct := fx.createType(fx.site, &schema.ContentType{UID: "homepage", PluralName: "homepages", Kind: schema.KindSingle, DisplayName: "Homepage", DraftPublish: true,
		Fields: []schema.Field{{Name: "headline", Type: schema.TypeString}}})
	if _, err := fx.engine.GetSingle(fx.ctx, ct, content.ReadOptions{}); !errors.Is(err, content.ErrNotFound) {
		t.Fatalf("empty single type: %v", err)
	}
	first, err := fx.engine.UpsertSingle(fx.ctx, ct, map[string]any{"headline": "Hi"}, content.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := fx.engine.UpsertSingle(fx.ctx, ct, map[string]any{"headline": "Hello"}, content.WriteOptions{Status: content.StatusPublished})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || second.Data["headline"] != "Hello" || !second.Published() {
		t.Fatalf("upsert should update the single entry: %+v", second)
	}
	if _, err := fx.engine.Create(fx.ctx, ct, map[string]any{}, content.WriteOptions{}); !errors.Is(err, content.ErrSingleExists) {
		t.Fatalf("second create: %v", err)
	}
}

func TestDuplicate(t *testing.T) {
	fx := newFixture(t)
	authorT, tagT, postT := relationTypes(fx, fx.site)
	a := fx.create(authorT, map[string]any{"name": "A"}, content.StatusAny)
	tg := fx.create(tagT, map[string]any{"name": "t"}, content.StatusAny)
	p := fx.create(postT, map[string]any{"title": "Orig", "author": float64(a.ID), "tags": []any{float64(tg.ID)}, "featuredTag": float64(tg.ID)}, content.StatusAny)

	cp, err := fx.engine.Duplicate(fx.ctx, postT, p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cp.ID == p.ID || cp.Data["title"] != "Orig" || len(cp.Links["tags"]) != 1 || len(cp.Links["author"]) != 1 {
		t.Fatalf("copy: %+v", cp)
	}
	if len(cp.Links["featuredTag"]) != 0 {
		t.Fatal("exclusive relation must not be copied")
	}
	if p, _ = fx.engine.Get(fx.ctx, postT, p.ID, content.ReadOptions{}); len(p.Links["featuredTag"]) != 1 {
		t.Fatal("original lost its exclusive relation")
	}

	art := fx.createType(fx.site, articleType())
	orig := fx.create(art, map[string]any{"title": "X", "slug": "unique-slug"}, content.StatusAny)
	cp, err = fx.engine.Duplicate(fx.ctx, art, orig.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cp.Data["slug"]; ok {
		t.Fatal("unique value copied")
	}
}
