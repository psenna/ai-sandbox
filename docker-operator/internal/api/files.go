package api

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/psenna/ai-sandbox/docker-operator/internal/filestore"
)

// fileListResponse is the GET /api/files body. Entries is never nil (JSON
// "[]" for an empty directory); Parent is the slash path of the parent
// directory, "" at the root.
type fileListResponse struct {
	Path    string            `json:"path"`
	Parent  string            `json:"parent"`
	Entries []filestore.Entry `json:"entries"`
}

// mkdirRequest is the POST /api/files/mkdir body.
type mkdirRequest struct {
	Path string `json:"path"`
}

// uploadResponse is the POST /api/files/upload body: one entry per stored
// part.
type uploadResponse struct {
	Uploaded []filestore.Entry `json:"uploaded"`
}

// filestoreOr501 returns the configured file store, or writes a 501 and
// reports false when none is configured.
func (h *Handler) filestoreOr501(w http.ResponseWriter) (*filestore.Store, bool) {
	if h.files == nil {
		writeError(w, http.StatusNotImplemented, CodeFilestoreDisabled,
			"the centralized file store is not configured on this operator (set FILESTORE_DIR)", "")
		return nil, false
	}
	return h.files, true
}

// filestoreError maps a filestore sentinel error to the right HTTP response.
func (h *Handler) filestoreError(w http.ResponseWriter, action string, err error) {
	switch {
	case filestore.IsInvalidPath(err):
		writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error(), "path")
	case filestore.IsNotDir(err):
		writeError(w, http.StatusBadRequest, CodeInvalidParam, err.Error(), "path")
	case filestore.IsNotFound(err):
		writeError(w, http.StatusNotFound, CodeNotFound, "no such file or directory", "")
	case filestore.IsTooLarge(err):
		writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
			"the file is larger than the operator's upload limit", "file")
	default:
		h.internalError(w, action, err)
	}
}

// parentPath returns the slash path of p's parent directory, "" for the root
// or a top-level entry.
func parentPath(p string) string {
	p = strings.Trim(p, "/")
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ""
	}
	return p[:i]
}

func (h *Handler) handleFilesList(w http.ResponseWriter, r *http.Request) {
	fs, ok := h.filestoreOr501(w)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	entries, err := fs.List(p)
	if err != nil {
		h.filestoreError(w, "listing files at "+p, err)
		return
	}
	writeJSON(w, http.StatusOK, fileListResponse{
		Path:    strings.Trim(p, "/"),
		Parent:  parentPath(p),
		Entries: entries,
	})
}

func (h *Handler) handleFilesDownload(w http.ResponseWriter, r *http.Request) {
	fs, ok := h.filestoreOr501(w)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	f, entry, err := fs.Open(p)
	if err != nil {
		if errors.Is(err, filestore.ErrIsDir) {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, "cannot download a directory", "path")
			return
		}
		h.filestoreError(w, "opening "+p+" for download", err)
		return
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": entry.Name}))
	http.ServeContent(w, r, entry.Name, entry.ModTime, f)
}

func (h *Handler) handleFilesUpload(w http.ResponseWriter, r *http.Request) {
	fs, ok := h.filestoreOr501(w)
	if !ok {
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data") {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "upload requires a multipart/form-data body", "Content-Type")
		return
	}
	dir := r.URL.Query().Get("path")

	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "malformed multipart body: "+err.Error(), "")
		return
	}

	uploaded := []filestore.Entry{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParam, "reading multipart body: "+err.Error(), "")
			return
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		name := part.FileName()
		if !filestoreSafeName(name) {
			_ = part.Close()
			writeError(w, http.StatusBadRequest, CodeInvalidParam, "unsafe upload filename "+name, "path")
			return
		}
		entry, saveErr := fs.Save(joinFilePath(dir, name), part, h.maxUpload)
		_ = part.Close()
		if saveErr != nil {
			h.filestoreError(w, "saving upload "+name, saveErr)
			return
		}
		uploaded = append(uploaded, entry)
	}
	writeJSON(w, http.StatusOK, uploadResponse{Uploaded: uploaded})
}

func (h *Handler) handleFilesMkdir(w http.ResponseWriter, r *http.Request) {
	fs, ok := h.filestoreOr501(w)
	if !ok {
		return
	}
	var req mkdirRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadJSON, "the request body is not valid JSON: "+err.Error(), "")
		return
	}
	if strings.Trim(req.Path, "/") == "" {
		writeError(w, http.StatusBadRequest, CodeMissingField, `"path" must not be empty`, "path")
		return
	}
	if err := fs.Mkdir(req.Path); err != nil {
		h.filestoreError(w, "creating directory "+req.Path, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": strings.Trim(req.Path, "/"), "status": "created"})
}

func (h *Handler) handleFilesDelete(w http.ResponseWriter, r *http.Request) {
	fs, ok := h.filestoreOr501(w)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	if err := fs.Remove(p); err != nil {
		h.filestoreError(w, "deleting "+p, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": strings.Trim(p, "/"), "status": "deleted"})
}

// filestoreSafeName rejects an attacker-controlled multipart filename that is
// empty, "." / "..", or carries a path separator or control byte -- the same
// rule filestore's own segment validator applies, checked here so a bad name
// is a 400 rather than an opaque save error.
func filestoreSafeName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r == 0x00 || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// joinFilePath joins a directory path and a single filename with a slash.
func joinFilePath(dir, name string) string {
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
