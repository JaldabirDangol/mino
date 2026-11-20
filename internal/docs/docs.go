// Package docs serves the public API documentation at /docs, rendered from the markdown files
// in the repository's docs/ folder. Pages are rendered once at startup (or on every request
// in dev mode, so edits show on reload).
package docs

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

//go:embed templates static
var embedded embed.FS

// Page is one documentation page.
type Page struct {
	Slug  string // URL path under /docs ("" is the index)
	File  string // markdown file name in the source FS
	Label string // navigation label
}

// Pages lists the published pages in navigation order.
var Pages = []Page{
	{Slug: "", File: "GUIDE.md", Label: "Guide"},
	{Slug: "api", File: "API.md", Label: "API reference"},
}

// Handler serves /docs.
type Handler struct {
	Source  fs.FS  // markdown files (docs.Files, or os.DirFS("docs") in dev)
	AppName string // shown in the header
	Dev     bool   // re-render on every request

	site *site // rendered at startup; nil in dev mode
}

type site struct {
	tmpl  *template.Template
	views map[string]*view
}

type heading struct {
	ID, Text string
}

type view struct {
	AppName string
	Title   string
	Slug    string
	Pages   []Page
	TOC     []heading
	Body    template.HTML
}

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

// Routes returns the router mounted at /docs. It fails if a page cannot be rendered, so a
// broken doc stops the server at startup instead of at the first request.
func (h *Handler) Routes() (http.Handler, error) {
	s, err := h.build()
	if err != nil {
		return nil, err
	}
	if !h.Dev {
		h.site = s
	}
	r := chi.NewRouter()
	r.Use(middleware.GetHead, securityPolicy)
	static, _ := fs.Sub(embedded, "static")
	r.Handle("/static/*", http.StripPrefix("/docs/static/", http.FileServerFS(static)))
	for _, p := range Pages {
		r.Get("/"+p.Slug, h.serve(p.Slug))
	}
	return r, nil
}

func (h *Handler) build() (*site, error) {
	tmpl, err := template.ParseFS(embedded, "templates/page.html")
	if err != nil {
		return nil, err
	}
	s := &site{tmpl: tmpl, views: map[string]*view{}}
	for _, p := range Pages {
		v, err := h.render(p)
		if err != nil {
			return nil, err
		}
		s.views[p.Slug] = v
	}
	return s, nil
}

func (h *Handler) serve(slug string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := h.site
		if s == nil {
			var err error
			if s, err = h.build(); err != nil {
				http.Error(w, "docs: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		var buf bytes.Buffer
		if err := s.tmpl.Execute(&buf, s.views[slug]); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if !h.Dev {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		_, _ = w.Write(buf.Bytes())
	}
}

// render converts one markdown file to HTML, collecting its h1 as the title and its h2s as
// the table of contents, and rewriting links between doc files to their /docs URLs.
func (h *Handler) render(p Page) (*view, error) {
	src, err := fs.ReadFile(h.Source, p.File)
	if err != nil {
		return nil, fmt.Errorf("docs: %w", err)
	}
	doc := md.Parser().Parse(text.NewReader(src))
	v := &view{AppName: h.AppName, Slug: p.Slug, Pages: Pages, Title: p.Label}
	err = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Heading:
			id, _ := n.AttributeString("id")
			idStr, _ := id.([]byte)
			label := plainText(n, src)
			switch n.Level {
			case 1:
				v.Title = label
			case 2:
				v.TOC = append(v.TOC, heading{ID: string(idStr), Text: label})
			}
		case *ast.Link:
			n.Destination = []byte(rewriteLink(string(n.Destination)))
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, src, doc); err != nil {
		return nil, fmt.Errorf("docs: render %s: %w", p.File, err)
	}
	// Wrap tables so wide ones scroll inside their own box. goldmark escapes raw HTML in the
	// markdown, so every <table> here is one it generated.
	out := strings.NewReplacer("<table>", `<div class="table-wrap"><table>`, "</table>", "</table></div>").Replace(buf.String())
	v.Body = template.HTML(out)
	return v, nil
}

// rewriteLink maps "API.md#populate" to "/docs/api#populate" for files that are published.
func rewriteLink(dest string) string {
	file, frag, _ := strings.Cut(dest, "#")
	for _, p := range Pages {
		if file == p.File {
			out := "/docs/" + p.Slug
			if frag != "" {
				out += "#" + frag
			}
			return out
		}
	}
	return dest
}

func plainText(n ast.Node, src []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			if t, ok := c.(*ast.Text); ok {
				b.Write(t.Segment.Value(src))
			} else if s, ok := c.(*ast.String); ok {
				b.Write(s.Value)
			}
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

// securityPolicy allows only this origin's styles, fonts and scripts. The admin fonts are
// reused from /admin/static/fonts.
func securityPolicy(next http.Handler) http.Handler {
	const csp = "default-src 'none'; style-src 'self'; font-src 'self'; script-src 'self'; img-src 'self' data:; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}
