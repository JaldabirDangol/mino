// Package admin serves the server-rendered admin panel at /admin. It is a client of the same
// services the REST API uses; it holds no content logic of its own.
package admin

import (
	"context"
	"crypto/subtle"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"mino/internal/apitokens"
	"mino/internal/auth"
	"mino/internal/content"
	"mino/internal/media"
	"mino/internal/schema"
	"mino/internal/server"
	"mino/internal/settings"
	"mino/internal/sites"
	"mino/internal/users"
)

// Handler serves the admin panel.
type Handler struct {
	Sites    *sites.Service
	Schemas  *schema.Service
	Engine   *content.Engine
	Media    *media.Service
	Users    *users.Service
	Sessions *auth.Sessions
	Tokens   *apitokens.Service
	Settings *settings.Service
	Log      *slog.Logger

	SecureCookies bool
	SessionMaxAge time.Duration
	PublicURL     string // used to show absolute API URLs; empty = same origin
	MediaOrigin   string // origin serving media files when not same-origin (S3/CDN), for the CSP
	Dev           bool

	render       *renderer
	loginByIP    *auth.Limiter
	loginByEmail *auth.Limiter
}

const sessionCookie = "mino_session"
const flashCookie = "mino_flash"

type ctxKey int

const (
	userKey ctxKey = iota
	sessionKey
	tokenKey
	siteKey
	accessKey
)

// Routes builds the router mounted at /admin.
func (h *Handler) Routes() (http.Handler, error) {
	var err error
	if h.render, err = newRenderer(h.Dev); err != nil {
		return nil, err
	}
	h.loginByIP = auth.NewLimiter(20, 10*time.Minute)
	h.loginByEmail = auth.NewLimiter(8, 10*time.Minute)

	r := chi.NewRouter()
	r.Use(h.securityPolicy, h.loadSession, h.checkOrigin)

	static, _ := fs.Sub(h.render.fsys, "static")
	r.Handle("/static/*", http.StripPrefix("/admin/static/", cacheStatic(http.FileServerFS(static), h.Dev)))
	r.Get("/login", h.loginPage)
	r.Post("/login", h.login)
	r.Get("/setup", h.setupPage)
	r.Post("/setup", h.setup)

	r.Group(func(r chi.Router) {
		r.Use(h.requireUser, h.checkCSRF)
		r.Post("/logout", h.logout)
		r.Get("/", h.home)
		r.Get("/account", h.accountPage)
		r.Post("/account", h.saveAccount)
		r.Post("/account/password", h.changePassword)

		r.Group(func(r chi.Router) {
			r.Use(h.requireSuper)
			r.Get("/sites", h.sitesPage)
			r.Get("/sites/new", h.siteForm)
			r.Post("/sites/new", h.saveSite)
			r.Get("/sites/{id}", h.siteForm)
			r.Post("/sites/{id}", h.saveSite)
			r.Post("/sites/{id}/delete", h.deleteSite)
			r.Get("/users", h.usersPage)
			r.Get("/users/new", h.userForm)
			r.Post("/users/new", h.saveUser)
			r.Get("/users/{id}", h.userForm)
			r.Post("/users/{id}", h.saveUser)
			r.Post("/users/{id}/delete", h.deleteUser)
			r.Get("/roles", h.rolesPage)
			r.Get("/roles/new", h.roleForm)
			r.Post("/roles/new", h.saveRole)
			r.Get("/roles/{id}", h.roleForm)
			r.Post("/roles/{id}", h.saveRole)
			r.Post("/roles/{id}/delete", h.deleteRole)
			r.Get("/settings", h.generalSettings)
			r.Post("/settings", h.saveGeneralSettings)
		})

		r.Route("/s/{site}", func(r chi.Router) {
			r.Use(h.siteAccess)
			r.Get("/", h.dashboard)

			r.Get("/content", h.contentHome)
			r.Get("/content/{uid}", h.contentIndex)
			r.Get("/content/{uid}/new", h.entryNew)
			r.Post("/content/{uid}/new", h.entryCreate)
			r.Get("/content/{uid}/options", h.relationOptions)
			r.Get("/content/{uid}/{id}", h.entryEdit)
			r.Post("/content/{uid}/{id}", h.entryUpdate)
			r.Get("/content/{uid}/{id}/preview", h.entryPreview)
			r.Post("/content/{uid}/{id}/delete", h.entryDelete)
			r.Post("/content/{uid}/{id}/duplicate", h.entryDuplicate)
			r.Post("/content/{uid}/{id}/publish", h.entryPublish)
			r.Post("/content/{uid}/{id}/unpublish", h.entryUnpublish)
			r.Post("/content/{uid}", h.singleSave) // single types

			r.Get("/builder", h.builderHome)
			r.Post("/builder/new", h.builderCreate)
			r.Get("/builder/{uid}", h.builderEdit)
			r.Post("/builder/{uid}", h.builderSave)
			r.Post("/builder/{uid}/delete", h.builderDelete)

			r.Get("/media", h.mediaPage)
			r.Get("/media/picker", h.mediaPicker)
			r.Post("/media/upload", h.mediaUpload)
			r.Get("/media/{id}", h.mediaDetail)
			r.Post("/media/{id}", h.mediaUpdate)
			r.Post("/media/{id}/delete", h.mediaDelete)

			r.Get("/tokens", h.tokensPage)
			r.Get("/tokens/new", h.tokenForm)
			r.Post("/tokens/new", h.saveToken)
			r.Get("/tokens/{id}", h.tokenForm)
			r.Post("/tokens/{id}", h.saveToken)
			r.Post("/tokens/{id}/regenerate", h.regenerateToken)
			r.Post("/tokens/{id}/delete", h.deleteToken)
			r.Get("/api-access", h.apiAccessPage)
			r.Post("/api-access", h.saveAPIAccess)
		})
	})
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { h.notFound(w, r) })
	return r, nil
}

// securityPolicy sets a Content-Security-Policy for admin pages. Scripts only load from this
// origin, so injected markup cannot run inline scripts or event handlers. Alpine.js evaluates
// its directive expressions with Function(), which requires 'unsafe-eval'.
func (h *Handler) securityPolicy(next http.Handler) http.Handler {
	media := "'self' data: blob:"
	if h.MediaOrigin != "" {
		media += " " + h.MediaOrigin
	}
	csp := "default-src 'self'; script-src 'self' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; " +
		"img-src " + media + "; media-src " + media + "; font-src 'self'; connect-src 'self'; " +
		"object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}

func cacheStatic(next http.Handler, dev bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !dev && r.URL.RawQuery != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		next.ServeHTTP(w, r)
	})
}

// --- middleware --------------------------------------------------------------

func (h *Handler) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" || strings.HasPrefix(r.URL.Path, "/admin/static/") {
			next.ServeHTTP(w, r)
			return
		}
		sess, err := h.Sessions.Lookup(r.Context(), c.Value)
		if err != nil {
			if !errors.Is(err, auth.ErrNoSession) {
				h.Log.Error("session lookup", "err", err)
			}
			h.clearCookie(w, sessionCookie)
			next.ServeHTTP(w, r)
			return
		}
		u, err := h.Users.Get(r.Context(), sess.UserID)
		if err != nil || !u.Active {
			next.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), userKey, u)
		ctx = context.WithValue(ctx, sessionKey, sess)
		ctx = context.WithValue(ctx, tokenKey, c.Value)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// checkOrigin rejects cross-origin form posts. Together with SameSite=Lax cookies and CSRF
// tokens this blocks cross-site request forgery, including on the login form.
func (h *Handler) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request blocked", http.StatusForbidden)
					return
				}
			} else if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "cross-origin request blocked", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r) == nil {
			if n, err := h.Users.Count(r.Context()); err == nil && n == 0 {
				h.redirect(w, r, "/admin/setup")
				return
			}
			target := "/admin/login"
			if r.Method == http.MethodGet && r.URL.Path != "/admin/" {
				target += "?next=" + url.QueryEscape(r.URL.RequestURI())
			}
			h.redirect(w, r, target)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) checkCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		token := r.Header.Get("X-CSRF-Token")
		if token == "" && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			token = r.PostFormValue("_csrf")
		}
		sess := r.Context().Value(sessionKey).(*auth.Session)
		if subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRFToken)) != 1 {
			h.Log.Warn("csrf token mismatch", "path", r.URL.Path, "ip", server.ClientIP(r))
			http.Error(w, "Your session form token is missing or expired. Reload the page and try again.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) requireSuper(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !currentUser(r).SuperAdmin {
			h.forbidden(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// siteAccess resolves the site in the URL and the user's permissions on it. Users who are
// not members get 404, so site slugs are not disclosed.
func (h *Handler) siteAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site, err := h.Sites.Get(r.Context(), chi.URLParam(r, "site"))
		if errors.Is(err, sites.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		acc, err := h.Users.SiteAccess(r.Context(), currentUser(r), site.ID)
		if errors.Is(err, users.ErrNoAccess) {
			h.notFound(w, r)
			return
		}
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		ctx := context.WithValue(r.Context(), siteKey, site)
		ctx = context.WithValue(ctx, accessKey, acc)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// --- request helpers ---------------------------------------------------------

func currentUser(r *http.Request) *users.User {
	u, _ := r.Context().Value(userKey).(*users.User)
	return u
}

func currentSite(r *http.Request) *sites.Site {
	s, _ := r.Context().Value(siteKey).(*sites.Site)
	return s
}

func currentAccess(r *http.Request) *users.Access {
	a, _ := r.Context().Value(accessKey).(*users.Access)
	return a
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func sitePath(r *http.Request, parts ...string) string {
	return "/admin/s/" + currentSite(r).Slug + strings.Join(parts, "")
}

// view assembles the common page data.
func (h *Handler) view(r *http.Request, title, nav string, data any) *View {
	v := &View{Title: title, Nav: nav, Data: data, Path: r.URL.Path, AppName: h.Settings.General(r.Context()).SiteName}
	v.User = currentUser(r)
	if sess, ok := r.Context().Value(sessionKey).(*auth.Session); ok {
		v.CSRF = sess.CSRFToken
	}
	if v.User != nil {
		var err error
		if v.User.SuperAdmin {
			v.Sites, err = h.Sites.List(r.Context())
		} else {
			v.Sites, err = h.Sites.ListForMember(r.Context(), v.User.ID)
		}
		if err != nil {
			h.Log.Error("list sites", "err", err)
		}
	}
	if s := currentSite(r); s != nil {
		v.Site = s
		v.Access = currentAccess(r)
		v.APIBase = h.PublicURL + "/api/" + s.Slug
		if types, err := h.Schemas.List(r.Context(), s.ID); err == nil {
			for _, ct := range types {
				if v.Access.CanContent(users.ContentRead, ct) {
					v.Types = append(v.Types, ct)
				}
			}
		}
	}
	return v
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request, status int, page string, v *View) {
	if c, err := r.Cookie(flashCookie); err == nil {
		v.Flash = decodeFlash(c.Value)
		h.clearCookie(w, flashCookie)
	}
	layout := "layout"
	if v.User == nil {
		layout = "bare"
	}
	if err := h.render.execute(w, status, page, layout, v); err != nil {
		h.Log.Error("render", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// partial renders a named template of a page without the layout (for HTMX swaps).
func (h *Handler) partial(w http.ResponseWriter, status int, page, name string, v *View) {
	if err := h.render.execute(w, status, page, name, v); err != nil {
		h.Log.Error("render partial", "page", page, "name", name, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (h *Handler) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/admin", MaxAge: maxAge,
		HttpOnly: true, Secure: h.SecureCookies, SameSite: http.SameSiteLaxMode})
}

func (h *Handler) clearCookie(w http.ResponseWriter, name string) { h.setCookie(w, name, "", -1) }

// redirect sends the browser elsewhere; HTMX requests get HX-Location so the swap becomes navigation.
func (h *Handler) redirect(w http.ResponseWriter, r *http.Request, to string) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// done redirects with a toast message.
func (h *Handler) done(w http.ResponseWriter, r *http.Request, to, kind, msg string) {
	h.setCookie(w, flashCookie, encodeFlash(Flash{Kind: kind, Message: msg}), 60)
	h.redirect(w, r, to)
}

// toast triggers a client-side toast on HTMX responses without navigation.
func toast(w http.ResponseWriter, kind, msg string) {
	b, _ := jsonMarshal(map[string]any{"toast": map[string]string{"kind": kind, "message": msg}})
	w.Header().Set("HX-Trigger", string(b))
}

func (h *Handler) errorPage(w http.ResponseWriter, r *http.Request, status int, title, msg string) {
	if isHTMX(r) {
		toast(w, "error", msg)
		w.WriteHeader(status)
		return
	}
	v := h.view(r, title, "", map[string]any{"Status": status, "Message": msg})
	h.page(w, r, status, "error", v)
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.errorPage(w, r, http.StatusNotFound, "Not found", "This page does not exist, or you do not have access to it.")
}

func (h *Handler) forbidden(w http.ResponseWriter, r *http.Request) {
	h.errorPage(w, r, http.StatusForbidden, "Not allowed", "Your role does not allow this action. Ask a super admin if you need access.")
}

func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, err error) {
	h.Log.Error("admin error", "method", r.Method, "path", r.URL.Path, "err", err)
	h.errorPage(w, r, http.StatusInternalServerError, "Something went wrong", "The server could not complete the request. The error has been logged.")
}

// formErrors flattens a validation error into field -> message for templates.
func formErrors(err error) (map[string]string, string, bool) {
	var ve *schema.ValidationError
	if !errors.As(err, &ve) {
		return nil, "", false
	}
	out := map[string]string{}
	for _, fe := range ve.Errors {
		key := strings.Join(fe.Path, ".")
		if _, exists := out[key]; !exists {
			out[key] = fe.Message
		}
	}
	msg := ve.Message
	if len(ve.Errors) == 0 {
		msg = ve.Error()
	}
	return out, msg, true
}
