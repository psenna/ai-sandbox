package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNew_ParsesImageRef(t *testing.T) {
	cases := []struct {
		name     string
		image    string
		baseURL  string
		wantBase string
		wantRepo string
	}{
		{
			name:     "ghcr default base",
			image:    "ghcr.io/psenna/ai-sandbox-agent:latest",
			wantBase: "https://ghcr.io",
			wantRepo: "psenna/ai-sandbox-agent",
		},
		{
			name:     "host:port with a tag",
			image:    "host:5000/team/img:tag",
			wantBase: "https://host:5000",
			wantRepo: "team/img",
		},
		{
			name:     "digest reference",
			image:    "ghcr.io/psenna/ai-sandbox-agent@sha256:" + strings.Repeat("a", 64),
			wantBase: "https://ghcr.io",
			wantRepo: "psenna/ai-sandbox-agent",
		},
		{
			name:     "docker.io is special-cased",
			image:    "library/alpine:3",
			wantBase: "https://registry-1.docker.io",
			wantRepo: "library/alpine",
		},
		{
			name:     "explicit base URL wins, trailing slash trimmed",
			image:    "ghcr.io/psenna/ai-sandbox-agent:latest",
			baseURL:  "http://localhost:5000/",
			wantBase: "http://localhost:5000",
			wantRepo: "psenna/ai-sandbox-agent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(Options{Image: tc.image, BaseURL: tc.baseURL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if c.base != tc.wantBase {
				t.Errorf("base = %q, want %q", c.base, tc.wantBase)
			}
			if c.repo != tc.wantRepo {
				t.Errorf("repo = %q, want %q", c.repo, tc.wantRepo)
			}
		})
	}
}

func TestNew_RejectsAGarbageImageRef(t *testing.T) {
	if _, err := New(Options{Image: "not a valid ref!!"}); err == nil {
		t.Fatal("New with a garbage image ref = nil error, want one")
	}
}

// newClient points an HTTPClient at srv with a short-timeout http.Client.
func newClient(t *testing.T, srv *httptest.Server, opts Options) *HTTPClient {
	t.Helper()
	opts.Image = "example.test/team/img:latest"
	opts.BaseURL = srv.URL
	opts.HTTP = &http.Client{Timeout: 2 * time.Second}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestListTags_AnonymousTokenDance(t *testing.T) {
	var (
		tokenService string
		tokenScope   string
		listAuth     string
		listCalls    int
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenService = r.URL.Query().Get("service")
		tokenScope = r.URL.Query().Get("scope")
		_, _ = w.Write([]byte(`{"token":"anon-tok","access_token":"ignored"}`))
	})
	mux.HandleFunc("/v2/team/img/tags/list", func(w http.ResponseWriter, r *http.Request) {
		listCalls++
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+baseOf(r)+`/token",service="registry.example",scope="repository:team/img:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		listAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"name":"team/img","tags":["a","b"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newClient(t, srv, Options{})
	tags, err := c.ListTags(context.Background())
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if fmt.Sprint(tags) != "[a b]" {
		t.Errorf("tags = %v, want [a b]", tags)
	}
	if tokenService != "registry.example" || tokenScope != "repository:team/img:pull" {
		t.Errorf("token request carried service=%q scope=%q, want the challenge values", tokenService, tokenScope)
	}
	if listAuth != "Bearer anon-tok" {
		t.Errorf("retried list request Authorization = %q, want %q", listAuth, "Bearer anon-tok")
	}
	if listCalls != 2 {
		t.Errorf("tags/list was called %d times, want 2 (challenge then retry)", listCalls)
	}
}

func TestListTags_StaticAuthTokenSkipsTheChallenge(t *testing.T) {
	tokenCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(http.ResponseWriter, *http.Request) { tokenCalls++ })
	mux.HandleFunc("/v2/team/img/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer static-tok" {
			t.Errorf("Authorization = %q, want the static token up front", got)
		}
		_, _ = w.Write([]byte(`{"tags":["x"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newClient(t, srv, Options{AuthToken: "static-tok"})
	tags, err := c.ListTags(context.Background())
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if fmt.Sprint(tags) != "[x]" {
		t.Errorf("tags = %v, want [x]", tags)
	}
	if tokenCalls != 0 {
		t.Errorf("token endpoint was hit %d times, want 0", tokenCalls)
	}
}

func TestListTags_FollowsLinkPagination(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/team/img/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("last") == "" {
			w.Header().Set("Link", `</v2/team/img/tags/list?last=b&n=2>; rel="next"`)
			_, _ = w.Write([]byte(`{"tags":["a","b"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"tags":["c"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newClient(t, srv, Options{})
	tags, err := c.ListTags(context.Background())
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if fmt.Sprint(tags) != "[a b c]" {
		t.Errorf("tags = %v, want [a b c] (both pages, in order)", tags)
	}
}

func TestListTags_ErrorClassification(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    error
	}{
		{
			name: "404 is ErrRepoNotFound",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			want: ErrRepoNotFound,
		},
		{
			name: "401 after a successful token fetch is ErrAuth",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_, _ = w.Write([]byte(`{"token":"t"}`))
					return
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+baseOf(r)+`/token",service="s"`)
				w.WriteHeader(http.StatusUnauthorized)
			},
			want: ErrAuth,
		},
		{
			name: "500 is ErrOffline",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			want: ErrOffline,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c := newClient(t, srv, Options{})
			_, err := c.ListTags(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("ListTags error = %v, want errors.Is(_, %v)", err, tc.want)
			}
		})
	}
}

func TestListTags_OfflineIsErrOffline(t *testing.T) {
	c, err := New(Options{Image: "example.test/team/img:latest", BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: time.Second}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.ListTags(context.Background())
	if !errors.Is(err, ErrOffline) {
		t.Fatalf("ListTags against a dead address = %v, want errors.Is(_, ErrOffline)", err)
	}
}

func TestListTags_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newClient(t, srv, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := c.ListTags(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ListTags with a cancelled context = %v, want context.Canceled", err)
	}
}

// baseOf reconstructs the server's own base URL from a request, so a handler
// can hand back an absolute realm without closing over the httptest.Server.
func baseOf(r *http.Request) string {
	return "http://" + r.Host
}
