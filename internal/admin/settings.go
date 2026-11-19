package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"mino/internal/apitokens"
	"mino/internal/schema"
	"mino/internal/settings"
	"mino/internal/sites"
	"mino/internal/users"
)

// lastValue returns the last submitted value: checkboxes are paired with a hidden "false"
// input before them, so the checkbox wins when checked.
func lastValue(r *http.Request, key string) string {
	vals := r.PostForm[key]
	if len(vals) == 0 {
		return ""
	}
	return vals[len(vals)-1]
}

func idParam(r *http.Request) int64 {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id
}

// --- sites (super admin) -------------------------------------------------------------

type siteRow struct {
	Site  *sites.Site
	Stats sites.Stats
}

func (h *Handler) sitesPage(w http.ResponseWriter, r *http.Request) {
	list, err := h.Sites.List(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	rows := make([]siteRow, len(list))
	for i, s := range list {
		rows[i].Site = s
		if rows[i].Stats, err = h.Sites.Stats(r.Context(), s.ID); err != nil {
			h.serverError(w, r, err)
			return
		}
	}
	h.page(w, r, http.StatusOK, "sites", h.view(r, "Sites", "sites", map[string]any{"Rows": rows}))
}

type siteFormData struct {
	Site   *sites.Site
	Stats  sites.Stats
	IsNew  bool
	Errors map[string]string
}

func (h *Handler) siteForm(w http.ResponseWriter, r *http.Request) {
	d := siteFormData{Site: &sites.Site{}, IsNew: true}
	if id := idParam(r); id > 0 {
		s, err := h.Sites.GetByID(r.Context(), id)
		if errors.Is(err, sites.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		d.Site, d.IsNew = s, false
		d.Stats, _ = h.Sites.Stats(r.Context(), s.ID)
	}
	h.page(w, r, http.StatusOK, "site_form", h.view(r, map[bool]string{true: "New site", false: d.Site.Name}[d.IsNew], "sites", d))
}

func (h *Handler) saveSite(w http.ResponseWriter, r *http.Request) {
	in := &sites.Site{ID: idParam(r), Name: r.PostFormValue("name"), Slug: r.PostFormValue("slug"), Description: r.PostFormValue("description")}
	var out *sites.Site
	var err error
	if in.ID == 0 {
		out, err = h.Sites.Create(r.Context(), in)
	} else {
		out, err = h.Sites.Update(r.Context(), in)
	}
	if errs, _, ok := formErrors(err); ok {
		h.page(w, r, http.StatusUnprocessableEntity, "site_form", h.view(r, "Site", "sites", siteFormData{Site: in, IsNew: in.ID == 0, Errors: errs}))
		return
	}
	if errors.Is(err, sites.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if in.ID == 0 {
		h.done(w, r, "/admin/s/"+out.Slug+"/", "success", out.Name+" created")
		return
	}
	h.done(w, r, "/admin/sites", "success", out.Name+" saved")
}

func (h *Handler) deleteSite(w http.ResponseWriter, r *http.Request) {
	s, err := h.Sites.GetByID(r.Context(), idParam(r))
	if errors.Is(err, sites.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if strings.TrimSpace(r.PostFormValue("confirmSlug")) != s.Slug {
		h.done(w, r, "/admin/sites/"+strconv.FormatInt(s.ID, 10), "error", "Type the slug “"+s.Slug+"” to confirm deletion.")
		return
	}
	// Remove stored files first: if that fails, the site and its records stay intact.
	if err := h.Media.DeleteSiteFiles(r.Context(), s.ID); err != nil {
		h.serverError(w, r, err)
		return
	}
	if err := h.Sites.Delete(r.Context(), s.ID); err != nil {
		h.serverError(w, r, err)
		return
	}
	h.Log.Info("site deleted", "site", s.Slug, "by", currentUser(r).ID)
	h.done(w, r, "/admin/sites", "success", s.Name+" and all its content were deleted")
}

// --- users (super admin) ------------------------------------------------------------

func (h *Handler) usersPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(page, 1)
	list, total, err := h.Users.List(r.Context(), q.Get("q"), page, 25)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	memberships := map[int64][]users.Membership{}
	for _, u := range list {
		if memberships[u.ID], err = h.Users.Memberships(r.Context(), u.ID); err != nil {
			h.serverError(w, r, err)
			return
		}
	}
	d := map[string]any{"Users": list, "Total": total, "Page": page, "PageCount": (total + 24) / 25, "Search": q.Get("q"), "Memberships": memberships}
	v := h.view(r, "Users", "users", d)
	if isHTMX(r) && r.Header.Get("HX-Target") == "users-table" {
		h.partial(w, http.StatusOK, "users", "table", v)
		return
	}
	h.page(w, r, http.StatusOK, "users", v)
}

type userFormData struct {
	Edit       *users.User
	Input      users.Input
	IsNew      bool
	IsSelf     bool
	Sites      []*sites.Site
	Roles      []*users.Role
	RoleBySite map[int64]int64
	Errors     map[string]string
	ErrorText  string
}

func (h *Handler) userFormData(r *http.Request, d *userFormData) error {
	var err error
	if d.Sites, err = h.Sites.List(r.Context()); err != nil {
		return err
	}
	d.Roles, err = h.Users.ListRoles(r.Context())
	return err
}

func (h *Handler) userForm(w http.ResponseWriter, r *http.Request) {
	d := &userFormData{IsNew: true, Input: users.Input{Active: true}, RoleBySite: map[int64]int64{}}
	if id := idParam(r); id > 0 {
		u, err := h.Users.Get(r.Context(), id)
		if errors.Is(err, users.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		d.Edit, d.IsNew, d.IsSelf = u, false, u.ID == currentUser(r).ID
		d.Input = users.Input{Email: u.Email, Name: u.Name, SuperAdmin: u.SuperAdmin, Active: u.Active}
		ms, err := h.Users.Memberships(r.Context(), u.ID)
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		for _, m := range ms {
			d.RoleBySite[m.SiteID] = m.RoleID
		}
	}
	if err := h.userFormData(r, d); err != nil {
		h.serverError(w, r, err)
		return
	}
	h.page(w, r, http.StatusOK, "user_form", h.view(r, map[bool]string{true: "Invite user", false: d.Input.Name}[d.IsNew], "users", d))
}

func (h *Handler) saveUser(w http.ResponseWriter, r *http.Request) {
	id := idParam(r)
	in := users.Input{Email: r.PostFormValue("email"), Name: r.PostFormValue("name"), Password: r.PostFormValue("password"),
		SuperAdmin: lastValue(r, "superAdmin") == "true", Active: lastValue(r, "active") == "true"}
	if id == currentUser(r).ID {
		in.Active = true // you cannot lock yourself out
	}
	roleBySite := map[int64]int64{}
	for key, vals := range r.PostForm {
		if siteStr, ok := strings.CutPrefix(key, "role_"); ok && len(vals) > 0 && vals[0] != "" {
			siteID, err1 := strconv.ParseInt(siteStr, 10, 64)
			roleID, err2 := strconv.ParseInt(vals[0], 10, 64)
			if err1 == nil && err2 == nil {
				roleBySite[siteID] = roleID
			}
		}
	}
	var u *users.User
	var err error
	if id == 0 {
		u, err = h.Users.Create(r.Context(), in)
	} else {
		u, err = h.Users.Update(r.Context(), id, in)
	}
	if err == nil {
		err = h.Users.SetMemberships(r.Context(), u.ID, roleBySite)
	}
	if err != nil {
		d := &userFormData{IsNew: id == 0, Input: in, RoleBySite: roleBySite, IsSelf: id == currentUser(r).ID}
		if id > 0 {
			d.Edit, _ = h.Users.Get(r.Context(), id)
		}
		switch errs, msg, ok := formErrors(err); {
		case ok:
			d.Errors, d.ErrorText = errs, msg
		case errors.Is(err, users.ErrLastSuperAdmin):
			d.ErrorText = "At least one active super admin must remain."
		default:
			h.serverError(w, r, err)
			return
		}
		if err := h.userFormData(r, d); err != nil {
			h.serverError(w, r, err)
			return
		}
		h.page(w, r, http.StatusUnprocessableEntity, "user_form", h.view(r, "User", "users", d))
		return
	}
	if in.Password != "" && id > 0 {
		token, _ := r.Context().Value(tokenKey).(string)
		_ = h.Sessions.DeleteForUser(r.Context(), u.ID, map[bool]string{true: token, false: ""}[u.ID == currentUser(r).ID])
	}
	h.done(w, r, "/admin/users", "success", map[bool]string{true: u.Name + " can now log in", false: u.Name + " saved"}[id == 0])
}

func (h *Handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	id := idParam(r)
	if id == currentUser(r).ID {
		h.done(w, r, "/admin/users", "error", "You cannot delete your own account.")
		return
	}
	err := h.Users.Delete(r.Context(), id)
	switch {
	case errors.Is(err, users.ErrLastSuperAdmin):
		h.done(w, r, "/admin/users", "error", "At least one active super admin must remain.")
	case errors.Is(err, users.ErrNotFound):
		h.notFound(w, r)
	case err != nil:
		h.serverError(w, r, err)
	default:
		h.done(w, r, "/admin/users", "success", "User deleted")
	}
}

// --- roles (super admin) ------------------------------------------------------------

type siteTypes struct {
	Site  *sites.Site
	Types []*schema.ContentType
}

type roleFormData struct {
	Role           *users.Role
	IsNew          bool
	ContentActions []string
	SiteActions    []string
	Sites          []siteTypes
	Errors         map[string]string
	ErrorText      string
}

func (h *Handler) rolesPage(w http.ResponseWriter, r *http.Request) {
	roles, err := h.Users.ListRoles(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.page(w, r, http.StatusOK, "roles", h.view(r, "Roles & permissions", "roles", map[string]any{"Roles": roles}))
}

func (h *Handler) roleFormData(r *http.Request, role *users.Role, isNew bool) (*roleFormData, error) {
	d := &roleFormData{Role: role, IsNew: isNew, ContentActions: users.ContentActions, SiteActions: users.SiteActions}
	list, err := h.Sites.List(r.Context())
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		types, err := h.Schemas.List(r.Context(), s.ID)
		if err != nil {
			return nil, err
		}
		d.Sites = append(d.Sites, siteTypes{Site: s, Types: types})
	}
	return d, nil
}

func (h *Handler) roleForm(w http.ResponseWriter, r *http.Request) {
	role, isNew := &users.Role{}, true
	if id := idParam(r); id > 0 {
		var err error
		if role, err = h.Users.GetRole(r.Context(), id); errors.Is(err, users.ErrNotFound) {
			h.notFound(w, r)
			return
		} else if err != nil {
			h.serverError(w, r, err)
			return
		}
		isNew = false
	}
	d, err := h.roleFormData(r, role, isNew)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.page(w, r, http.StatusOK, "role_form", h.view(r, map[bool]string{true: "New role", false: role.Name}[isNew], "roles", d))
}

// saveRole reads checkbox names "p:<action>:<subject>" and "own:<action>:<subject>".
func (h *Handler) saveRole(w http.ResponseWriter, r *http.Request) {
	role := &users.Role{ID: idParam(r), Name: r.PostFormValue("name"), Description: r.PostFormValue("description")}
	for key := range r.PostForm {
		rest, ok := strings.CutPrefix(key, "p:")
		if !ok {
			continue
		}
		action, subject, _ := strings.Cut(rest, ":")
		role.Permissions = append(role.Permissions, users.Permission{Action: action, Subject: subject,
			OwnOnly: r.PostFormValue("own:"+rest) == "true"})
	}
	saved, err := h.Users.SaveRole(r.Context(), role)
	if err != nil {
		errs, msg, ok := formErrors(err)
		if !ok {
			if errors.Is(err, users.ErrNotFound) {
				h.notFound(w, r)
				return
			}
			h.serverError(w, r, err)
			return
		}
		d, derr := h.roleFormData(r, role, role.ID == 0)
		if derr != nil {
			h.serverError(w, r, derr)
			return
		}
		d.Errors, d.ErrorText = errs, msg
		h.page(w, r, http.StatusUnprocessableEntity, "role_form", h.view(r, "Role", "roles", d))
		return
	}
	h.done(w, r, "/admin/roles", "success", saved.Name+" saved")
}

func (h *Handler) deleteRole(w http.ResponseWriter, r *http.Request) {
	err := h.Users.DeleteRole(r.Context(), idParam(r))
	if _, msg, ok := formErrors(err); ok {
		h.done(w, r, "/admin/roles", "error", msg)
		return
	}
	if errors.Is(err, users.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.done(w, r, "/admin/roles", "success", "Role deleted")
}

// --- general settings ----------------------------------------------------------------

func (h *Handler) generalSettings(w http.ResponseWriter, r *http.Request) {
	h.page(w, r, http.StatusOK, "settings", h.view(r, "General settings", "settings", h.settingsData(h.Settings.General(r.Context()), nil)))
}

func (h *Handler) settingsData(g settings.General, errs map[string]string) map[string]any {
	return map[string]any{"General": g, "Errors": errs, "Storage": h.Media.Storage().Name(), "MaxUploadMB": h.Media.MaxSize() >> 20}
}

func (h *Handler) saveGeneralSettings(w http.ResponseWriter, r *http.Request) {
	g := settings.General{SiteName: r.PostFormValue("siteName")}
	g.DefaultPageSize, _ = strconv.Atoi(r.PostFormValue("defaultPageSize"))
	g.MaxPageSize, _ = strconv.Atoi(r.PostFormValue("maxPageSize"))
	if err := h.Settings.SaveGeneral(r.Context(), g); err != nil {
		errs, _, ok := formErrors(err)
		if !ok {
			h.serverError(w, r, err)
			return
		}
		h.page(w, r, http.StatusUnprocessableEntity, "settings", h.view(r, "General settings", "settings", h.settingsData(g, errs)))
		return
	}
	h.done(w, r, "/admin/settings", "success", "Settings saved")
}

// --- API tokens (per site) -----------------------------------------------------------

func (h *Handler) canTokens(w http.ResponseWriter, r *http.Request) bool {
	if !currentAccess(r).Can(users.TokensManage) {
		h.forbidden(w, r)
		return false
	}
	return true
}

func (h *Handler) tokensPage(w http.ResponseWriter, r *http.Request) {
	if !h.canTokens(w, r) {
		return
	}
	list, err := h.Tokens.List(r.Context(), currentSite(r).ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.page(w, r, http.StatusOK, "tokens", h.view(r, "API tokens", "tokens", map[string]any{"Tokens": list}))
}

type tokenFormData struct {
	Token  *apitokens.Token
	IsNew  bool
	Types  []*schema.ContentType
	Secret string // shown once after creation or regeneration
	Errors map[string]string
	Expiry string
}

func (h *Handler) tokenFormView(r *http.Request, d *tokenFormData) *View {
	d.Types, _ = h.Schemas.List(r.Context(), currentSite(r).ID)
	title := "New API token"
	if !d.IsNew {
		title = d.Token.Name
	}
	return h.view(r, title, "tokens", d)
}

func (h *Handler) tokenForm(w http.ResponseWriter, r *http.Request) {
	if !h.canTokens(w, r) {
		return
	}
	d := &tokenFormData{Token: &apitokens.Token{Type: apitokens.ReadOnly}, IsNew: true, Expiry: "30"}
	if id := idParam(r); id > 0 {
		t, err := h.Tokens.Get(r.Context(), currentSite(r).ID, id)
		if errors.Is(err, apitokens.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		d.Token, d.IsNew = t, false
	}
	h.page(w, r, http.StatusOK, "token_form", h.tokenFormView(r, d))
}

func (h *Handler) saveToken(w http.ResponseWriter, r *http.Request) {
	if !h.canTokens(w, r) {
		return
	}
	t := &apitokens.Token{ID: idParam(r), SiteID: currentSite(r).ID, Name: r.PostFormValue("name"),
		Description: r.PostFormValue("description"), Type: r.PostFormValue("type"), Permissions: r.PostForm["perm"]}
	d := &tokenFormData{Token: t, IsNew: t.ID == 0, Expiry: r.PostFormValue("expiry")}
	var err error
	if t.ID == 0 {
		if days, convErr := strconv.Atoi(d.Expiry); convErr == nil && days > 0 {
			exp := time.Now().Add(time.Duration(days) * 24 * time.Hour)
			t.ExpiresAt = &exp
		}
		var secret string
		secret, d.Token, err = h.Tokens.Create(r.Context(), t, currentUser(r).ID)
		if err == nil {
			d.Secret, d.IsNew = secret, false
			v := h.tokenFormView(r, d)
			v.Flash = &Flash{Kind: "success", Message: "Token created. Copy it now — it will not be shown again."}
			h.page(w, r, http.StatusOK, "token_form", v)
			return
		}
		d.Token = t
	} else {
		_, err = h.Tokens.Update(r.Context(), t)
		if err == nil {
			h.done(w, r, sitePath(r, "/tokens"), "success", t.Name+" saved")
			return
		}
	}
	if errs, _, ok := formErrors(err); ok {
		d.Errors = errs
		h.page(w, r, http.StatusUnprocessableEntity, "token_form", h.tokenFormView(r, d))
		return
	}
	if errors.Is(err, apitokens.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	h.serverError(w, r, err)
}

func (h *Handler) regenerateToken(w http.ResponseWriter, r *http.Request) {
	if !h.canTokens(w, r) {
		return
	}
	t, err := h.Tokens.Get(r.Context(), currentSite(r).ID, idParam(r))
	if errors.Is(err, apitokens.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	secret, err := h.Tokens.Regenerate(r.Context(), t.SiteID, t.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	t, _ = h.Tokens.Get(r.Context(), t.SiteID, t.ID)
	v := h.tokenFormView(r, &tokenFormData{Token: t, Secret: secret})
	v.Flash = &Flash{Kind: "success", Message: "New token generated. The old one stopped working."}
	h.page(w, r, http.StatusOK, "token_form", v)
}

func (h *Handler) deleteToken(w http.ResponseWriter, r *http.Request) {
	if !h.canTokens(w, r) {
		return
	}
	err := h.Tokens.Delete(r.Context(), currentSite(r).ID, idParam(r))
	if errors.Is(err, apitokens.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.done(w, r, sitePath(r, "/tokens"), "success", "Token revoked")
}

// --- public API access (per site) -----------------------------------------------------

func (h *Handler) apiAccessPage(w http.ResponseWriter, r *http.Request) {
	if !h.canTokens(w, r) {
		return
	}
	types, err := h.Schemas.List(r.Context(), currentSite(r).ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	perms, err := h.Tokens.PublicPermissions(r.Context(), currentSite(r).ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.page(w, r, http.StatusOK, "api_access", h.view(r, "Public API access", "api-access", map[string]any{"Types": types, "Perms": perms}))
}

func (h *Handler) saveAPIAccess(w http.ResponseWriter, r *http.Request) {
	if !h.canTokens(w, r) {
		return
	}
	if err := h.Tokens.SetPublicPermissions(r.Context(), currentSite(r).ID, r.PostForm["perm"]); err != nil {
		if _, msg, ok := formErrors(err); ok {
			h.done(w, r, sitePath(r, "/api-access"), "error", msg)
			return
		}
		h.serverError(w, r, err)
		return
	}
	h.done(w, r, sitePath(r, "/api-access"), "success", "Public access saved")
}
