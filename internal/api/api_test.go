package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mino/internal/api"
	"mino/internal/apitokens"
	"mino/internal/content"
	"mino/internal/media"
	"mino/internal/schema"
	"mino/internal/settings"
	"mino/internal/sites"
	"mino/internal/testutil"
)

type env struct {
	t      *testing.T
	srv    *httptest.Server
	tokens *apitokens.Service
	engine *content.Engine
	types  *schema.Service
	siteA  *sites.Site
	siteB  *sites.Site
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testutil.DB(t)
	ctx := context.Background()
	siteSvc := sites.NewService(pool)
	types := schema.NewService(pool)
	store := &media.Local{Dir: t.TempDir(), BaseURL: "/uploads"}
	mediaSvc := media.NewService(pool, store, 1<<20, testutil.Logger())
	engine := content.New(pool, types)
	engine.MediaURL = mediaSvc.URL
	tokens := apitokens.NewService(pool)
	h := &api.Handler{Sites: siteSvc, Schemas: types, Engine: engine, Media: mediaSvc, Tokens: tokens,
		Settings: settings.NewService(pool), Log: testutil.Logger(), CORSOrigins: []string{"*"}}
	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", h.Routes()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a, _ := siteSvc.Create(ctx, &sites.Site{Name: "A", Slug: "site-a"})
	b, _ := siteSvc.Create(ctx, &sites.Site{Name: "B", Slug: "site-b"})
	e := &env{t: t, srv: srv, tokens: tokens, engine: engine, types: types, siteA: a, siteB: b}
	for _, s := range []*sites.Site{a, b} {
		e.mustType(s, &schema.ContentType{UID: "author", PluralName: "authors", Kind: schema.KindCollection, DisplayName: "Author",
			Fields: []schema.Field{{Name: "name", Type: schema.TypeString}, {Name: "email", Type: schema.TypeEmail, Private: true}}})
		e.mustType(s, &schema.ContentType{UID: "article", PluralName: "articles", Kind: schema.KindCollection, DisplayName: "Article", DraftPublish: true,
			Fields: []schema.Field{
				{Name: "title", Type: schema.TypeString, Required: true},
				{Name: "views", Type: schema.TypeNumber, Format: "integer"},
				{Name: "author", Type: schema.TypeRelation, Relation: schema.ManyToOne, Target: "author"},
				{Name: "cover", Type: schema.TypeMedia},
			}})
		e.mustType(s, &schema.ContentType{UID: "homepage", PluralName: "homepages", Kind: schema.KindSingle, DisplayName: "Homepage",
			Fields: []schema.Field{{Name: "headline", Type: schema.TypeString}}})
	}
	return e
}

func (e *env) mustType(s *sites.Site, ct *schema.ContentType) {
	e.t.Helper()
	ct.SiteID = s.ID
	if _, err := e.types.Create(context.Background(), ct, 0); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) token(s *sites.Site, typ string, perms ...string) string {
	e.t.Helper()
	secret, _, err := e.tokens.Create(context.Background(), &apitokens.Token{SiteID: s.ID, Name: fmt.Sprint(typ, perms, len(e.t.Name())), Type: typ, Permissions: perms}, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return secret
}

type response struct {
	Status int
	Body   map[string]any
}

func (r response) data() map[string]any { m, _ := r.Body["data"].(map[string]any); return m }
func (r response) list() []any          { l, _ := r.Body["data"].([]any); return l }
func (r response) errMsg() string {
	if e, ok := r.Body["error"].(map[string]any); ok {
		return fmt.Sprint(e["message"])
	}
	return ""
}

func (e *env) do(method, path, token string, body any) response {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := response{Status: resp.StatusCode}
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.Body); err != nil {
			e.t.Fatalf("%s %s: invalid JSON %q", method, path, raw)
		}
	}
	return out
}

func expect(t *testing.T, r response, status int) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status %d, want %d; body %v", r.Status, status, r.Body)
	}
}

func TestCRUDWithFullAccessToken(t *testing.T) {
	e := setup(t)
	tok := e.token(e.siteA, apitokens.FullAccess)

	r := e.do("POST", "/api/site-a/articles", tok, map[string]any{"data": map[string]any{"title": "Hello", "views": 3}})
	expect(t, r, 201)
	id := r.data()["id"]
	if r.data()["publishedAt"] != nil {
		t.Fatal("API creates drafts unless ?status=published")
	}

	r = e.do("POST", "/api/site-a/articles", tok, map[string]any{"data": map[string]any{"views": 1.5}})
	expect(t, r, 400)
	if errs := r.Body["error"].(map[string]any)["details"].(map[string]any)["errors"].([]any); len(errs) != 1 {
		t.Fatalf("expected one field error (drafts skip required), got %v", errs)
	}

	r = e.do("PUT", fmt.Sprintf("/api/site-a/articles/%v?status=published", id), tok, map[string]any{"data": map[string]any{"views": 4}})
	expect(t, r, 200)
	if r.data()["title"] != "Hello" || r.data()["views"] != 4.0 || r.data()["publishedAt"] == nil {
		t.Fatalf("partial update + publish: %v", r.data())
	}

	expect(t, e.do("GET", fmt.Sprintf("/api/site-a/articles/%v", id), tok, nil), 200)
	expect(t, e.do("DELETE", fmt.Sprintf("/api/site-a/articles/%v", id), tok, nil), 204)
	expect(t, e.do("GET", fmt.Sprintf("/api/site-a/articles/%v", id), tok, nil), 404)

	expect(t, e.do("POST", "/api/site-a/articles", tok, map[string]any{"title": "no envelope"}), 400)
	expect(t, e.do("GET", "/api/site-a/nothing", tok, nil), 404)
	expect(t, e.do("GET", "/api/no-such-site/articles", tok, nil), 404)
	expect(t, e.do("GET", "/api/site-a/articles/abc", tok, nil), 404)
}

func TestPublicAccessAndDrafts(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ct, _ := e.types.Get(ctx, e.siteA.ID, "article")
	pub, _ := e.engine.Create(ctx, ct, map[string]any{"title": "Live"}, content.WriteOptions{Status: content.StatusPublished})
	draft, _ := e.engine.Create(ctx, ct, map[string]any{"title": "Draft"}, content.WriteOptions{})

	expect(t, e.do("GET", "/api/site-a/articles", "", nil), 403) // nothing is public by default

	if err := e.tokens.SetPublicPermissions(ctx, e.siteA.ID, []string{"find:article", "findOne:article"}); err != nil {
		t.Fatal(err)
	}
	r := e.do("GET", "/api/site-a/articles", "", nil)
	expect(t, r, 200)
	if len(r.list()) != 1 || r.list()[0].(map[string]any)["title"] != "Live" {
		t.Fatalf("public list must contain only published entries: %v", r.list())
	}
	meta := r.Body["meta"].(map[string]any)["pagination"].(map[string]any)
	if meta["total"] != 1.0 || meta["page"] != 1.0 || meta["pageSize"] != 25.0 {
		t.Fatalf("pagination meta: %v", meta)
	}
	expect(t, e.do("GET", fmt.Sprintf("/api/site-a/articles/%d", pub.ID), "", nil), 200)
	expect(t, e.do("GET", fmt.Sprintf("/api/site-a/articles/%d", draft.ID), "", nil), 404)
	expect(t, e.do("GET", "/api/site-a/articles?status=draft", "", nil), 403)
	expect(t, e.do("POST", "/api/site-a/articles", "", map[string]any{"data": map[string]any{"title": "x"}}), 403)

	// Public permissions are per site.
	expect(t, e.do("GET", "/api/site-b/articles", "", nil), 403)

	ro := e.token(e.siteA, apitokens.ReadOnly)
	r = e.do("GET", "/api/site-a/articles?status=draft", ro, nil)
	expect(t, r, 200)
	if len(r.list()) != 1 || r.list()[0].(map[string]any)["title"] != "Draft" {
		t.Fatalf("drafts with token: %v", r.list())
	}
	expect(t, e.do("POST", "/api/site-a/articles", ro, map[string]any{"data": map[string]any{"title": "x"}}), 403)
}

func TestTokensAreBoundToTheirSite(t *testing.T) {
	e := setup(t)
	tokA := e.token(e.siteA, apitokens.FullAccess)
	r := e.do("GET", "/api/site-b/articles", tokA, nil)
	expect(t, r, 401)
	expect(t, e.do("GET", "/api/site-a/articles", "mino_invalid", nil), 401)
	expect(t, e.do("GET", "/api/site-a/articles", "", nil), 403)
}

func TestCustomTokenPermissions(t *testing.T) {
	e := setup(t)
	tok := e.token(e.siteA, apitokens.Custom, "find:article", "create:article")
	expect(t, e.do("GET", "/api/site-a/articles", tok, nil), 200)
	expect(t, e.do("POST", "/api/site-a/articles", tok, map[string]any{"data": map[string]any{"title": "x"}}), 201)
	expect(t, e.do("GET", "/api/site-a/articles/1", tok, nil), 403) // findOne not granted
	expect(t, e.do("GET", "/api/site-a/authors", tok, nil), 403)
}

func TestPopulateRespectsPermissionsAndPrivateFields(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	authorT, _ := e.types.Get(ctx, e.siteA.ID, "author")
	articleT, _ := e.types.Get(ctx, e.siteA.ID, "article")
	a, _ := e.engine.Create(ctx, authorT, map[string]any{"name": "Ann", "email": "ann@example.com"}, content.WriteOptions{})
	_, _ = e.engine.Create(ctx, articleT, map[string]any{"title": "T", "author": float64(a.ID)}, content.WriteOptions{Status: content.StatusPublished})
	_ = e.tokens.SetPublicPermissions(ctx, e.siteA.ID, []string{"find:article"})

	r := e.do("GET", "/api/site-a/articles?populate=author", "", nil)
	expect(t, r, 403)
	if !strings.Contains(r.errMsg(), "author") {
		t.Fatalf("error should name the field: %s", r.errMsg())
	}

	_ = e.tokens.SetPublicPermissions(ctx, e.siteA.ID, []string{"find:article", "find:author"})
	r = e.do("GET", "/api/site-a/articles?populate[0]=author&fields[0]=title", "", nil)
	expect(t, r, 200)
	item := r.list()[0].(map[string]any)
	author := item["author"].(map[string]any)
	if author["name"] != "Ann" {
		t.Fatalf("populated author: %v", author)
	}
	if _, leaked := author["email"]; leaked {
		t.Fatal("private field of a populated relation exposed")
	}
	if _, ok := item["views"]; ok {
		t.Fatal("fields selection ignored")
	}

	r = e.do("GET", "/api/site-a/authors", e.token(e.siteA, apitokens.ReadOnly), nil)
	if _, leaked := r.list()[0].(map[string]any)["email"]; leaked {
		t.Fatal("private field exposed to token")
	}
}

func TestFiltersAndSortingOverHTTP(t *testing.T) {
	e := setup(t)
	tok := e.token(e.siteA, apitokens.FullAccess)
	for i, title := range []string{"Banana", "apple", "Cherry"} {
		expect(t, e.do("POST", "/api/site-a/articles?status=published", tok, map[string]any{"data": map[string]any{"title": title, "views": i * 10}}), 201)
	}
	r := e.do("GET", "/api/site-a/articles?sort=title:asc&filters[views][$gte]=10", tok, nil)
	expect(t, r, 200)
	if got := fmt.Sprintf("%v %v", r.list()[0].(map[string]any)["title"], r.list()[1].(map[string]any)["title"]); got != "apple Cherry" {
		t.Fatalf("order: %s", got)
	}
	r = e.do("GET", "/api/site-a/articles?filters[views][$gt]=abc", tok, nil)
	expect(t, r, 400)
	expect(t, e.do("GET", "/api/site-a/articles?filters[nope][$eq]=1", tok, nil), 400)
	r = e.do("GET", "/api/site-a/articles?pagination[pageSize]=1&pagination[page]=2&sort=views", tok, nil)
	if len(r.list()) != 1 || r.list()[0].(map[string]any)["title"] != "apple" {
		t.Fatalf("pagination: %v", r.list())
	}
}

func TestSingleType(t *testing.T) {
	e := setup(t)
	tok := e.token(e.siteA, apitokens.FullAccess)
	expect(t, e.do("GET", "/api/site-a/homepage", tok, nil), 404)
	expect(t, e.do("PUT", "/api/site-a/homepage", tok, map[string]any{"data": map[string]any{"headline": "Hi"}}), 200)
	r := e.do("PUT", "/api/site-a/homepage", tok, map[string]any{"data": map[string]any{"headline": "Hello"}})
	expect(t, r, 200)
	r = e.do("GET", "/api/site-a/homepage", tok, nil)
	if r.data()["headline"] != "Hello" {
		t.Fatalf("single: %v", r.data())
	}
	expect(t, e.do("POST", "/api/site-a/homepage", tok, map[string]any{"data": map[string]any{}}), 404)
	expect(t, e.do("PUT", "/api/site-a/articles", tok, map[string]any{"data": map[string]any{}}), 405)
	expect(t, e.do("DELETE", "/api/site-a/homepage", tok, nil), 204)
	expect(t, e.do("GET", "/api/site-a/homepage", tok, nil), 404)
	// Site B's homepage is independent.
	expect(t, e.do("GET", "/api/site-b/homepage", e.token(e.siteB, apitokens.FullAccess), nil), 404)
}

func upload(t *testing.T, e *env, site, token, name string, body []byte) response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("files", name)
	_, _ = fw.Write(body)
	_ = mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/"+site+"/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := response{Status: resp.StatusCode}
	_ = json.NewDecoder(resp.Body).Decode(&out.Body)
	return out
}

func TestUploadAndMediaFields(t *testing.T) {
	e := setup(t)
	tok := e.token(e.siteA, apitokens.FullAccess)
	var img bytes.Buffer
	_ = png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 3, 2)))

	expect(t, upload(t, e, "site-a", e.token(e.siteA, apitokens.ReadOnly), "a.png", img.Bytes()), 403)
	expect(t, upload(t, e, "site-a", tok, "x.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)), 400)
	expect(t, upload(t, e, "site-a", tok, "big.txt", bytes.Repeat([]byte("a"), 2<<20)), 413)

	r := upload(t, e, "site-a", tok, "a.png", img.Bytes())
	expect(t, r, 201)
	file := r.list()[0].(map[string]any)
	if file["width"] != 3.0 || file["mime"] != "image/png" || !strings.HasPrefix(fmt.Sprint(file["url"]), "/uploads/") {
		t.Fatalf("uploaded file: %v", file)
	}

	r = e.do("POST", "/api/site-a/articles?populate=cover", tok, map[string]any{"data": map[string]any{"title": "With image", "cover": file["id"]}})
	expect(t, r, 201)
	if cover := r.data()["cover"].(map[string]any); cover["id"] != file["id"] {
		t.Fatalf("cover: %v", cover)
	}

	r = e.do("GET", "/api/site-a/upload/files", tok, nil)
	expect(t, r, 200)
	if len(r.list()) != 1 {
		t.Fatalf("files: %v", r.list())
	}
	// Another site's token cannot see or use this file.
	tokB := e.token(e.siteB, apitokens.FullAccess)
	expect(t, e.do("GET", fmt.Sprintf("/api/site-b/upload/files/%v", file["id"]), tokB, nil), 404)
	expect(t, e.do("POST", "/api/site-b/articles", tokB, map[string]any{"data": map[string]any{"title": "x", "cover": file["id"]}}), 400)

	expect(t, e.do("DELETE", fmt.Sprintf("/api/site-a/upload/files/%v", file["id"]), tok, nil), 204)
	expect(t, e.do("GET", fmt.Sprintf("/api/site-a/upload/files/%v", file["id"]), tok, nil), 404)
}

func TestCORS(t *testing.T) {
	e := setup(t)
	req, _ := http.NewRequest("OPTIONS", e.srv.URL+"/api/site-a/articles", nil)
	req.Header.Set("Origin", "https://www.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
}
