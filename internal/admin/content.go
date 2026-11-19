package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"mino/internal/content"
	"mino/internal/media"
	"mino/internal/schema"
	"mino/internal/users"
)

// siteType resolves {uid} within the current site and checks read access.
func (h *Handler) siteType(w http.ResponseWriter, r *http.Request) (*schema.ContentType, bool) {
	ct, err := h.Schemas.Get(r.Context(), currentSite(r).ID, chi.URLParam(r, "uid"))
	if errors.Is(err, schema.ErrNotFound) {
		h.notFound(w, r)
		return nil, false
	}
	if err != nil {
		h.serverError(w, r, err)
		return nil, false
	}
	if !currentAccess(r).CanContent(users.ContentRead, ct) {
		h.notFound(w, r)
		return nil, false
	}
	return ct, true
}

func (h *Handler) contentHome(w http.ResponseWriter, r *http.Request) {
	v := h.view(r, "Content Manager", "content", nil)
	if len(v.Types) > 0 {
		h.redirect(w, r, sitePath(r, "/content/", v.Types[0].UID))
		return
	}
	h.page(w, r, http.StatusOK, "content_empty", v)
}

// --- list ------------------------------------------------------------------------

type listColumn struct {
	Field    schema.Field
	Sortable bool
}

type listRow struct {
	Entry     *content.Entry
	CanEdit   bool
	CanDelete bool
}

type listData struct {
	Type       *schema.ContentType
	Columns    []listColumn
	Rows       []listRow
	Result     *content.Result
	Params     listParams
	Filterable []schema.Field
	CanCreate  bool
	QueryError string
	Pager      pager
}

// Offset is the number of entries before the current page.
func (d *listData) Offset() int { return (d.Result.Page - 1) * d.Result.PageSize }

type listParams struct {
	Search, Status, Sort, FilterField, FilterOp, FilterValue string
	Page, Size                                               int
}

// URL builds the list URL with overrides (key, value pairs).
func (p listParams) URL(base string, kv ...string) string {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	set("q", p.Search)
	set("status", p.Status)
	set("sort", p.Sort)
	set("ff", p.FilterField)
	set("fo", p.FilterOp)
	set("fv", p.FilterValue)
	if p.Size != 25 && p.Size != 0 {
		set("size", strconv.Itoa(p.Size))
	}
	if p.Page > 1 {
		set("page", strconv.Itoa(p.Page))
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			v.Del(kv[i])
		} else {
			v.Set(kv[i], kv[i+1])
		}
	}
	if len(v) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

// SortList returns, for a column header: the sort value its link applies (cycling
// asc → desc → default), the current direction, and the aria-sort value.
func (p listParams) SortList(field string) []string {
	switch p.Sort {
	case field + ":asc":
		return []string{field + ":desc", "asc", "ascending"}
	case field + ":desc":
		return []string{"", "desc", "descending"}
	}
	return []string{field + ":asc", "", "none"}
}

func (h *Handler) contentIndex(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.siteType(w, r)
	if !ok {
		return
	}
	if ct.Kind == schema.KindSingle {
		h.singleForm(w, r, ct)
		return
	}
	acc := currentAccess(r)
	q := r.URL.Query()
	p := listParams{Search: strings.TrimSpace(q.Get("q")), Status: q.Get("status"), Sort: q.Get("sort"),
		FilterField: q.Get("ff"), FilterOp: q.Get("fo"), FilterValue: q.Get("fv")}
	p.Page, _ = strconv.Atoi(q.Get("page"))
	p.Page = max(p.Page, 1)
	p.Size, _ = strconv.Atoi(q.Get("size"))
	if p.Size != 10 && p.Size != 50 && p.Size != 100 {
		p.Size = 25
	}

	d := &listData{Type: ct, Params: p, CanCreate: acc.CanContent(users.ContentCreate, ct)}
	// Reuse the API's parser for sort and filter so validation is identical.
	vals := url.Values{}
	if p.Sort != "" {
		vals.Set("sort", p.Sort)
	}
	if p.FilterField != "" && p.FilterOp != "" && (p.FilterValue != "" || p.FilterOp == "$null" || p.FilterOp == "$notNull") {
		fv := p.FilterValue
		if p.FilterOp == "$null" || p.FilterOp == "$notNull" {
			fv = "true"
		}
		vals.Set("filters["+p.FilterField+"]["+p.FilterOp+"]", fv)
	}
	query, err := content.ParseQuery(ct, vals, content.Limits{DefaultPageSize: p.Size, MaxPageSize: 100})
	var qe *content.QueryError
	if errors.As(err, &qe) {
		d.QueryError = qe.Msg
		query = content.Query{}
	} else if err != nil {
		h.serverError(w, r, err)
		return
	}
	if len(query.Sort) == 0 {
		query.Sort = []content.Sort{{Field: "updatedAt", Desc: true}}
	}
	query.Page, query.PageSize, query.Search = p.Page, p.Size, p.Search
	query.Status = content.StatusAny
	switch p.Status {
	case "draft":
		query.Status = content.StatusDraft
	case "published":
		query.Status = content.StatusPublished
	}
	res, err := h.Engine.List(r.Context(), ct, query)
	if errors.As(err, &qe) {
		d.QueryError = qe.Msg
		res, err = h.Engine.List(r.Context(), ct, content.Query{Page: 1, PageSize: p.Size, Status: content.StatusAny})
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	d.Result = res
	base := sitePath(r, "/content/", ct.UID)
	d.Pager = makePager(res.Page, res.PageCount(), func(n int) string { return p.URL(base, "page", strconv.Itoa(n)) }, "#entries", true)
	for _, e := range res.Entries {
		d.Rows = append(d.Rows, listRow{Entry: e,
			CanEdit:   acc.CanEntry(users.ContentUpdate, ct, e.CreatedBy),
			CanDelete: acc.CanEntry(users.ContentDelete, ct, e.CreatedBy)})
	}
	for _, f := range ct.Fields {
		if f.IsLink() || f.Type == schema.TypeJSON {
			continue
		}
		// The shared query parser refuses private fields for filtering and sorting.
		if !f.Private {
			d.Filterable = append(d.Filterable, f)
		}
		if len(d.Columns) < 4 && f.Type != schema.TypeRichText && f.Type != schema.TypeText {
			d.Columns = append(d.Columns, listColumn{Field: f, Sortable: f.Sortable() && !f.Private})
		}
	}

	v := h.view(r, ct.DisplayName, "content", d)
	if isHTMX(r) && r.Header.Get("HX-Target") == "entries" {
		h.partial(w, http.StatusOK, "content_list", "entries", v)
		return
	}
	h.page(w, r, http.StatusOK, "content_list", v)
}

// --- form ------------------------------------------------------------------------

type linkItem struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
	Thumb string `json:"thumb,omitempty"`
	Mime  string `json:"mime,omitempty"`
}

type formField struct {
	Field   schema.Field
	Value   string
	Checked bool
	Links   []linkItem
	Error   string
}

type entryForm struct {
	Type       *schema.ContentType
	Entry      *content.Entry
	Fields     []formField
	Error      string
	CanSave    bool
	CanPublish bool
	CanDelete  bool
	CanCreate  bool
	ReadOnly   bool
	Action     string // form action URL
}

func valueString(f schema.Field, v any) string {
	if v == nil {
		return ""
	}
	switch f.Type {
	case schema.TypeNumber:
		return strconv.FormatFloat(v.(float64), 'f', -1, 64)
	case schema.TypeDateTime:
		if t, err := time.Parse(time.RFC3339Nano, fmt.Sprint(v)); err == nil {
			return t.UTC().Format("2006-01-02T15:04")
		}
	case schema.TypeJSON:
		b, _ := json.MarshalIndent(v, "", "  ")
		return string(b)
	}
	return fmt.Sprint(v)
}

func (h *Handler) formFields(ct *schema.ContentType, e *content.Entry) []formField {
	out := make([]formField, len(ct.Fields))
	for i, f := range ct.Fields {
		ff := formField{Field: f}
		var v any
		if e != nil {
			v = e.Data[f.Name]
		} else if f.Default != nil && !f.IsLink() {
			v, _ = schema.CoerceValue(f, f.Default)
		}
		switch f.Type {
		case schema.TypeBoolean:
			ff.Checked, _ = v.(bool)
		case schema.TypeRelation:
			if e != nil {
				for _, t := range e.Relations[f.Name] {
					ff.Links = append(ff.Links, linkItem{ID: t.ID, Label: t.Title()})
				}
			}
		case schema.TypeMedia:
			if e != nil {
				for _, m := range e.Media[f.Name] {
					ff.Links = append(ff.Links, mediaItem(m))
				}
			}
		default:
			ff.Value = valueString(f, v)
		}
		out[i] = ff
	}
	return out
}

func mediaItem(m *media.File) linkItem {
	it := linkItem{ID: m.ID, Label: m.Name, Mime: m.Mime}
	if m.IsImage() {
		it.Thumb = m.URL
	}
	return it
}

// parseForm converts submitted form values to engine input. Read-only fields are skipped so
// they keep their stored values. Conversion errors are returned per field; the engine
// performs all remaining validation.
func parseForm(ct *schema.ContentType, form url.Values) (map[string]any, map[string]string) {
	input := map[string]any{}
	errs := map[string]string{}
	for _, f := range ct.Fields {
		if f.ReadOnly {
			continue
		}
		raw := strings.TrimSpace(form.Get(f.Name))
		switch f.Type {
		case schema.TypeBoolean:
			vals := form[f.Name]
			input[f.Name] = len(vals) > 0 && vals[len(vals)-1] == "true"
		case schema.TypeNumber:
			if raw == "" {
				input[f.Name] = nil
			} else if n, err := strconv.ParseFloat(raw, 64); err == nil {
				input[f.Name] = n
			} else {
				errs[f.Name] = "Must be a number"
			}
		case schema.TypeDateTime:
			if raw == "" {
				input[f.Name] = nil
			} else if t, err := time.Parse("2006-01-02T15:04", raw); err == nil {
				input[f.Name] = t.UTC().Format(time.RFC3339)
			} else {
				input[f.Name] = raw // let the engine report the format error
			}
		case schema.TypeJSON:
			if raw == "" {
				input[f.Name] = nil
			} else {
				var v any
				if err := json.Unmarshal([]byte(raw), &v); err != nil {
					errs[f.Name] = "Not valid JSON: " + err.Error()
				} else {
					input[f.Name] = v
				}
			}
		case schema.TypeRelation, schema.TypeMedia:
			ids := []int64{}
			for _, s := range form[f.Name] {
				if id, err := strconv.ParseInt(s, 10, 64); err == nil && id > 0 {
					ids = append(ids, id)
				}
			}
			input[f.Name] = ids
		case schema.TypeRichText, schema.TypeText:
			input[f.Name] = form.Get(f.Name) // keep whitespace in long text
		default:
			input[f.Name] = raw
		}
	}
	return input, errs
}

// refill rebuilds form fields from submitted values after a failed save, resolving link labels.
func (h *Handler) refill(r *http.Request, ct *schema.ContentType, form url.Values, errs map[string]string) []formField {
	out := h.formFields(ct, nil)
	for i := range out {
		f := out[i].Field
		out[i].Error = errs[f.Name]
		out[i].Value = form.Get(f.Name)
		switch f.Type {
		case schema.TypeBoolean:
			vals := form[f.Name]
			out[i].Checked = len(vals) > 0 && vals[len(vals)-1] == "true"
		case schema.TypeRelation, schema.TypeMedia:
			out[i].Links = h.linkItems(r, ct, f, form[f.Name])
		}
	}
	return out
}

func (h *Handler) linkItems(r *http.Request, ct *schema.ContentType, f schema.Field, rawIDs []string) []linkItem {
	var out []linkItem
	if len(rawIDs) == 0 {
		return out
	}
	if f.Type == schema.TypeMedia {
		for _, s := range rawIDs {
			id, _ := strconv.ParseInt(s, 10, 64)
			if m, err := h.Media.Get(r.Context(), ct.SiteID, id); err == nil {
				out = append(out, mediaItem(m))
			}
		}
		return out
	}
	target, err := h.Schemas.Get(r.Context(), ct.SiteID, f.Target)
	if err != nil {
		return out
	}
	q := content.Query{Page: 1, PageSize: len(rawIDs), Filter: content.Filter{Field: "id", Op: "$in", Value: rawIDs}}
	res, err := h.Engine.List(r.Context(), target, q)
	if err != nil {
		return out
	}
	byID := map[int64]*content.Entry{}
	for _, e := range res.Entries {
		byID[e.ID] = e
	}
	for _, s := range rawIDs {
		id, _ := strconv.ParseInt(s, 10, 64)
		if e, ok := byID[id]; ok {
			out = append(out, linkItem{ID: e.ID, Label: e.Title()})
		}
	}
	return out
}

func (h *Handler) allLinks(ct *schema.ContentType) []string {
	var out []string
	for _, f := range ct.Fields {
		if f.IsLink() {
			out = append(out, f.Name)
		}
	}
	return out
}

func (h *Handler) renderForm(w http.ResponseWriter, r *http.Request, status int, ct *schema.ContentType, e *content.Entry, fields []formField, errMsg string) {
	acc := currentAccess(r)
	d := &entryForm{Type: ct, Entry: e, Fields: fields, Error: errMsg, CanCreate: acc.CanContent(users.ContentCreate, ct)}
	if e == nil {
		d.CanSave = acc.CanContent(users.ContentCreate, ct)
		d.CanPublish = d.CanSave && acc.CanContent(users.ContentPublish, ct)
		d.Action = sitePath(r, "/content/", ct.UID, "/new")
		if ct.Kind == schema.KindSingle {
			d.CanSave = acc.CanContent(users.ContentCreate, ct) || acc.CanContent(users.ContentUpdate, ct)
			d.Action = sitePath(r, "/content/", ct.UID)
		}
	} else {
		d.CanSave = acc.CanEntry(users.ContentUpdate, ct, e.CreatedBy)
		d.CanPublish = d.CanSave && acc.CanEntry(users.ContentPublish, ct, e.CreatedBy)
		d.CanDelete = acc.CanEntry(users.ContentDelete, ct, e.CreatedBy)
		d.Action = sitePath(r, "/content/", ct.UID, "/", strconv.FormatInt(e.ID, 10))
		if ct.Kind == schema.KindSingle {
			d.Action = sitePath(r, "/content/", ct.UID)
		}
	}
	if ct.Kind == schema.KindSingle && e == nil {
		d.CanPublish = d.CanSave && acc.CanContent(users.ContentPublish, ct)
	}
	d.ReadOnly = !d.CanSave
	title := ct.DisplayName
	if e != nil && ct.Kind == schema.KindCollection {
		title = e.Title()
	} else if e == nil && ct.Kind == schema.KindCollection {
		title = "New " + strings.ToLower(ct.DisplayName)
	}
	h.page(w, r, status, "content_form", h.view(r, title, "content", d))
}

func entryIDParam(r *http.Request) int64 {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id
}

func (h *Handler) loadEntry(w http.ResponseWriter, r *http.Request, ct *schema.ContentType) (*content.Entry, bool) {
	e, err := h.Engine.Get(r.Context(), ct, entryIDParam(r), content.ReadOptions{Populate: h.allLinks(ct)})
	if errors.Is(err, content.ErrNotFound) {
		h.notFound(w, r)
		return nil, false
	}
	if err != nil {
		h.serverError(w, r, err)
		return nil, false
	}
	return e, true
}

func (h *Handler) entryNew(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.siteType(w, r)
	if !ok {
		return
	}
	if ct.Kind == schema.KindSingle || !currentAccess(r).CanContent(users.ContentCreate, ct) {
		h.forbidden(w, r)
		return
	}
	h.renderForm(w, r, http.StatusOK, ct, nil, h.formFields(ct, nil), "")
}

func (h *Handler) entryEdit(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.siteType(w, r)
	if !ok {
		return
	}
	e, ok := h.loadEntry(w, r, ct)
	if !ok {
		return
	}
	h.renderForm(w, r, http.StatusOK, ct, e, h.formFields(ct, e), "")
}

func (h *Handler) singleForm(w http.ResponseWriter, r *http.Request, ct *schema.ContentType) {
	e, err := h.Engine.GetSingle(r.Context(), ct, content.ReadOptions{Populate: h.allLinks(ct)})
	if errors.Is(err, content.ErrNotFound) {
		e, err = nil, nil
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.renderForm(w, r, http.StatusOK, ct, e, h.formFields(ct, e), "")
}

// saveResult handles the outcome of a create/update.
func (h *Handler) saveResult(w http.ResponseWriter, r *http.Request, ct *schema.ContentType, existing *content.Entry, saved *content.Entry, err error, publishing bool) {
	if err != nil {
		errs, msg, ok := formErrors(err)
		if !ok {
			if errors.Is(err, content.ErrNotFound) {
				h.notFound(w, r)
				return
			}
			h.serverError(w, r, err)
			return
		}
		if publishing {
			msg = "Fix the highlighted fields to publish."
		} else if len(errs) > 0 {
			msg = "Fix the highlighted fields to save."
		}
		h.renderForm(w, r, http.StatusUnprocessableEntity, ct, existing, h.refill(r, ct, r.PostForm, errs), msg)
		return
	}
	msg := "Saved"
	if publishing {
		msg = "Published"
	}
	to := sitePath(r, "/content/", ct.UID, "/", strconv.FormatInt(saved.ID, 10))
	if ct.Kind == schema.KindSingle {
		to = sitePath(r, "/content/", ct.UID)
	}
	h.done(w, r, to, "success", msg)
}

func (h *Handler) wantsPublish(r *http.Request, ct *schema.ContentType) bool {
	return ct.DraftPublish && r.PostFormValue("action") == "publish"
}

func (h *Handler) entryCreate(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.siteType(w, r)
	if !ok {
		return
	}
	acc := currentAccess(r)
	publish := h.wantsPublish(r, ct)
	if ct.Kind == schema.KindSingle || !acc.CanContent(users.ContentCreate, ct) || (publish && !acc.CanContent(users.ContentPublish, ct)) {
		h.forbidden(w, r)
		return
	}
	input, perr := parseForm(ct, r.PostForm)
	if len(perr) > 0 {
		h.renderForm(w, r, http.StatusUnprocessableEntity, ct, nil, h.refill(r, ct, r.PostForm, perr), "Fix the highlighted fields to save.")
		return
	}
	opts := content.WriteOptions{UserID: currentUser(r).ID}
	if publish {
		opts.Status = content.StatusPublished
	}
	e, err := h.Engine.Create(r.Context(), ct, input, opts)
	h.saveResult(w, r, ct, nil, e, err, publish)
}

func (h *Handler) entryUpdate(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.siteType(w, r)
	if !ok {
		return
	}
	cur, ok := h.loadEntry(w, r, ct)
	if !ok {
		return
	}
	acc := currentAccess(r)
	publish := h.wantsPublish(r, ct)
	if !acc.CanEntry(users.ContentUpdate, ct, cur.CreatedBy) || (publish && !acc.CanEntry(users.ContentPublish, ct, cur.CreatedBy)) {
		h.forbidden(w, r)
		return
	}
	input, perr := parseForm(ct, r.PostForm)
	if len(perr) > 0 {
		h.renderForm(w, r, http.StatusUnprocessableEntity, ct, cur, h.refill(r, ct, r.PostForm, perr), "Fix the highlighted fields to save.")
		return
	}
	opts := content.WriteOptions{UserID: currentUser(r).ID}
	if publish {
		opts.Status = content.StatusPublished
	}
	e, err := h.Engine.Update(r.Context(), ct, cur.ID, input, opts)
	h.saveResult(w, r, ct, cur, e, err, publish)
}

func (h *Handler) singleSave(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.siteType(w, r)
	if !ok {
		return
	}
	if ct.Kind != schema.KindSingle {
		h.notFound(w, r)
		return
	}
	acc := currentAccess(r)
	cur, err := h.Engine.GetSingle(r.Context(), ct, content.ReadOptions{Populate: h.allLinks(ct)})
	if errors.Is(err, content.ErrNotFound) {
		cur, err = nil, nil
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	publish := h.wantsPublish(r, ct)
	allowed := acc.CanContent(users.ContentCreate, ct)
	if cur != nil {
		allowed = acc.CanEntry(users.ContentUpdate, ct, cur.CreatedBy)
	}
	if !allowed || (publish && !acc.CanContent(users.ContentPublish, ct)) {
		h.forbidden(w, r)
		return
	}
	input, perr := parseForm(ct, r.PostForm)
	if len(perr) > 0 {
		h.renderForm(w, r, http.StatusUnprocessableEntity, ct, cur, h.refill(r, ct, r.PostForm, perr), "Fix the highlighted fields to save.")
		return
	}
	opts := content.WriteOptions{UserID: currentUser(r).ID}
	if publish {
		opts.Status = content.StatusPublished
	}
	e, err := h.Engine.UpsertSingle(r.Context(), ct, input, opts)
	h.saveResult(w, r, ct, cur, e, err, publish)
}

func (h *Handler) entryAction(w http.ResponseWriter, r *http.Request, action string, run func(*schema.ContentType, *content.Entry) (string, string, error)) {
	ct, ok := h.siteType(w, r)
	if !ok {
		return
	}
	e, ok := h.loadEntry(w, r, ct)
	if !ok {
		return
	}
	if !currentAccess(r).CanEntry(action, ct, e.CreatedBy) {
		h.forbidden(w, r)
		return
	}
	to, msg, err := run(ct, e)
	if err != nil {
		if _, emsg, ok := formErrors(err); ok {
			h.done(w, r, sitePath(r, "/content/", ct.UID, "/", strconv.FormatInt(e.ID, 10)), "error", emsg+". Open the entry and complete the required fields.")
			return
		}
		h.serverError(w, r, err)
		return
	}
	h.done(w, r, to, "success", msg)
}

func (h *Handler) entryDelete(w http.ResponseWriter, r *http.Request) {
	h.entryAction(w, r, users.ContentDelete, func(ct *schema.ContentType, e *content.Entry) (string, string, error) {
		to := sitePath(r, "/content/", ct.UID)
		return to, "Deleted “" + e.Title() + "”", h.Engine.Delete(r.Context(), ct, e.ID)
	})
}

func (h *Handler) entryPublish(w http.ResponseWriter, r *http.Request) {
	h.entryAction(w, r, users.ContentPublish, func(ct *schema.ContentType, e *content.Entry) (string, string, error) {
		_, err := h.Engine.Publish(r.Context(), ct, e.ID, currentUser(r).ID)
		return h.backTo(r, ct, e), "Published", err
	})
}

func (h *Handler) entryUnpublish(w http.ResponseWriter, r *http.Request) {
	h.entryAction(w, r, users.ContentPublish, func(ct *schema.ContentType, e *content.Entry) (string, string, error) {
		_, err := h.Engine.Unpublish(r.Context(), ct, e.ID, currentUser(r).ID)
		return h.backTo(r, ct, e), "Unpublished — the entry is now a draft", err
	})
}

func (h *Handler) backTo(r *http.Request, ct *schema.ContentType, e *content.Entry) string {
	if ct.Kind == schema.KindSingle {
		return sitePath(r, "/content/", ct.UID)
	}
	return sitePath(r, "/content/", ct.UID, "/", strconv.FormatInt(e.ID, 10))
}

func (h *Handler) entryDuplicate(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.siteType(w, r)
	if !ok {
		return
	}
	if ct.Kind == schema.KindSingle || !currentAccess(r).CanContent(users.ContentCreate, ct) {
		h.forbidden(w, r)
		return
	}
	cp, err := h.Engine.Duplicate(r.Context(), ct, entryIDParam(r), currentUser(r).ID)
	if errors.Is(err, content.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if _, msg, ok := formErrors(err); ok {
		h.done(w, r, sitePath(r, "/content/", ct.UID), "error", "Could not duplicate: "+msg)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.done(w, r, sitePath(r, "/content/", ct.UID, "/", strconv.FormatInt(cp.ID, 10)), "success", "Duplicated as a draft. Unique fields were left empty.")
}

// entryPreview shows exactly what the REST API returns for this entry with populate=*.
func (h *Handler) entryPreview(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.siteType(w, r)
	if !ok {
		return
	}
	e, ok := h.loadEntry(w, r, ct)
	if !ok {
		return
	}
	body := map[string]any{"data": content.Serialize(e, content.SerializeOptions{}), "meta": map[string]any{}}
	path := "/" + ct.PluralName + "/" + strconv.FormatInt(e.ID, 10)
	if ct.Kind == schema.KindSingle {
		path = "/" + ct.UID
	}
	v := h.view(r, "API preview", "content", map[string]any{"Body": body, "Path": path + "?populate=*", "Draft": ct.DraftPublish && !e.Published()})
	h.partial(w, http.StatusOK, "content_form", "preview", v)
}

// relationOptions lists candidate entries for a relation picker.
func (h *Handler) relationOptions(w http.ResponseWriter, r *http.Request) {
	ct, ok := h.siteType(w, r)
	if !ok {
		return
	}
	f, found := ct.Field(r.URL.Query().Get("field"))
	if !found || f.Type != schema.TypeRelation {
		h.notFound(w, r)
		return
	}
	target, err := h.Schemas.Get(r.Context(), ct.SiteID, f.Target)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	var items []linkItem
	if currentAccess(r).CanContent(users.ContentRead, target) {
		res, err := h.Engine.List(r.Context(), target, content.Query{Page: 1, PageSize: 20, Search: r.URL.Query().Get("q"),
			Sort: []content.Sort{{Field: "updatedAt", Desc: true}}})
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		for _, e := range res.Entries {
			items = append(items, linkItem{ID: e.ID, Label: e.Title()})
		}
	}
	h.partial(w, http.StatusOK, "content_form", "relation-options", h.view(r, "", "", map[string]any{"Items": items, "Target": target}))
}
