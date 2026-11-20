package docs_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"

	docsfiles "mino/docs"
	"mino/internal/docs"
)

func serve(t *testing.T, h *docs.Handler) *httptest.Server {
	t.Helper()
	routes, err := h.Routes()
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Mount("/docs", routes)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

// The real embedded docs render, and every page has a title and an index.
func TestEmbeddedPages(t *testing.T) {
	srv := serve(t, &docs.Handler{Source: docsfiles.Files, AppName: "Mino"})
	for _, path := range []string{"/docs", "/docs/", "/docs/api"} {
		res, body := get(t, srv.URL+path)
		if res.StatusCode != 200 {
			t.Fatalf("%s: status %d", path, res.StatusCode)
		}
		if res.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("%s: no CSP header", path)
		}
		if !strings.Contains(body, `class="toc"`) || !strings.Contains(body, "<h2 id=") {
			t.Errorf("%s: missing table of contents or headings", path)
		}
	}
	if res, _ := get(t, srv.URL+"/docs/static/docs.css"); res.StatusCode != 200 {
		t.Errorf("docs.css: status %d", res.StatusCode)
	}
	if res, err := http.Head(srv.URL + "/docs/api"); err != nil || res.StatusCode != 200 {
		t.Errorf("HEAD /docs/api: %v %v", res, err)
	}
	if res, _ := get(t, srv.URL+"/docs/nope"); res.StatusCode != 404 {
		t.Errorf("unknown page: status %d, want 404", res.StatusCode)
	}
}

func TestRendering(t *testing.T) {
	src := fstest.MapFS{
		"GUIDE.md": {Data: []byte("# Getting started\n\nSee [populate](API.md#populate) and [elsewhere](https://example.com).\n\n" +
			"## First `step`\n\n<script>alert(1)</script>\n\n| a | b |\n|---|---|\n| 1 | 2 |\n")},
		"API.md": {Data: []byte("# Reference\n\n## Populate\n\nBack to the [guide](GUIDE.md).\n")},
	}
	srv := serve(t, &docs.Handler{Source: src, AppName: "Mino"})

	_, body := get(t, srv.URL+"/docs/")
	for _, want := range []string{
		"<title>Getting started · Mino docs</title>",
		`href="/docs/api#populate"`,
		`href="https://example.com"`,
		`<a href="#first-step">First step</a>`,
		`<div class="table-wrap"><table>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("guide: missing %q", want)
		}
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("raw HTML in markdown must not be rendered")
	}

	_, body = get(t, srv.URL+"/docs/api")
	if !strings.Contains(body, `href="/docs/"`) || !strings.Contains(body, `<h2 id="populate">`) {
		t.Error("api: link to guide or heading id missing")
	}
}

func TestMissingFileFailsAtStartup(t *testing.T) {
	h := &docs.Handler{Source: fstest.MapFS{"GUIDE.md": {Data: []byte("# Hi")}}}
	if _, err := h.Routes(); err == nil {
		t.Fatal("expected an error when API.md is missing")
	}
}
