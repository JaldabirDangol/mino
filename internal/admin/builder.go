package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"mino/internal/schema"
	"mino/internal/users"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func (h *Handler) canBuild(w http.ResponseWriter, r *http.Request) bool {
	if !currentAccess(r).Can(users.SchemaManage) {
		h.forbidden(w, r)
		return false
	}
	return true
}

type builderData struct {
	Types       []*schema.ContentType
	Current     *schema.ContentType
	Counts      map[int64]int
	FieldTypes  []schema.FieldTypeInfo
	Targets     []map[string]string
	Kinds       []schema.RelationKind
	Create      map[string]string // values of the "new content type" form after an error
	Errors      map[string]string
	OpenCreate  bool
	CurrentJSON string
}

func (h *Handler) builderData(r *http.Request, current *schema.ContentType) (*builderData, error) {
	types, err := h.Schemas.List(r.Context(), currentSite(r).ID)
	if err != nil {
		return nil, err
	}
	counts, err := h.Schemas.Count(r.Context(), currentSite(r).ID)
	if err != nil {
		return nil, err
	}
	d := &builderData{Types: types, Current: current, Counts: counts, FieldTypes: schema.TypeInfo(), Kinds: schema.RelationKinds}
	for _, t := range types {
		if t.Kind == schema.KindCollection {
			d.Targets = append(d.Targets, map[string]string{"uid": t.UID, "name": t.DisplayName})
		}
	}
	if current != nil {
		b, _ := json.Marshal(current)
		d.CurrentJSON = string(b)
	}
	return d, nil
}

func (h *Handler) builderHome(w http.ResponseWriter, r *http.Request) {
	if !h.canBuild(w, r) {
		return
	}
	d, err := h.builderData(r, nil)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if len(d.Types) > 0 && r.URL.Query().Get("new") == "" {
		h.redirect(w, r, sitePath(r, "/builder/", d.Types[0].UID))
		return
	}
	d.OpenCreate = r.URL.Query().Get("new") != ""
	h.page(w, r, http.StatusOK, "builder", h.view(r, "Content-Type Builder", "builder", d))
}

func (h *Handler) builderCreate(w http.ResponseWriter, r *http.Request) {
	if !h.canBuild(w, r) {
		return
	}
	kind := schema.Kind(r.PostFormValue("kind"))
	ct := &schema.ContentType{
		SiteID:       currentSite(r).ID,
		DisplayName:  r.PostFormValue("displayName"),
		UID:          strings.TrimSpace(r.PostFormValue("uid")),
		PluralName:   strings.TrimSpace(r.PostFormValue("pluralName")),
		Kind:         kind,
		Description:  r.PostFormValue("description"),
		DraftPublish: r.PostFormValue("draftPublish") == "true",
	}
	created, err := h.Schemas.Create(r.Context(), ct, currentUser(r).ID)
	if err != nil {
		errs, msg, ok := formErrors(err)
		if !ok {
			h.serverError(w, r, err)
			return
		}
		d, derr := h.builderData(r, nil)
		if derr != nil {
			h.serverError(w, r, derr)
			return
		}
		if len(errs) == 0 {
			errs = map[string]string{"displayName": msg}
		}
		d.Errors, d.OpenCreate = errs, true
		d.Create = map[string]string{"displayName": ct.DisplayName, "uid": ct.UID, "pluralName": ct.PluralName,
			"kind": string(ct.Kind), "description": ct.Description, "draftPublish": strconv.FormatBool(ct.DraftPublish)}
		h.partial(w, http.StatusUnprocessableEntity, "builder", "create-form", h.view(r, "", "builder", d))
		return
	}
	h.done(w, r, sitePath(r, "/builder/", created.UID), "success", created.DisplayName+" created. Add its fields next.")
}

func (h *Handler) builderEdit(w http.ResponseWriter, r *http.Request) {
	if !h.canBuild(w, r) {
		return
	}
	ct, err := h.Schemas.Get(r.Context(), currentSite(r).ID, chi.URLParam(r, "uid"))
	if errors.Is(err, schema.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	d, err := h.builderData(r, ct)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.page(w, r, http.StatusOK, "builder", h.view(r, ct.DisplayName+" · Content-Type Builder", "builder", d))
}

type planItem struct {
	Level   string `json:"level"` // warning | destructive | blocked
	Message string `json:"message"`
}

// builderSave accepts {"contentType": {...}, "confirm": bool} and answers in JSON.
func (h *Handler) builderSave(w http.ResponseWriter, r *http.Request) {
	if !h.canBuild(w, r) {
		return
	}
	var body struct {
		ContentType schema.ContentType `json:"contentType"`
		Confirm     bool               `json:"confirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "The schema could not be read."})
		return
	}
	uid := chi.URLParam(r, "uid")
	updated, plan, err := h.Schemas.Update(r.Context(), currentSite(r).ID, uid, &body.ContentType, body.Confirm, currentUser(r).ID)
	var pe *schema.PlanError
	var ve *schema.ValidationError
	switch {
	case errors.As(err, &pe):
		items := []planItem{}
		for _, imp := range plan.Notable() {
			lvl := map[schema.ImpactLevel]string{schema.ImpactWarning: "warning", schema.ImpactDestructive: "destructive", schema.ImpactBlocked: "blocked"}[imp.Level]
			items = append(items, planItem{Level: lvl, Message: imp.Message})
		}
		writeJSON(w, http.StatusConflict, map[string]any{"plan": items, "blocked": plan.Blocked()})
	case errors.As(err, &ve):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": ve.Message, "errors": ve.Errors})
	case errors.Is(err, schema.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "This content type no longer exists."})
	case err != nil:
		h.Log.Error("save schema", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "The server could not save the schema."})
	default:
		h.setCookie(w, flashCookie, encodeFlash(Flash{Kind: "success", Message: updated.DisplayName + " saved"}), 60)
		writeJSON(w, http.StatusOK, map[string]any{"redirect": sitePath(r, "/builder/", updated.UID)})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) builderDelete(w http.ResponseWriter, r *http.Request) {
	if !h.canBuild(w, r) {
		return
	}
	site := currentSite(r)
	uid := chi.URLParam(r, "uid")
	ct, err := h.Schemas.Get(r.Context(), site.ID, uid)
	if errors.Is(err, schema.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if strings.TrimSpace(r.PostFormValue("confirmName")) != ct.UID {
		h.done(w, r, sitePath(r, "/builder/", uid), "error", "Type the API id “"+ct.UID+"” to confirm deletion.")
		return
	}
	if err := h.Schemas.Delete(r.Context(), site.ID, uid); err != nil {
		if _, msg, ok := formErrors(err); ok {
			h.done(w, r, sitePath(r, "/builder/", uid), "error", msg)
			return
		}
		h.serverError(w, r, err)
		return
	}
	h.done(w, r, sitePath(r, "/builder"), "success", ct.DisplayName+" and all its entries were deleted")
}
