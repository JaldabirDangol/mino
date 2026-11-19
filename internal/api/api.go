// Package api serves the public REST API: /api/{site}/{type}[/{id}] and /api/{site}/upload.
// All content logic lives in the content engine; this package only translates HTTP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"mino/internal/apitokens"
	"mino/internal/content"
	"mino/internal/media"
	"mino/internal/schema"
	"mino/internal/settings"
	"mino/internal/sites"
)

// Handler holds the services the API needs.
type Handler struct {
	Sites    *sites.Service
	Schemas  *schema.Service
	Engine   *content.Engine
	Media    *media.Service
	Tokens   *apitokens.Service
	Settings *settings.Service
	Log      *slog.Logger
	// CORSOrigins lists allowed browser origins; "*" allows any. The API uses bearer tokens,
	// never cookies, so allowing any origin does not expose credentials.
	CORSOrigins []string
}

// maxBodyBytes bounds JSON request bodies.
const maxBodyBytes = 2 << 20

type ctxKey int

const (
	siteKey ctxKey = iota
	principalKey
)

// Routes returns the router to mount at /api.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(h.cors)
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "NotFoundError", "Not Found", nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowedError", "Method Not Allowed", nil)
	})
	r.Route("/{site}", func(r chi.Router) {
		r.Use(h.resolveSite)
		r.Get("/upload/files", h.listFiles)
		r.Get("/upload/files/{id}", h.getFile)
		r.Delete("/upload/files/{id}", h.deleteFile)
		r.Post("/upload", h.upload)
		r.Get("/{type}", h.find)
		r.Post("/{type}", h.create)
		r.Put("/{type}", h.updateSingle)
		r.Delete("/{type}", h.deleteSingle)
		r.Get("/{type}/{id}", h.findOne)
		r.Put("/{type}/{id}", h.update)
		r.Delete("/{type}/{id}", h.delete)
	})
	return r
}

func (h *Handler) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if slices.Contains(h.CORSOrigins, "*") {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if slices.Contains(h.CORSOrigins, origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// resolveSite loads the site and identifies the caller. A presented token must be valid and
// belong to this site; otherwise the request is rejected rather than downgraded to public.
func (h *Handler) resolveSite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		site, err := h.Sites.Get(ctx, chi.URLParam(r, "site"))
		if errors.Is(err, sites.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NotFoundError", "No site with this slug. Check the first part of the URL: /api/{site}/…", nil)
			return
		}
		if err != nil {
			h.internal(w, r, err)
			return
		}
		p := &apitokens.Principal{}
		if authz := r.Header.Get("Authorization"); authz != "" {
			secret, ok := strings.CutPrefix(authz, "Bearer ")
			tok, err := h.Tokens.Authenticate(ctx, strings.TrimSpace(secret))
			if !ok || errors.Is(err, apitokens.ErrInvalid) || (tok != nil && tok.SiteID != site.ID) {
				writeError(w, http.StatusUnauthorized, "UnauthorizedError", "The API token is invalid, expired, or belongs to another site. Send \"Authorization: Bearer <token>\" with a token created for this site.", nil)
				return
			}
			if err != nil {
				h.internal(w, r, err)
				return
			}
			p.Token = tok
		} else if p.Public, err = h.Tokens.PublicPermissions(ctx, site.ID); err != nil {
			h.internal(w, r, err)
			return
		}
		ctx = context.WithValue(ctx, siteKey, site)
		ctx = context.WithValue(ctx, principalKey, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func siteFrom(r *http.Request) *sites.Site { return r.Context().Value(siteKey).(*sites.Site) }

func principalFrom(r *http.Request) *apitokens.Principal {
	return r.Context().Value(principalKey).(*apitokens.Principal)
}

// --- responses -------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Status  int    `json:"status"`
	Name    string `json:"name"`
	Message string `json:"message"`
	Details any    `json:"details"`
}

func writeError(w http.ResponseWriter, status int, name, msg string, details any) {
	if details == nil {
		details = map[string]any{}
	}
	writeJSON(w, status, map[string]any{"data": nil, "error": errorBody{status, name, msg, details}})
}

func (h *Handler) internal(w http.ResponseWriter, r *http.Request, err error) {
	h.Log.Error("api error", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "InternalServerError", "Internal Server Error", nil)
}

// allow checks a permission and, when it is missing, answers 403 with a message that says
// what is blocked and where to change it.
func allow(w http.ResponseWriter, r *http.Request, perm string) bool {
	p := principalFrom(r)
	if p.Allowed(perm) {
		return true
	}
	action, subject, _ := strings.Cut(perm, ":")
	what := fmt.Sprintf("%q on %q", action, subject)
	if perm == apitokens.UploadRead || perm == apitokens.UploadWrite {
		what = map[string]string{apitokens.UploadRead: "reading the media library", apitokens.UploadWrite: "uploading or deleting media"}[perm]
	}
	if !p.Authenticated() {
		if strings.HasPrefix(perm, "find") {
			forbidden(w, fmt.Sprintf("Public access to %s is turned off. Turn it on in the admin under Site settings → Public API access, or send an API token in the Authorization header.", what))
		} else {
			forbidden(w, fmt.Sprintf("An API token is required for %s. Create one in the admin under Site settings → API Tokens and send it as \"Authorization: Bearer <token>\".", what))
		}
		return false
	}
	forbidden(w, fmt.Sprintf("This API token (%s) does not allow %s. Change its permissions in the admin under Site settings → API Tokens.", p.Token.Name, what))
	return false
}

func forbidden(w http.ResponseWriter, msg string) {
	if msg == "" {
		msg = "Forbidden"
	}
	writeError(w, http.StatusForbidden, "ForbiddenError", msg, nil)
}

// handleErr maps engine errors to responses.
func (h *Handler) handleErr(w http.ResponseWriter, r *http.Request, err error) {
	var ve *schema.ValidationError
	var qe *content.QueryError
	switch {
	case errors.As(err, &ve):
		msg := ve.Message
		if len(ve.Errors) == 1 {
			msg = ve.Error()
		}
		writeError(w, http.StatusBadRequest, "ValidationError", msg, map[string]any{"errors": ve.Errors})
	case errors.As(err, &qe):
		writeError(w, http.StatusBadRequest, "ValidationError", qe.Msg, nil)
	case errors.Is(err, content.ErrNotFound), errors.Is(err, media.ErrNotFound):
		writeError(w, http.StatusNotFound, "NotFoundError", "Not Found", nil)
	case errors.Is(err, content.ErrSingleExists):
		writeError(w, http.StatusBadRequest, "ValidationError", err.Error(), nil)
	case errors.Is(err, media.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "PayloadTooLargeError",
			"File exceeds the limit of "+strconv.FormatInt(h.Media.MaxSize()>>20, 10)+" MB", nil)
	case errors.Is(err, media.ErrType):
		writeError(w, http.StatusBadRequest, "ValidationError", err.Error(), nil)
	default:
		h.internal(w, r, err)
	}
}

// --- content ---------------------------------------------------------------

func (h *Handler) contentType(w http.ResponseWriter, r *http.Request) (*schema.ContentType, bool) {
	ct, err := h.Schemas.GetByRoute(r.Context(), siteFrom(r).ID, chi.URLParam(r, "type"))
	if errors.Is(err, schema.ErrNotFound) {
		writeError(w, http.StatusNotFound, "NotFoundError", fmt.Sprintf("This site has no content type at %q. Collections use their plural API id (e.g. /articles), single types their singular id (e.g. /homepage).", chi.URLParam(r, "type")), nil)
		return nil, false
	}
	if err != nil {
		h.internal(w, r, err)
		return nil, false
	}
	return ct, true
}

// parseQuery reads list/read parameters and enforces read rules shared by every GET:
// drafts need a token, and populating a relation needs find permission on its target type.
func (h *Handler) parseQuery(w http.ResponseWriter, r *http.Request, ct *schema.ContentType) (content.Query, bool) {
	g := h.Settings.General(r.Context())
	q, err := content.ParseQuery(ct, r.URL.Query(), content.Limits{DefaultPageSize: g.DefaultPageSize, MaxPageSize: g.MaxPageSize})
	if err != nil {
		h.handleErr(w, r, err)
		return q, false
	}
	p := principalFrom(r)
	if !ct.DraftPublish {
		q.Status = content.StatusAny
	} else if q.Status == content.StatusDraft && !p.Authenticated() {
		forbidden(w, "Reading drafts requires an API token")
		return q, false
	}
	for _, name := range q.Populate {
		f, _ := ct.Field(name)
		if f.Type == schema.TypeRelation && f.Target != ct.UID && !p.Allowed(apitokens.Perm(apitokens.Find, f.Target)) {
			forbidden(w, "Populating "+name+" requires find permission on "+f.Target)
			return q, false
		}
	}
	return q, true
}

func serialize(e *content.Entry, q content.Query) map[string]any {
	return content.Serialize(e, content.SerializeOptions{Fields: q.Fields})
}

func (h *Handler) find(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.contentType(w, r)
	if !ok {
		return
	}
	if !allow(w, r, apitokens.Perm(apitokens.Find, ct.UID)) {
		return
	}
	q, ok := h.parseQuery(w, r, ct)
	if !ok {
		return
	}
	if ct.Kind == schema.KindSingle {
		e, err := h.Engine.GetSingle(r.Context(), ct, content.ReadOptions{Status: q.Status, Populate: q.Populate})
		if err != nil {
			h.handleErr(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": serialize(e, q), "meta": map[string]any{}})
		return
	}
	res, err := h.Engine.List(r.Context(), ct, q)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	data := make([]map[string]any, len(res.Entries))
	for i, e := range res.Entries {
		data[i] = serialize(e, q)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "meta": map[string]any{"pagination": map[string]int{
		"page": res.Page, "pageSize": res.PageSize, "pageCount": res.PageCount(), "total": res.Total,
	}}})
}

func entryID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "NotFoundError", "Not Found", nil)
		return 0, false
	}
	return id, true
}

func (h *Handler) collection(w http.ResponseWriter, r *http.Request) (*schema.ContentType, bool) {
	ct, ok := h.contentType(w, r)
	if ok && ct.Kind != schema.KindCollection {
		writeError(w, http.StatusNotFound, "NotFoundError", "Not Found", nil)
		return nil, false
	}
	return ct, ok
}

func (h *Handler) findOne(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.collection(w, r)
	if !ok {
		return
	}
	id, ok := entryID(w, r)
	if !ok {
		return
	}
	if !allow(w, r, apitokens.Perm(apitokens.FindOne, ct.UID)) {
		return
	}
	q, ok := h.parseQuery(w, r, ct)
	if !ok {
		return
	}
	e, err := h.Engine.Get(r.Context(), ct, id, content.ReadOptions{Status: q.Status, Populate: q.Populate})
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": serialize(e, q), "meta": map[string]any{}})
}

// readBody decodes {"data": {...}}.
func readBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var body struct {
		Data map[string]any `json:"data"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "PayloadTooLargeError", "Request body too large", nil)
		} else {
			writeError(w, http.StatusBadRequest, "ValidationError", "Request body must be JSON like {\"data\": {...}}", nil)
		}
		return nil, false
	}
	if body.Data == nil {
		writeError(w, http.StatusBadRequest, "ValidationError", "Missing \"data\" payload in the request body", nil)
		return nil, false
	}
	return body.Data, true
}

// writeStatus reads ?status=published|draft for writes.
func writeStatus(w http.ResponseWriter, r *http.Request) (content.Status, bool) {
	switch s := r.URL.Query().Get("status"); s {
	case "":
		return content.StatusAny, true
	case "published":
		return content.StatusPublished, true
	case "draft":
		return content.StatusDraft, true
	}
	writeError(w, http.StatusBadRequest, "ValidationError", "status must be published or draft", nil)
	return "", false
}

// respondEntry re-reads a written entry with the request's populate/fields parameters.
func (h *Handler) respondEntry(w http.ResponseWriter, r *http.Request, ct *schema.ContentType, e *content.Entry, status int) {
	q, ok := h.parseQuery(w, r, ct)
	if !ok {
		return
	}
	if len(q.Populate) > 0 {
		var err error
		if e, err = h.Engine.Get(r.Context(), ct, e.ID, content.ReadOptions{Populate: q.Populate}); err != nil {
			h.handleErr(w, r, err)
			return
		}
	}
	writeJSON(w, status, map[string]any{"data": serialize(e, q), "meta": map[string]any{}})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.collection(w, r)
	if !ok {
		return
	}
	if !allow(w, r, apitokens.Perm(apitokens.Create, ct.UID)) {
		return
	}
	status, ok := writeStatus(w, r)
	if !ok {
		return
	}
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	e, err := h.Engine.Create(r.Context(), ct, data, content.WriteOptions{Status: status})
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	h.respondEntry(w, r, ct, e, http.StatusCreated)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.collection(w, r)
	if !ok {
		return
	}
	id, ok := entryID(w, r)
	if !ok {
		return
	}
	if !allow(w, r, apitokens.Perm(apitokens.Update, ct.UID)) {
		return
	}
	status, ok := writeStatus(w, r)
	if !ok {
		return
	}
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	e, err := h.Engine.Update(r.Context(), ct, id, data, content.WriteOptions{Status: status})
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	h.respondEntry(w, r, ct, e, http.StatusOK)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.collection(w, r)
	if !ok {
		return
	}
	id, ok := entryID(w, r)
	if !ok {
		return
	}
	if !allow(w, r, apitokens.Perm(apitokens.Delete, ct.UID)) {
		return
	}
	if err := h.Engine.Delete(r.Context(), ct, id); err != nil {
		h.handleErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) single(w http.ResponseWriter, r *http.Request, action string) (*schema.ContentType, bool) {
	ct, ok := h.contentType(w, r)
	if !ok {
		return nil, false
	}
	if ct.Kind != schema.KindSingle {
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowedError", "Use /"+ct.PluralName+"/{id} for collection entries", nil)
		return nil, false
	}
	if !allow(w, r, apitokens.Perm(action, ct.UID)) {
		return nil, false
	}
	return ct, true
}

func (h *Handler) updateSingle(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.single(w, r, apitokens.Update)
	if !ok {
		return
	}
	status, ok := writeStatus(w, r)
	if !ok {
		return
	}
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	e, err := h.Engine.UpsertSingle(r.Context(), ct, data, content.WriteOptions{Status: status})
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	h.respondEntry(w, r, ct, e, http.StatusOK)
}

func (h *Handler) deleteSingle(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.single(w, r, apitokens.Delete)
	if !ok {
		return
	}
	e, err := h.Engine.GetSingle(r.Context(), ct, content.ReadOptions{})
	if err == nil {
		err = h.Engine.Delete(r.Context(), ct, e.ID)
	}
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
