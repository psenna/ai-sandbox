package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/filestore"
)

func newFilestore(t *testing.T) *filestore.Store {
	t.Helper()
	s, err := filestore.New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("filestore.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestFiles_Disabled(t *testing.T) {
	h := newTestHandler(newFakeManager(5), dockerclienttest.New())
	cases := []struct {
		method, path string
	}{
		{"GET", "/api/files"},
		{"DELETE", "/api/files"},
		{"GET", "/api/files/download?path=x"},
		{"POST", "/api/files/upload"},
		{"POST", "/api/files/mkdir"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := doJSON(t, h, tc.method, tc.path, nil)
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501; body %s", rec.Code, rec.Body)
			}
			if got := decodeEnvelope(t, rec).Error.Code; got != CodeFilestoreDisabled {
				t.Errorf("code = %q, want %q", got, CodeFilestoreDisabled)
			}
		})
	}
}

func TestFiles_MethodNotAllowed(t *testing.T) {
	h := newTestHandlerFiles(newFakeManager(5), dockerclienttest.New(), newFilestore(t), 1<<20)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/files"},
		{"DELETE", "/api/files/upload"},
	} {
		rec := doJSON(t, h, tc.method, tc.path, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s status = %d, want 405", tc.method, tc.path, rec.Code)
		}
		if got := decodeEnvelope(t, rec).Error.Code; got != CodeMethodNotAllowed {
			t.Errorf("%s %s code = %q, want method_not_allowed", tc.method, tc.path, got)
		}
	}
}

func TestFiles_List(t *testing.T) {
	fs := newFilestore(t)
	if err := fs.Mkdir("agents/agt_1/sub"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Save("agents/agt_1/a.txt", strings.NewReader("hi"), 1<<20); err != nil {
		t.Fatal(err)
	}
	h := newTestHandlerFiles(newFakeManager(5), dockerclienttest.New(), fs, 1<<20)

	rec := doJSON(t, h, "GET", "/api/files?path=agents/agt_1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var resp fileListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Parent != "agents" {
		t.Errorf("parent = %q, want agents", resp.Parent)
	}
	if len(resp.Entries) != 2 || !resp.Entries[0].IsDir || resp.Entries[0].Name != "sub" || resp.Entries[1].Name != "a.txt" {
		t.Errorf("entries = %+v, want sub (dir) then a.txt", resp.Entries)
	}

	if err := fs.Mkdir("empty"); err != nil {
		t.Fatal(err)
	}
	rec = doJSON(t, h, "GET", "/api/files?path=empty", nil)
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"entries":[]`)) {
		t.Errorf("empty dir body = %s, want entries:[]", rec.Body.String())
	}
}

func TestFiles_ListTraversal(t *testing.T) {
	h := newTestHandlerFiles(newFakeManager(5), dockerclienttest.New(), newFilestore(t), 1<<20)
	for _, p := range []string{"../../etc", "/etc/passwd"} {
		rec := doJSON(t, h, "GET", "/api/files?path="+p, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("path=%q status = %d, want 400", p, rec.Code)
		}
	}
}

func TestFiles_Download(t *testing.T) {
	fs := newFilestore(t)
	if err := fs.Mkdir("d"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Save("d/f.bin", bytes.NewReader([]byte("binary-bytes")), 1<<20); err != nil {
		t.Fatal(err)
	}
	h := newTestHandlerFiles(newFakeManager(5), dockerclienttest.New(), fs, 1<<20)

	rec := doJSON(t, h, "GET", "/api/files/download?path=d/f.bin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	if rec.Body.String() != "binary-bytes" {
		t.Errorf("body = %q", rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "f.bin") {
		t.Errorf("Content-Disposition = %q, want the filename", cd)
	}

	if rec := doJSON(t, h, "GET", "/api/files/download?path=d", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("download of a dir status = %d, want 400", rec.Code)
	}
	if rec := doJSON(t, h, "GET", "/api/files/download?path=d/missing", nil); rec.Code != http.StatusNotFound {
		t.Errorf("download of a missing file status = %d, want 404", rec.Code)
	}
}

// TestFiles_ListRegularFile: listing a path that is a file, not a directory,
// is a client mistake -> 400 invalid_param, not a 500.
func TestFiles_ListRegularFile(t *testing.T) {
	fs := newFilestore(t)
	if err := fs.Mkdir("d"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Save("d/f", bytes.NewReader([]byte("x")), 1<<20); err != nil {
		t.Fatal(err)
	}
	h := newTestHandlerFiles(newFakeManager(5), dockerclienttest.New(), fs, 1<<20)
	rec := doJSON(t, h, "GET", "/api/files?path=d/f", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("list of a regular file status = %d, want 400; body %s", rec.Code, rec.Body)
	}
}

func uploadBody(t *testing.T, files map[string]string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, content := range files {
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return mw.FormDataContentType(), &buf
}

func TestFiles_Upload(t *testing.T) {
	fs := newFilestore(t)
	if err := fs.Mkdir("agents/agt_1"); err != nil {
		t.Fatal(err)
	}
	h := newTestHandlerFiles(newFakeManager(5), dockerclienttest.New(), fs, 32)

	ct, body := uploadBody(t, map[string]string{"one.txt": "AAA", "two.txt": "BBB"})
	r := httptest.NewRequest("POST", "/api/files/upload?path=agents/agt_1", body)
	r.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	for name, want := range map[string]string{"one.txt": "AAA", "two.txt": "BBB"} {
		b, err := os.ReadFile(filepath.Join(fs.Root(), "agents", "agt_1", name)) //nolint:gosec // G304: test reads back a file it just wrote under a t.TempDir() root
		if err != nil || string(b) != want {
			t.Errorf("%s on disk = %q / %v, want %q", name, b, err, want)
		}
	}

	// An unsafe part filename is a 400. Go's multipart reader passes
	// FileName() through filepath.Base, so a "/"-bearing name never reaches
	// the handler intact; ".." and a backslash survive it and must be
	// rejected here.
	for _, bad := range []string{"..", `ev\il`} {
		ct, body := uploadBody(t, map[string]string{bad: "x"})
		r := httptest.NewRequest("POST", "/api/files/upload?path=agents/agt_1", body)
		r.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("filename %q status = %d, want 400", bad, rec.Code)
		}
	}

	// Over the cap -> 413, nothing written.
	ct, body = uploadBody(t, map[string]string{"huge.txt": strings.Repeat("x", 64)})
	r = httptest.NewRequest("POST", "/api/files/upload?path=agents/agt_1", body)
	r.Header.Set("Content-Type", ct)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap status = %d, want 413; body %s", rec.Code, rec.Body)
	}
	if got := decodeEnvelope(t, rec).Error.Code; got != CodePayloadTooLarge {
		t.Errorf("code = %q, want payload_too_large", got)
	}
	if _, err := os.Stat(filepath.Join(fs.Root(), "agents", "agt_1", "huge.txt")); !os.IsNotExist(err) {
		t.Errorf("over-cap upload left a partial file: %v", err)
	}
}

func TestFiles_UploadRequiresMultipart(t *testing.T) {
	h := newTestHandlerFiles(newFakeManager(5), dockerclienttest.New(), newFilestore(t), 1<<20)
	r := httptest.NewRequest("POST", "/api/files/upload", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestFiles_Mkdir(t *testing.T) {
	fs := newFilestore(t)
	h := newTestHandlerFiles(newFakeManager(5), dockerclienttest.New(), fs, 1<<20)

	rec := doJSON(t, h, "POST", "/api/files/mkdir", mkdirRequest{Path: "agents/agt_1/new"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(fs.Root(), "agents", "agt_1", "new")); err != nil {
		t.Errorf("dir not created: %v", err)
	}
	// Idempotent.
	if rec := doJSON(t, h, "POST", "/api/files/mkdir", mkdirRequest{Path: "agents/agt_1/new"}); rec.Code != http.StatusOK {
		t.Errorf("second mkdir status = %d, want 200", rec.Code)
	}
	// Empty path.
	rec = doJSON(t, h, "POST", "/api/files/mkdir", mkdirRequest{Path: ""})
	if rec.Code != http.StatusBadRequest || decodeEnvelope(t, rec).Error.Code != CodeMissingField {
		t.Errorf("empty path: status %d code %q, want 400 missing_field", rec.Code, decodeEnvelope(t, rec).Error.Code)
	}
	// Unknown JSON field.
	r := httptest.NewRequest("POST", "/api/files/mkdir", bytes.NewReader([]byte(`{"pat":"x"}`)))
	r.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest || decodeEnvelope(t, rec).Error.Code != CodeBadJSON {
		t.Errorf("unknown field: status %d, want 400 bad_json", rec.Code)
	}
}

func TestFiles_Delete(t *testing.T) {
	fs := newFilestore(t)
	if err := fs.Mkdir("agents/agt_1/sub"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Save("agents/agt_1/f", strings.NewReader("x"), 1<<20); err != nil {
		t.Fatal(err)
	}
	h := newTestHandlerFiles(newFakeManager(5), dockerclienttest.New(), fs, 1<<20)

	if rec := doJSON(t, h, "DELETE", "/api/files?path=agents/agt_1/f", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete file status = %d; body %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, "DELETE", "/api/files?path=agents/agt_1", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete tree status = %d", rec.Code)
	}
	// Already gone -> still 200.
	if rec := doJSON(t, h, "DELETE", "/api/files?path=agents/agt_1", nil); rec.Code != http.StatusOK {
		t.Errorf("delete already-gone status = %d, want 200", rec.Code)
	}
	// Structural paths -> 400.
	for _, p := range []string{"", "agents"} {
		if rec := doJSON(t, h, "DELETE", "/api/files?path="+p, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("delete path=%q status = %d, want 400", p, rec.Code)
		}
	}
}
