package admin

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"mino/internal/content"
	"mino/internal/media"
	"mino/internal/schema"
	"mino/internal/users"
)

type dashboardData struct {
	Collections []typeCount
	Singles     []typeCount
	Recent      []*content.Entry
	MediaCount  int
	CanBuild    bool
}

type typeCount struct {
	Type  *schema.ContentType
	Count int
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	v := h.view(r, "Dashboard", "dashboard", nil)
	counts, err := h.Schemas.Count(r.Context(), currentSite(r).ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	d := &dashboardData{CanBuild: v.Access.Can(users.SchemaManage)}
	for _, t := range v.Types {
		tc := typeCount{Type: t, Count: counts[t.ID]}
		if t.Kind == schema.KindSingle {
			d.Singles = append(d.Singles, tc)
		} else {
			d.Collections = append(d.Collections, tc)
		}
	}
	if d.Recent, err = h.Engine.Recent(r.Context(), v.Types, 8); err != nil {
		h.serverError(w, r, err)
		return
	}
	if v.Access.Can(users.MediaRead) {
		_, d.MediaCount, _ = h.Media.List(r.Context(), currentSite(r).ID, media.ListQuery{PageSize: 1})
	}
	v.Data = d
	h.page(w, r, http.StatusOK, "dashboard", v)
}

type mediaData struct {
	Files     []*media.File
	Usage     map[int64]int
	Total     int
	Page      int
	PageCount int
	Search    string
	Category  string
	CanUpload bool
	CanEdit   bool
	CanDelete bool
	MaxMB     int64
	Picker    bool   // rendered inside the media picker
	Field     string // picker target field
	Multiple  bool
	Accept    []string
	File      *media.File // detail view
	Errors    map[string]string
	Pager     pager
}

func (h *Handler) mediaList(r *http.Request, picker bool) (*mediaData, error) {
	acc := currentAccess(r)
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	size := 30
	if picker {
		size = 18
	}
	d := &mediaData{Page: max(page, 1), Search: q.Get("q"), Category: q.Get("type"), Picker: picker,
		CanUpload: acc.Can(users.MediaUpload), CanEdit: acc.Can(users.MediaUpdate), CanDelete: acc.Can(users.MediaDelete),
		MaxMB: h.Media.MaxSize() >> 20, Field: q.Get("field"), Multiple: q.Get("multiple") == "1"}
	if d.Category == "" && picker && q.Get("accept") != "" {
		d.Accept = q["accept"]
	}
	if len(d.Accept) == 1 {
		d.Category = d.Accept[0]
	}
	files, total, err := h.Media.List(r.Context(), currentSite(r).ID, media.ListQuery{Search: d.Search, Category: d.Category, Page: d.Page, PageSize: size})
	if err != nil {
		return nil, err
	}
	d.Files, d.Total, d.PageCount = files, total, (total+size-1)/size
	pq := r.URL.Query()
	base, target := sitePath(r, "/media"), "#media-grid"
	if picker {
		base, target = sitePath(r, "/media/picker"), "#picker-grid"
	}
	d.Pager = makePager(d.Page, d.PageCount, func(n int) string {
		pq.Set("page", strconv.Itoa(n))
		return base + "?" + pq.Encode()
	}, target, !picker)
	ids := make([]int64, len(files))
	for i, f := range files {
		ids[i] = f.ID
	}
	d.Usage, err = h.Media.Usage(r.Context(), ids)
	return d, err
}

func (h *Handler) mediaPage(w http.ResponseWriter, r *http.Request) {
	if !currentAccess(r).Can(users.MediaRead) {
		h.forbidden(w, r)
		return
	}
	d, err := h.mediaList(r, false)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	v := h.view(r, "Media Library", "media", d)
	if isHTMX(r) && r.Header.Get("HX-Target") == "media-grid" {
		h.partial(w, http.StatusOK, "media", "grid", v)
		return
	}
	h.page(w, r, http.StatusOK, "media", v)
}

func (h *Handler) mediaPicker(w http.ResponseWriter, r *http.Request) {
	if !currentAccess(r).Can(users.MediaRead) {
		h.forbidden(w, r)
		return
	}
	d, err := h.mediaList(r, true)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	name := "picker"
	if r.Header.Get("HX-Target") == "picker-grid" {
		name = "grid"
	}
	h.partial(w, http.StatusOK, "media", name, h.view(r, "", "media", d))
}

// mediaUpload streams one or more files ("files" parts). It answers HTMX requests by
// re-rendering the grid (library) or the picker grid.
func (h *Handler) mediaUpload(w http.ResponseWriter, r *http.Request) {
	if !currentAccess(r).Can(users.MediaUpload) {
		h.forbidden(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.Media.MaxSize()*20+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		toast(w, "error", "Choose one or more files to upload.")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var uploaded int
	var failures []string
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			failures = append(failures, "the upload was interrupted")
			break
		}
		if part.FormName() != "files" || part.FileName() == "" {
			part.Close()
			continue
		}
		name := part.FileName()
		_, err = h.Media.Upload(r.Context(), currentSite(r).ID, currentUser(r).ID, name, part)
		part.Close()
		switch {
		case err == nil:
			uploaded++
		case errors.Is(err, media.ErrTooLarge):
			failures = append(failures, name+" is larger than "+strconv.FormatInt(h.Media.MaxSize()>>20, 10)+" MB")
		case errors.Is(err, media.ErrType):
			failures = append(failures, name+": "+err.Error())
		default:
			h.Log.Error("upload", "err", err)
			failures = append(failures, name+" could not be stored")
		}
	}
	switch {
	case len(failures) > 0 && uploaded > 0:
		toast(w, "error", strconv.Itoa(uploaded)+" uploaded. Not uploaded: "+joinMax(failures, 3))
	case len(failures) > 0:
		toast(w, "error", "Not uploaded: "+joinMax(failures, 3))
	default:
		toast(w, "success", strconv.Itoa(uploaded)+" "+map[bool]string{true: "file", false: "files"}[uploaded == 1]+" uploaded")
	}
	r.URL.RawQuery = r.URL.Query().Encode()
	d, err := h.mediaList(r, r.URL.Query().Get("picker") == "1")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.partial(w, http.StatusOK, "media", "grid", h.view(r, "", "media", d))
}

func joinMax(items []string, n int) string {
	out := ""
	for i, s := range items {
		if i == n {
			return out + " and " + strconv.Itoa(len(items)-n) + " more"
		}
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

func (h *Handler) mediaFile(w http.ResponseWriter, r *http.Request) (*media.File, bool) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	f, err := h.Media.Get(r.Context(), currentSite(r).ID, id)
	if errors.Is(err, media.ErrNotFound) {
		h.notFound(w, r)
		return nil, false
	}
	if err != nil {
		h.serverError(w, r, err)
		return nil, false
	}
	return f, true
}

func (h *Handler) mediaDetail(w http.ResponseWriter, r *http.Request) {
	if !currentAccess(r).Can(users.MediaRead) {
		h.forbidden(w, r)
		return
	}
	f, ok := h.mediaFile(w, r)
	if !ok {
		return
	}
	acc := currentAccess(r)
	usage, _ := h.Media.Usage(r.Context(), []int64{f.ID})
	d := &mediaData{File: f, Usage: usage, CanEdit: acc.Can(users.MediaUpdate), CanDelete: acc.Can(users.MediaDelete)}
	h.partial(w, http.StatusOK, "media", "detail", h.view(r, "", "media", d))
}

func (h *Handler) mediaUpdate(w http.ResponseWriter, r *http.Request) {
	if !currentAccess(r).Can(users.MediaUpdate) {
		h.forbidden(w, r)
		return
	}
	f, ok := h.mediaFile(w, r)
	if !ok {
		return
	}
	_, err := h.Media.Update(r.Context(), currentSite(r).ID, f.ID, r.PostFormValue("name"), r.PostFormValue("alt"), r.PostFormValue("caption"))
	if errs, _, isVE := formErrors(err); isVE {
		f.Name, f.Alt, f.Caption = r.PostFormValue("name"), r.PostFormValue("alt"), r.PostFormValue("caption")
		usage, _ := h.Media.Usage(r.Context(), []int64{f.ID})
		d := &mediaData{File: f, Usage: usage, CanEdit: true, CanDelete: currentAccess(r).Can(users.MediaDelete), Errors: errs}
		h.partial(w, http.StatusUnprocessableEntity, "media", "detail", h.view(r, "", "media", d))
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.done(w, r, sitePath(r, "/media"), "success", "File details saved")
}

func (h *Handler) mediaDelete(w http.ResponseWriter, r *http.Request) {
	if !currentAccess(r).Can(users.MediaDelete) {
		h.forbidden(w, r)
		return
	}
	f, ok := h.mediaFile(w, r)
	if !ok {
		return
	}
	if err := h.Media.Delete(r.Context(), currentSite(r).ID, f.ID); err != nil {
		h.serverError(w, r, err)
		return
	}
	h.done(w, r, sitePath(r, "/media"), "success", "Deleted "+f.Name)
}
