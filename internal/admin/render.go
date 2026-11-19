package admin

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"mino/internal/apitokens"
	"mino/internal/content"
	"mino/internal/schema"
	"mino/internal/sites"
	"mino/internal/users"
)

//go:embed templates static
var embedded embed.FS

// View is the data every page template receives.
type View struct {
	Title   string
	AppName string
	User    *users.User
	Site    *sites.Site
	Sites   []*sites.Site // sites the user can switch to
	Access  *users.Access
	Types   []*schema.ContentType // content types of the current site
	Nav     string                // active navigation key
	CSRF    string
	Flash   *Flash
	Path    string
	APIBase string // absolute or root-relative API base for the current site
	Data    any
}

// Flash is a one-shot message shown as a toast after a redirect.
type Flash struct {
	Kind    string `json:"k"` // success | error | info
	Message string `json:"m"`
}

type renderer struct {
	dev   bool
	fsys  fs.FS
	pages map[string]*template.Template
	// assetVersion busts browser caches when embedded assets change.
	assetVersion string
}

func newRenderer(dev bool) (*renderer, error) {
	r := &renderer{dev: dev, fsys: embedded}
	if dev {
		// Read templates and static files from the source tree so edits show without rebuilding.
		r.fsys = os.DirFS("internal/admin")
	}
	h := sha256.New()
	for _, f := range []string{"static/app.css", "static/admin.js", "static/editor.js"} {
		b, _ := fs.ReadFile(r.fsys, f)
		h.Write(b)
	}
	r.assetVersion = hex.EncodeToString(h.Sum(nil))[:10]
	return r, r.load()
}

func (r *renderer) funcs() template.FuncMap {
	return template.FuncMap{
		"icon":  icon,
		"asset": func(name string) string { return "/admin/static/" + name + "?v=" + r.assetVersion },
		"siteHue": func(slug string) int {
			h := fnv.New32a()
			h.Write([]byte(slug))
			// Spread hues and skip muddy yellow-greens for better contrast with white text.
			hue := int(h.Sum32() % 300)
			if hue > 80 {
				hue += 60
			}
			return hue
		},
		"initial": func(s string) string {
			for _, r := range s {
				return strings.ToUpper(string(r))
			}
			return "?"
		},
		"ago":  ago,
		"date": func(t time.Time) string { return t.Local().Format("2 Jan 2006, 15:04") },
		"dateptr": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.Local().Format("2 Jan 2006, 15:04")
		},
		"bytes":       humanBytes,
		"json":        prettyJSON,
		"jsonAttr":    func(v any) (string, error) { b, err := json.Marshal(v); return string(b), err },
		"typeLabel":   func(t schema.FieldType) string { return t.Label() },
		"typeGlyph":   typeGlyph,
		"actionLabel": users.ActionLabel,
		"perm":        apitokens.Perm,
		"subject":     schema.PermissionSubject,
		"contains":    func(list []string, s string) bool { return slices.Contains(list, s) },
		"add":         func(a, b int) int { return a + b },
		"sub":         func(a, b int) int { return a - b },
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, fmt.Errorf("dict needs key/value pairs")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m, nil
		},
		"title": func(e *content.Entry) string { return e.Title() },
		"cell":  cell,
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return one
			}
			return many
		},
		"lower":     strings.ToLower,
		"roleHas":   func(r *users.Role, action, subject string) bool { ok, _ := r.Has(action, subject); return ok },
		"roleOwn":   func(r *users.Role, action, subject string) bool { _, own := r.Has(action, subject); return own },
		"list":      func(xs ...string) []string { return xs },
		"hasPrefix": strings.HasPrefix,
	}
}

func (r *renderer) load() error {
	base, err := template.New("").Funcs(r.funcs()).ParseFS(r.fsys, "templates/layout.html", "templates/partials.html")
	if err != nil {
		return err
	}
	files, err := fs.Glob(r.fsys, "templates/pages/*.html")
	if err != nil {
		return err
	}
	pages := map[string]*template.Template{}
	for _, f := range files {
		t, err := base.Clone()
		if err != nil {
			return err
		}
		if _, err := t.ParseFS(r.fsys, f); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		pages[strings.TrimSuffix(path.Base(f), ".html")] = t
	}
	r.pages = pages
	return nil
}

// execute renders template name from page into w, buffering so errors never produce half pages.
func (r *renderer) execute(w http.ResponseWriter, status int, page, name string, data any) error {
	if r.dev {
		if err := r.load(); err != nil {
			return err
		}
	}
	t, ok := r.pages[page]
	if !ok {
		return fmt.Errorf("unknown page %q", page)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, err := buf.WriteTo(w)
	return err
}

// prettyJSON indents without escaping <, > and & (the template escapes output itself).
func prettyJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(v)
	return strings.TrimSpace(buf.String()), err
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	}
	return t.Local().Format("2 Jan 2006")
}

func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
}

func typeGlyph(t schema.FieldType) string {
	return map[schema.FieldType]string{
		schema.TypeString: "Aa", schema.TypeText: "¶", schema.TypeRichText: "Rt", schema.TypeNumber: "12",
		schema.TypeBoolean: "0/1", schema.TypeDate: "D", schema.TypeDateTime: "DT", schema.TypeEmail: "@",
		schema.TypeEnum: "≡", schema.TypeJSON: "{}", schema.TypeMedia: "Img", schema.TypeRelation: "↔",
	}[t]
}

// cell renders a scalar value for a list table.
func cell(f schema.Field, v any) string {
	if v == nil {
		return ""
	}
	switch f.Type {
	case schema.TypeBoolean:
		if b, _ := v.(bool); b {
			return "Yes"
		}
		return "No"
	case schema.TypeRichText:
		return truncate(stripTags(fmt.Sprint(v)), 80)
	case schema.TypeJSON:
		b, _ := json.Marshal(v)
		return truncate(string(b), 60)
	case schema.TypeDateTime:
		if t, err := time.Parse(time.RFC3339Nano, fmt.Sprint(v)); err == nil {
			return t.Local().Format("2 Jan 2006, 15:04")
		}
	case schema.TypeNumber:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%f", v), "0"), ".")
	}
	return truncate(fmt.Sprint(v), 80)
}

func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
			b.WriteByte(' ')
		case !in:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// pager is pagination ready to render. Links with an empty Href are ellipses.
type pager struct {
	Prev, Next string
	Links      []pageLink
	Target     string // HTMX target; empty for full-page navigation
	Push       bool   // update the browser URL when paging via HTMX
}

type pageLink struct {
	Num     int
	Href    string
	Current bool
}

func makePager(page, count int, href func(int) string, target string, push bool) pager {
	p := pager{Target: target, Push: push}
	if count <= 1 {
		return p
	}
	if page > 1 {
		p.Prev = href(page - 1)
	}
	if page < count {
		p.Next = href(page + 1)
	}
	for _, n := range pageWindow(page, count) {
		if n == 0 {
			p.Links = append(p.Links, pageLink{})
		} else {
			p.Links = append(p.Links, pageLink{Num: n, Href: href(n), Current: n == page})
		}
	}
	return p
}

// pageWindow returns page numbers to show, with 0 meaning an ellipsis.
func pageWindow(current, total int) []int {
	if total <= 7 {
		out := make([]int, total)
		for i := range out {
			out[i] = i + 1
		}
		return out
	}
	set := map[int]bool{1: true, total: true, current: true, current - 1: true, current + 1: true}
	var out []int
	last := 0
	for p := 1; p <= total; p++ {
		if !set[p] {
			continue
		}
		if p-last > 1 {
			out = append(out, 0)
		}
		out = append(out, p)
		last = p
	}
	return out
}

func encodeFlash(f Flash) string {
	b, _ := json.Marshal(f)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeFlash(s string) *Flash {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	var f Flash
	if json.Unmarshal(b, &f) != nil || f.Message == "" {
		return nil
	}
	return &f
}
