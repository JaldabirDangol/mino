package admin_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"mino/internal/admin"
	"mino/internal/apitokens"
	"mino/internal/auth"
	"mino/internal/content"
	"mino/internal/media"
	"mino/internal/schema"
	"mino/internal/server"
	"mino/internal/settings"
	"mino/internal/sites"
	"mino/internal/testutil"
	"mino/internal/users"
)

type env struct {
	t      *testing.T
	srv    *httptest.Server
	users  *users.Service
	sites  *sites.Service
	types  *schema.Service
	engine *content.Engine
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testutil.DB(t)
	sitesSvc := sites.NewService(pool)
	types := schema.NewService(pool)
	engine := content.New(pool, types)
	usersSvc := users.NewService(pool)
	h := &admin.Handler{Sites: sitesSvc, Schemas: types, Engine: engine,
		Media: media.NewService(pool, &media.Local{Dir: t.TempDir()}, 1<<20, testutil.Logger()),
		Users: usersSvc, Sessions: auth.NewSessions(pool, time.Hour, 24*time.Hour),
		Tokens: apitokens.NewService(pool), Settings: settings.NewService(pool), Log: testutil.Logger(),
		SessionMaxAge: 24 * time.Hour}
	routes, err := h.Routes()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(server.Options{Log: testutil.Logger(), Pool: pool, API: http.NotFoundHandler(), Admin: routes}))
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, users: usersSvc, sites: sitesSvc, types: types, engine: engine}
}

// client is a browser-like client: cookie jar, no automatic redirects.
type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: e.t, base: e.srv.URL, http: &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type resp struct {
	Status   int
	Location string
	Body     string
}

func (c *client) do(method, path string, form url.Values, headers map[string]string) resp {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, c.base+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{Status: res.StatusCode, Location: res.Header.Get("Location"), Body: string(b)}
}

func (c *client) get(path string) resp { return c.do("GET", path, nil, nil) }

var csrfRe = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

// post submits a form with the CSRF token taken from a page the user can see.
func (c *client) post(path string, form url.Values) resp {
	c.t.Helper()
	page := c.get("/admin/account")
	m := csrfRe.FindStringSubmatch(page.Body)
	if m == nil {
		c.t.Fatalf("no CSRF token on account page (status %d)", page.Status)
	}
	form.Set("_csrf", m[1])
	return c.do("POST", path, form, nil)
}

func (c *client) login(email, password string) resp {
	return c.do("POST", "/admin/login", url.Values{"email": {email}, "password": {password}}, nil)
}

func expect(t *testing.T, r resp, status int) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status %d, want %d (location %q)\n%s", r.Status, status, r.Location, truncate(r.Body))
	}
}

func truncate(s string) string {
	if len(s) > 600 {
		return s[:600] + "…"
	}
	return s
}

func TestFirstRunSetup(t *testing.T) {
	e := setup(t)
	c := e.client()
	r := c.get("/admin/")
	expect(t, r, http.StatusSeeOther)
	if r.Location != "/admin/setup" {
		t.Fatalf("redirect to %q", r.Location)
	}
	expect(t, c.get("/admin/setup"), http.StatusOK)

	bad := url.Values{"name": {"Ada"}, "email": {"ada@example.com"}, "password": {"long password!"}, "confirm": {"different"},
		"siteName": {"Blog"}, "siteSlug": {"blog"}}
	r = c.do("POST", "/admin/setup", bad, nil)
	expect(t, r, http.StatusUnprocessableEntity)
	if !strings.Contains(r.Body, "Passwords do not match") {
		t.Fatal("mismatch error not shown")
	}

	good := url.Values{"name": {"Ada"}, "email": {"ada@example.com"}, "password": {"long password!"}, "confirm": {"long password!"},
		"siteName": {"Blog"}, "siteSlug": {"blog"}}
	r = c.do("POST", "/admin/setup", good, nil)
	expect(t, r, http.StatusSeeOther)
	if r.Location != "/admin/s/blog/" {
		t.Fatalf("setup redirect %q", r.Location)
	}
	expect(t, c.get("/admin/s/blog/"), http.StatusOK)

	// Setup cannot run again, even from a fresh browser.
	other := e.client()
	r = other.do("POST", "/admin/setup", good, nil)
	expect(t, r, http.StatusSeeOther)
	if n, _ := e.users.Count(context.Background()); n != 1 {
		t.Fatalf("users = %d after second setup attempt", n)
	}
}

type fixture struct {
	*env
	siteA, siteB *sites.Site
	article      *schema.ContentType
	admin        *users.User
	author       *users.User
}

func withData(t *testing.T) *fixture {
	e := setup(t)
	ctx := context.Background()
	f := &fixture{env: e}
	f.admin, _ = e.users.CreateFirst(ctx, users.Input{Name: "Admin", Email: "admin@example.com", Password: "admin password"})
	f.author, _ = e.users.Create(ctx, users.Input{Name: "Sam", Email: "sam@example.com", Password: "author password", Active: true})
	f.siteA, _ = e.sites.Create(ctx, &sites.Site{Name: "A", Slug: "site-a"})
	f.siteB, _ = e.sites.Create(ctx, &sites.Site{Name: "B", Slug: "site-b"})
	var err error
	f.article, err = e.types.Create(ctx, &schema.ContentType{SiteID: f.siteA.ID, UID: "article", PluralName: "articles",
		Kind: schema.KindCollection, DisplayName: "Article", DraftPublish: true,
		Fields: []schema.Field{{Name: "title", Type: schema.TypeString, Required: true}, {Name: "views", Type: schema.TypeNumber}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	roles, _ := e.users.ListRoles(ctx)
	for _, r := range roles {
		if r.Name == "Author" {
			_ = e.users.SetMemberships(ctx, f.author.ID, map[int64]int64{f.siteA.ID: r.ID})
		}
	}
	return f
}

func TestLoginLogoutAndRateLimit(t *testing.T) {
	f := withData(t)
	c := f.client()
	expect(t, c.get("/admin/s/site-a/"), http.StatusSeeOther) // not logged in

	r := c.login("admin@example.com", "wrong password")
	expect(t, r, http.StatusUnauthorized)
	r = c.login("admin@example.com", "admin password")
	expect(t, r, http.StatusSeeOther)
	expect(t, c.get("/admin/s/site-a/"), http.StatusOK)

	expect(t, c.post("/admin/logout", url.Values{}), http.StatusSeeOther)
	expect(t, c.get("/admin/s/site-a/"), http.StatusSeeOther)

	// Repeated failures for one account are throttled.
	attacker := f.client()
	var last resp
	for range 10 {
		last = attacker.login("sam@example.com", "guess")
	}
	expect(t, last, http.StatusTooManyRequests)
	// Even the right password is refused while throttled.
	expect(t, attacker.login("sam@example.com", "author password"), http.StatusTooManyRequests)
}

func TestLoginRedirectStaysInAdmin(t *testing.T) {
	f := withData(t)
	c := f.client()
	r := c.do("POST", "/admin/login", url.Values{"email": {"admin@example.com"}, "password": {"admin password"}, "next": {"https://evil.example/"}}, nil)
	expect(t, r, http.StatusSeeOther)
	if r.Location != "/admin/" {
		t.Fatalf("open redirect to %q", r.Location)
	}
}

func TestCSRFAndOriginChecks(t *testing.T) {
	f := withData(t)
	c := f.client()
	expect(t, c.login("admin@example.com", "admin password"), http.StatusSeeOther)
	form := url.Values{"title": {"Hello"}}

	expect(t, c.do("POST", "/admin/s/site-a/content/article/new", form, nil), http.StatusForbidden)
	form.Set("_csrf", "forged")
	expect(t, c.do("POST", "/admin/s/site-a/content/article/new", form, nil), http.StatusForbidden)

	// A valid token from a cross-origin page is still rejected.
	page := c.get("/admin/account")
	form.Set("_csrf", csrfRe.FindStringSubmatch(page.Body)[1])
	expect(t, c.do("POST", "/admin/s/site-a/content/article/new", form, map[string]string{"Origin": "https://evil.example"}), http.StatusForbidden)

	r := c.do("POST", "/admin/s/site-a/content/article/new", form, map[string]string{"Origin": f.srv.URL})
	expect(t, r, http.StatusSeeOther)
	if !strings.HasPrefix(r.Location, "/admin/s/site-a/content/article/") {
		t.Fatalf("redirect %q", r.Location)
	}
}

func TestContentFormValidationAndPublish(t *testing.T) {
	f := withData(t)
	c := f.client()
	c.login("admin@example.com", "admin password")

	r := c.post("/admin/s/site-a/content/article/new", url.Values{"title": {""}, "views": {"abc"}, "action": {"save"}})
	expect(t, r, http.StatusUnprocessableEntity)
	if !strings.Contains(r.Body, "Must be a number") {
		t.Fatal("number conversion error not shown")
	}

	r = c.post("/admin/s/site-a/content/article/new", url.Values{"views": {"3"}, "action": {"publish"}})
	expect(t, r, http.StatusUnprocessableEntity)
	if !strings.Contains(r.Body, "This field is required") {
		t.Fatal("required error not shown on publish")
	}

	r = c.post("/admin/s/site-a/content/article/new", url.Values{"title": {"Live"}, "views": {"3"}, "action": {"publish"}})
	expect(t, r, http.StatusSeeOther)
	res, _ := f.engine.List(context.Background(), f.article, content.Query{Page: 1, PageSize: 10, Status: content.StatusPublished})
	if res.Total != 1 || res.Entries[0].Data["views"] != 3.0 {
		t.Fatalf("published entries: %+v", res.Entries)
	}
}

func TestAuthorPermissions(t *testing.T) {
	f := withData(t)
	ctx := context.Background()
	adminEntry, _ := f.engine.Create(ctx, f.article, map[string]any{"title": "By admin"}, content.WriteOptions{UserID: f.admin.ID})

	c := f.client()
	expect(t, c.login("sam@example.com", "author password"), http.StatusSeeOther)

	// Navigation and global pages.
	expect(t, c.get("/admin/s/site-a/content/article"), http.StatusOK)
	expect(t, c.get("/admin/s/site-b/"), http.StatusNotFound) // not a member: the site is invisible
	expect(t, c.get("/admin/users"), http.StatusForbidden)
	expect(t, c.get("/admin/s/site-a/builder"), http.StatusForbidden)
	expect(t, c.get("/admin/s/site-a/tokens"), http.StatusForbidden)

	// Own entries: create and edit, but not publish.
	r := c.post("/admin/s/site-a/content/article/new", url.Values{"title": {"Mine"}, "action": {"save"}})
	expect(t, r, http.StatusSeeOther)
	own := r.Location[strings.LastIndex(r.Location, "/")+1:]
	expect(t, c.post("/admin/s/site-a/content/article/"+own, url.Values{"title": {"Mine, edited"}, "action": {"save"}}), http.StatusSeeOther)
	expect(t, c.post("/admin/s/site-a/content/article/"+own, url.Values{"title": {"Mine"}, "action": {"publish"}}), http.StatusForbidden)
	expect(t, c.post("/admin/s/site-a/content/article/"+own+"/publish", url.Values{}), http.StatusForbidden)

	// Someone else's entry: readable, not writable.
	other := "/admin/s/site-a/content/article/" + itoa(adminEntry.ID)
	page := c.get(other)
	expect(t, page, http.StatusOK)
	if !strings.Contains(page.Body, "Read only") {
		t.Fatal("other's entry should render read only")
	}
	expect(t, c.post(other, url.Values{"title": {"Hijacked"}, "action": {"save"}}), http.StatusForbidden)
	expect(t, c.post(other+"/delete", url.Values{}), http.StatusForbidden)
	got, _ := f.engine.Get(ctx, f.article, adminEntry.ID, content.ReadOptions{})
	if got.Data["title"] != "By admin" {
		t.Fatal("author modified another user's entry")
	}

	// Own entry can be deleted.
	expect(t, c.post("/admin/s/site-a/content/article/"+own+"/delete", url.Values{}), http.StatusSeeOther)
}

func TestDeactivatedUserLosesSession(t *testing.T) {
	f := withData(t)
	c := f.client()
	c.login("sam@example.com", "author password")
	expect(t, c.get("/admin/s/site-a/"), http.StatusOK)
	_, err := f.users.Update(context.Background(), f.author.ID, users.Input{Name: "Sam", Email: "sam@example.com", Active: false})
	if err != nil {
		t.Fatal(err)
	}
	expect(t, c.get("/admin/s/site-a/"), http.StatusSeeOther)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
