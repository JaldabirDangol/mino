package admin

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"mino/internal/server"
	"mino/internal/sites"
	"mino/internal/users"
)

type loginData struct {
	Email  string
	Next   string
	Error  string
	Errors map[string]string
}

// safeNext only allows redirects within the admin panel.
func safeNext(next string) string {
	if strings.HasPrefix(next, "/admin/") && !strings.HasPrefix(next, "//") && !strings.Contains(next, "\\") {
		return next
	}
	return "/admin/"
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		h.redirect(w, r, "/admin/")
		return
	}
	if n, err := h.Users.Count(r.Context()); err == nil && n == 0 {
		h.redirect(w, r, "/admin/setup")
		return
	}
	h.page(w, r, http.StatusOK, "login", h.view(r, "Log in", "", loginData{Next: r.URL.Query().Get("next")}))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.PostFormValue("email"))
	data := loginData{Email: email, Next: r.PostFormValue("next")}
	ip := server.ClientIP(r)
	okIP, retryIP := h.loginByIP.Allow(ip)
	okEmail, retryEmail := h.loginByEmail.Allow(strings.ToLower(email))
	if !okIP || !okEmail {
		wait := max(retryIP, retryEmail)
		data.Error = fmt.Sprintf("Too many login attempts. Try again in %d minutes.", int(wait.Minutes())+1)
		h.Log.Warn("login rate limited", "ip", ip, "email", email)
		h.page(w, r, http.StatusTooManyRequests, "login", h.view(r, "Log in", "", data))
		return
	}
	u, err := h.Users.Authenticate(r.Context(), email, r.PostFormValue("password"))
	if errors.Is(err, users.ErrInvalidCredentials) {
		data.Error = "That email and password combination is not correct."
		h.page(w, r, http.StatusUnauthorized, "login", h.view(r, "Log in", "", data))
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.loginByEmail.Reset(strings.ToLower(email))
	if err := h.startSession(w, r, u.ID); err != nil {
		h.serverError(w, r, err)
		return
	}
	h.Log.Info("admin login", "user", u.ID, "ip", ip)
	h.redirect(w, r, safeNext(data.Next))
}

func (h *Handler) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	token, _, err := h.Sessions.Create(r.Context(), userID, server.ClientIP(r), r.UserAgent())
	if err != nil {
		return err
	}
	h.setCookie(w, sessionCookie, token, int(h.SessionMaxAge.Seconds()))
	return nil
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if token, ok := r.Context().Value(tokenKey).(string); ok {
		if err := h.Sessions.Delete(r.Context(), token); err != nil {
			h.Log.Error("logout", "err", err)
		}
	}
	h.clearCookie(w, sessionCookie)
	h.redirect(w, r, "/admin/login")
}

type setupData struct {
	Name, Email, SiteName, SiteSlug string
	Errors                          map[string]string
	Error                           string
}

func (h *Handler) setupPage(w http.ResponseWriter, r *http.Request) {
	if n, err := h.Users.Count(r.Context()); err != nil || n > 0 {
		h.redirect(w, r, "/admin/login")
		return
	}
	h.page(w, r, http.StatusOK, "setup", h.view(r, "Set up Mino", "", setupData{}))
}

// setup creates the first super admin and, optionally, the first site.
func (h *Handler) setup(w http.ResponseWriter, r *http.Request) {
	if n, err := h.Users.Count(r.Context()); err != nil || n > 0 {
		h.redirect(w, r, "/admin/login")
		return
	}
	d := setupData{Name: r.PostFormValue("name"), Email: r.PostFormValue("email"),
		SiteName: r.PostFormValue("siteName"), SiteSlug: r.PostFormValue("siteSlug")}
	d.Errors = map[string]string{}
	if r.PostFormValue("password") != r.PostFormValue("confirm") {
		d.Errors["confirm"] = "Passwords do not match"
	}
	site := &sites.Site{Name: d.SiteName, Slug: d.SiteSlug}
	if err := site.Validate(); err != nil {
		errs, _, _ := formErrors(err)
		for k, v := range errs {
			d.Errors["site."+k] = v
		}
	}
	if len(d.Errors) == 0 {
		u, err := h.Users.CreateFirst(r.Context(), users.Input{Name: d.Name, Email: d.Email, Password: r.PostFormValue("password")})
		if errs, _, ok := formErrors(err); ok {
			d.Errors = errs
		} else if err != nil {
			h.serverError(w, r, err)
			return
		} else {
			if _, err := h.Sites.Create(r.Context(), site); err != nil {
				h.Log.Error("setup: create first site", "err", err)
			}
			if err := h.startSession(w, r, u.ID); err != nil {
				h.serverError(w, r, err)
				return
			}
			h.done(w, r, "/admin/s/"+site.Slug+"/", "success", "Welcome to Mino. Your first site is ready.")
			return
		}
	}
	h.page(w, r, http.StatusUnprocessableEntity, "setup", h.view(r, "Set up Mino", "", d))
}

// home sends users to their first site, or to site management.
func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	v := h.view(r, "Home", "", nil)
	if len(v.Sites) > 0 {
		h.redirect(w, r, "/admin/s/"+v.Sites[0].Slug+"/")
		return
	}
	if v.User.SuperAdmin {
		h.done(w, r, "/admin/sites/new", "info", "Create your first site to start adding content.")
		return
	}
	h.errorPage(w, r, http.StatusForbidden, "No sites yet", "You are not assigned to any site. Ask a super admin to give you access.")
}

type accountData struct {
	Name, Email string
	Memberships []users.Membership
	Errors      map[string]string
}

func (h *Handler) accountPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	ms, err := h.Users.Memberships(r.Context(), u.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.page(w, r, http.StatusOK, "account", h.view(r, "Your account", "account", accountData{Name: u.Name, Email: u.Email, Memberships: ms}))
}

func (h *Handler) saveAccount(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	in := users.Input{Name: r.PostFormValue("name"), Email: r.PostFormValue("email"), SuperAdmin: u.SuperAdmin, Active: true}
	if _, err := h.Users.Update(r.Context(), u.ID, in); err != nil {
		errs, _, ok := formErrors(err)
		if !ok {
			h.serverError(w, r, err)
			return
		}
		h.page(w, r, http.StatusUnprocessableEntity, "account", h.view(r, "Your account", "account", accountData{Name: in.Name, Email: in.Email, Errors: errs}))
		return
	}
	h.done(w, r, "/admin/account", "success", "Profile saved")
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	errs := map[string]string{}
	if r.PostFormValue("password") != r.PostFormValue("confirm") {
		errs["confirm"] = "Passwords do not match"
	} else if err := h.Users.ChangePassword(r.Context(), u.ID, r.PostFormValue("currentPassword"), r.PostFormValue("password")); err != nil {
		var ok bool
		if errs, _, ok = formErrors(err); !ok {
			h.serverError(w, r, err)
			return
		}
	}
	if len(errs) > 0 {
		h.page(w, r, http.StatusUnprocessableEntity, "account", h.view(r, "Your account", "account", accountData{Name: u.Name, Email: u.Email, Errors: errs}))
		return
	}
	// Sign out other devices; keep this session.
	token, _ := r.Context().Value(tokenKey).(string)
	if err := h.Sessions.DeleteForUser(r.Context(), u.ID, token); err != nil {
		h.Log.Error("revoke sessions", "err", err)
	}
	h.done(w, r, "/admin/account", "success", "Password changed. Other sessions were signed out.")
}
