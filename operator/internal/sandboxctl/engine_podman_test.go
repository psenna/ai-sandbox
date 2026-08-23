package sandboxctl

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func noopSleep(context.Context, time.Duration) error { return nil }

// newTestPodmanTeardown builds a *podmanTeardown against srv, with a no-op
// sleep (tests must not spend real wall-clock time on retry backoff).
func newTestPodmanTeardown(srv *httptest.Server) *podmanTeardown {
	return &podmanTeardown{
		baseURL: srv.URL,
		client:  srv.Client(),
		log:     logr.Discard(),
		sleep:   noopSleep,
	}
}

func TestPodmanTeardown_StopsAndRemovesEveryContainer(t *testing.T) {
	var stopped, removed []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.41/containers/json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]dockerContainer{
			{ID: "id-a", Names: []string{"/container-a"}},
			{ID: "id-b", Names: []string{"/container-b"}},
		})
	})
	mux.HandleFunc("/v1.41/containers/id-a/stop", func(w http.ResponseWriter, r *http.Request) {
		stopped = append(stopped, "id-a")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1.41/containers/id-b/stop", func(w http.ResponseWriter, r *http.Request) {
		stopped = append(stopped, "id-b")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1.41/containers/id-a", func(w http.ResponseWriter, r *http.Request) {
		removed = append(removed, "id-a")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1.41/containers/id-b", func(w http.ResponseWriter, r *http.Request) {
		removed = append(removed, "id-b")
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newTestPodmanTeardown(srv)
	report, err := p.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(stopped) != 2 || len(removed) != 2 {
		t.Errorf("stopped=%v removed=%v, want both containers stopped and removed", stopped, removed)
	}
	if report.Engine != "rootless-podman" {
		t.Errorf("Engine = %q, want rootless-podman", report.Engine)
	}
	wantNames := []string{"container-a", "container-b"}
	if len(report.Containers) != len(wantNames) {
		t.Fatalf("Containers = %v, want %v", report.Containers, wantNames)
	}
	for i, n := range wantNames {
		if report.Containers[i] != n {
			t.Errorf("Containers[%d] = %q, want %q", i, report.Containers[i], n)
		}
	}
}

func TestPodmanTeardown_ContainerNamesAreSortedAndDeterministic(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.41/containers/json", func(w http.ResponseWriter, r *http.Request) {
		// Deliberately shuffled API order.
		_ = json.NewEncoder(w).Encode([]dockerContainer{
			{ID: "id-z", Names: []string{"/zebra"}},
			{ID: "id-a", Names: []string{"/apple"}},
			{ID: "id-m", Names: []string{"/mango"}},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newTestPodmanTeardown(srv)
	report, err := p.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	want := []string{"apple", "mango", "zebra"}
	if len(report.Containers) != len(want) {
		t.Fatalf("Containers = %v, want %v", report.Containers, want)
	}
	for i, n := range want {
		if report.Containers[i] != n {
			t.Errorf("Containers[%d] = %q, want %q (not sorted/deterministic)", i, report.Containers[i], n)
		}
	}
}

func TestPodmanTeardown_NoContainersIsANoOp(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.41/containers/json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]dockerContainer{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newTestPodmanTeardown(srv)
	report, err := p.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(report.Containers) != 0 {
		t.Errorf("Containers = %v, want empty", report.Containers)
	}
	if report.Note == "" {
		t.Error("Note is empty, want a human-readable explanation")
	}
}

func TestPodmanTeardown_ConnectionRefusedIsANoOpNotAFailure(t *testing.T) {
	// Bind then immediately close a loopback listener: connecting to its
	// address afterward reliably yields ECONNREFUSED, without a real podman
	// service or any wall-clock sleep.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing listener: %v", err)
	}

	p := &podmanTeardown{
		baseURL: "http://" + addr,
		client:  &http.Client{Timeout: 2 * time.Second},
		log:     logr.Discard(),
		sleep:   noopSleep,
	}
	report, err := p.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown: %v, want nil error (connection refused is a no-op)", err)
	}
	if report.Engine != "rootless-podman" {
		t.Errorf("Engine = %q, want rootless-podman", report.Engine)
	}
	if len(report.Containers) != 0 {
		t.Errorf("Containers = %v, want empty", report.Containers)
	}
	if !strings.Contains(report.Note, "not listening") {
		t.Errorf("Note = %q, want it to explain the engine is not listening", report.Note)
	}
}

func TestPodmanTeardown_ServerErrorRetriesThenFails(t *testing.T) {
	var requests int
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.41/containers/json", func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newTestPodmanTeardown(srv)
	_, err := p.Teardown(context.Background())
	if err == nil {
		t.Fatal("Teardown: expected error, got nil")
	}
	if requests != podmanTeardownAttempts {
		t.Errorf("requests = %d, want exactly podmanTeardownAttempts (%d)", requests, podmanTeardownAttempts)
	}
}

func TestPodmanTeardown_TreatsNotFoundAsAlreadyGone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.41/containers/json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]dockerContainer{{ID: "id-gone", Names: []string{"/gone"}}})
	})
	mux.HandleFunc("/v1.41/containers/id-gone/stop", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/v1.41/containers/id-gone", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newTestPodmanTeardown(srv)
	report, err := p.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown: %v, want nil (404 on stop/remove means already gone)", err)
	}
	if len(report.Containers) != 1 || report.Containers[0] != "gone" {
		t.Errorf("Containers = %v, want [gone]", report.Containers)
	}
}

func TestPodmanTeardown_RemoveUsesForceAndVolumes(t *testing.T) {
	var removeQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.41/containers/json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]dockerContainer{{ID: "id-x", Names: []string{"/x"}}})
	})
	mux.HandleFunc("/v1.41/containers/id-x/stop", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1.41/containers/id-x", func(w http.ResponseWriter, r *http.Request) {
		removeQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newTestPodmanTeardown(srv)
	if _, err := p.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if !strings.Contains(removeQuery, "force=true") || !strings.Contains(removeQuery, "v=true") {
		t.Errorf("remove query = %q, want it to contain force=true and v=true", removeQuery)
	}
}

func TestNewEngineTeardown_SelectsPodmanForRootlessPodman(t *testing.T) {
	got := NewEngineTeardown("rootless-podman", "tcp://127.0.0.1:2375", logr.Discard())
	if _, ok := got.(*podmanTeardown); !ok {
		t.Errorf("NewEngineTeardown(rootless-podman, ...) = %T, want *podmanTeardown", got)
	}

	for _, engine := range []string{"", "none"} {
		got := NewEngineTeardown(engine, "", logr.Discard())
		if _, ok := got.(noopEngineTeardown); !ok {
			t.Errorf("NewEngineTeardown(%q, ...) = %T, want noopEngineTeardown", engine, got)
		}
	}

	failed := NewEngineTeardown("rootless-podman", "://not-a-valid-url", logr.Discard())
	if _, ok := failed.(failedEngineTeardown); !ok {
		t.Fatalf("NewEngineTeardown with an unparseable endpoint = %T, want failedEngineTeardown", failed)
	}
	if _, err := failed.Teardown(context.Background()); err == nil {
		t.Error("failedEngineTeardown.Teardown(): expected error, got nil")
	}
}
