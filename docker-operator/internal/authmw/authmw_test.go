package authmw

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// okHandler is the wrapped handler: it writes a marker so a test can tell "the
// request reached next" from "the middleware rejected it".
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("reached-next"))
})

const validBearer = "operator-bearer-value-abc123"

func do(t *testing.T, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestRequireBearer_Disabled(t *testing.T) {
	h := RequireBearer("", okHandler)
	rec := do(t, h, httptest.NewRequest("GET", "/api/agents", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "reached-next" {
		t.Fatalf("empty token must pass through: code=%d body=%q", rec.Code, rec.Body)
	}
}

func TestRequireBearer_Header(t *testing.T) {
	h := RequireBearer(validBearer, okHandler)

	t.Run("valid Bearer reaches next", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/api/agents", nil)
		r.Header.Set("Authorization", "Bearer "+validBearer)
		if rec := do(t, h, r); rec.Code != http.StatusOK {
			t.Fatalf("code=%d, want 200; body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("missing header is 401 with the error envelope", func(t *testing.T) {
		rec := do(t, h, httptest.NewRequest("GET", "/api/agents", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code=%d, want 401", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"code":"unauthorized"`) {
			t.Errorf("body = %s, want the {\"error\":{\"code\":\"unauthorized\"}} envelope", rec.Body)
		}
		if rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("WWW-Authenticate = %q, want %q", rec.Header().Get("WWW-Authenticate"), "Bearer")
		}
		if rec.Body.String() == "reached-next" {
			t.Error("request reached next despite no credential")
		}
	})

	t.Run("wrong token is 401", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/api/agents", nil)
		r.Header.Set("Authorization", "Bearer not-the-token")
		if rec := do(t, h, r); rec.Code != http.StatusUnauthorized {
			t.Fatalf("code=%d, want 401", rec.Code)
		}
	})

	t.Run("non-Bearer scheme is 401", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/api/agents", nil)
		r.Header.Set("Authorization", "Basic "+validBearer)
		if rec := do(t, h, r); rec.Code != http.StatusUnauthorized {
			t.Fatalf("code=%d, want 401", rec.Code)
		}
	})

	t.Run("empty Bearer value is 401", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/api/agents", nil)
		r.Header.Set("Authorization", "Bearer ")
		if rec := do(t, h, r); rec.Code != http.StatusUnauthorized {
			t.Fatalf("code=%d, want 401", rec.Code)
		}
	})
}

func TestRequireBearer_QueryParamOnlyForWebSocket(t *testing.T) {
	h := RequireBearer(validBearer, okHandler)

	wsReq := func(token string) *http.Request {
		r := httptest.NewRequest("GET", "/ws/agents/agt_1/terminal?token="+token, nil)
		r.Header.Set("Connection", "keep-alive, Upgrade")
		r.Header.Set("Upgrade", "websocket")
		return r
	}

	t.Run("valid ?token= on a WS handshake reaches next", func(t *testing.T) {
		if rec := do(t, h, wsReq(validBearer)); rec.Code != http.StatusOK {
			t.Fatalf("code=%d, want 200; body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("wrong ?token= on a WS handshake is 401", func(t *testing.T) {
		if rec := do(t, h, wsReq("nope")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("code=%d, want 401", rec.Code)
		}
	})

	t.Run("?token= is ignored on a plain (non-upgrade) request", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/api/agents?token="+validBearer, nil)
		if rec := do(t, h, r); rec.Code != http.StatusUnauthorized {
			t.Fatalf("code=%d, want 401 (query token must not authenticate a non-WS call)", rec.Code)
		}
	})

	t.Run("header still wins on a WS handshake", func(t *testing.T) {
		r := wsReq("nope")
		r.Header.Set("Authorization", "Bearer "+validBearer)
		if rec := do(t, h, r); rec.Code != http.StatusOK {
			t.Fatalf("code=%d, want 200 (a valid header authenticates regardless of a bad query token)", rec.Code)
		}
	})
}
