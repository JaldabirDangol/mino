package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"mino/internal/apitokens"
	"mino/internal/media"
)

// maxFilesPerUpload bounds one multipart request.
const maxFilesPerUpload = 20

func (h *Handler) listFiles(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, apitokens.UploadRead) {
		return
	}
	qv := r.URL.Query()
	page, _ := strconv.Atoi(qv.Get("pagination[page]"))
	size, _ := strconv.Atoi(qv.Get("pagination[pageSize]"))
	q := media.ListQuery{Search: qv.Get("search"), Category: qv.Get("type"), Page: max(page, 1), PageSize: size}
	if q.PageSize < 1 || q.PageSize > 100 {
		q.PageSize = 25
	}
	files, total, err := h.Media.List(r.Context(), siteFrom(r).ID, q)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	data := make([]map[string]any, len(files))
	for i, f := range files {
		data[i] = f.JSON()
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "meta": map[string]any{"pagination": map[string]int{
		"page": q.Page, "pageSize": q.PageSize, "pageCount": (total + q.PageSize - 1) / q.PageSize, "total": total,
	}}})
}

func (h *Handler) getFile(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, apitokens.UploadRead) {
		return
	}
	id, ok := entryID(w, r)
	if !ok {
		return
	}
	f, err := h.Media.Get(r.Context(), siteFrom(r).ID, id)
	if err != nil {
		h.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": f.JSON(), "meta": map[string]any{}})
}

func (h *Handler) deleteFile(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, apitokens.UploadWrite) {
		return
	}
	id, ok := entryID(w, r)
	if !ok {
		return
	}
	if err := h.Media.Delete(r.Context(), siteFrom(r).ID, id); err != nil {
		h.handleErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// upload accepts multipart/form-data with one or more parts named "files". Parts are streamed
// to temporary files, so memory use does not grow with upload size.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, apitokens.UploadWrite) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.Media.MaxSize()*maxFilesPerUpload+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "ValidationError", "Expected multipart/form-data with a \"files\" field", nil)
		return
	}
	var out []map[string]any
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "ValidationError", "Malformed multipart body", nil)
			return
		}
		if part.FormName() != "files" || part.FileName() == "" {
			part.Close()
			continue
		}
		if len(out) == maxFilesPerUpload {
			writeError(w, http.StatusBadRequest, "ValidationError", "Too many files in one request", nil)
			return
		}
		f, err := h.Media.Upload(r.Context(), siteFrom(r).ID, 0, part.FileName(), part)
		part.Close()
		if err != nil {
			h.handleErr(w, r, err)
			return
		}
		out = append(out, f.JSON())
	}
	if len(out) == 0 {
		writeError(w, http.StatusBadRequest, "ValidationError", "No files received; send them in a \"files\" field", nil)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"data": out, "meta": map[string]any{}})
}
