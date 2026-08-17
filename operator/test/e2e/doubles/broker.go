package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// brokerState is the fake git-proxy broker's in-memory canned-response
// store, keyed by the exact request path (e.g.
// "/org/repo/checks/refs/heads/feat/x"). PUT /_control/state programs an
// entry; DELETE /_control/state clears everything; GET routes fall back to
// a documented per-endpoint default when no entry is programmed.
type brokerState struct {
	mu    sync.RWMutex
	byKey map[string]storedResponse
	log   []RecordedRequest
}

type storedResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type controlPut struct {
	Path   string          `json:"path"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func newBrokerState() *brokerState {
	return &brokerState{byKey: map[string]storedResponse{}}
}

func (s *brokerState) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, RecordedRequest{Method: r.Method, Path: r.URL.Path, Time: time.Now().UTC()})
}

func (s *brokerState) requests() []RecordedRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]RecordedRequest, len(s.log))
	copy(out, s.log)
	return out
}

func (s *brokerState) get(path string) (storedResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.byKey[path]
	return v, ok
}

func (s *brokerState) put(path string, resp storedResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byKey[path] = resp
}

func (s *brokerState) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byKey = map[string]storedResponse{}
	s.log = nil
}

// brokerToken returns the bearer token every non-control, non-healthz
// broker route requires, defaulting to "e2e-token" (matching the
// git-proxy-token Secret the e2e manifests provision).
func brokerToken() string {
	if v := os.Getenv("BROKER_TOKEN"); v != "" {
		return v
	}
	return "e2e-token"
}

// runBroker starts the fake git-proxy broker and blocks until it exits.
func runBroker() error {
	state := newBrokerState()
	token := brokerToken()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("PUT /_control/state", func(w http.ResponseWriter, r *http.Request) {
		var in controlPut
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if in.Path == "" {
			http.Error(w, `"path" is required`, http.StatusBadRequest)
			return
		}
		status := in.Status
		if status == 0 {
			status = http.StatusOK
		}
		state.put(in.Path, storedResponse{Status: status, Body: in.Body})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /_control/state", func(w http.ResponseWriter, _ *http.Request) {
		state.reset()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /_control/requests", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, state.requests())
	})

	mux.HandleFunc("GET /{owner}/{repo}/prs/{n}", func(w http.ResponseWriter, r *http.Request) {
		serveCanned(w, r, state, http.StatusNotFound, nil)
	})
	mux.HandleFunc("GET /{owner}/{repo}/prs", func(w http.ResponseWriter, r *http.Request) {
		serveCanned(w, r, state, http.StatusOK, []PRState{})
	})
	mux.HandleFunc("GET /{owner}/{repo}/checks/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		serveCanned(w, r, state, http.StatusOK, CheckSummary{Overall: "pending", Checks: []CheckRun{}, Workflows: []WorkflowRun{}})
	})
	mux.HandleFunc("GET /{owner}/{repo}/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		serveCanned(w, r, state, http.StatusNotFound, nil)
	})

	handler := recordAndAuth(state, token, mux)

	addr := ":8080"
	log.Printf("broker: listening on %s", addr)
	return http.ListenAndServe(addr, handler)
}

// serveCanned looks up a programmed response for r.URL.Path; if none is
// programmed it serves defaultStatus/defaultBody (defaultBody == nil means
// "empty body, defaultStatus only", used for the 404 cases).
func serveCanned(w http.ResponseWriter, r *http.Request, state *brokerState, defaultStatus int, defaultBody any) {
	if resp, ok := state.get(r.URL.Path); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.Status)
		if len(resp.Body) > 0 {
			_, _ = w.Write(resp.Body)
		}
		return
	}
	if defaultBody == nil {
		http.Error(w, "not programmed", defaultStatus)
		return
	}
	writeJSON(w, defaultStatus, defaultBody)
}

// recordAndAuth wraps next with request logging (all routes) and Bearer
// auth (every route except /healthz and /_control/*).
func recordAndAuth(state *brokerState, token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_control/requests" {
			state.record(r)
		}
		if r.URL.Path == "/healthz" || isControlPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("Authorization")
		if got != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isControlPath(p string) bool {
	return len(p) >= len("/_control") && p[:len("/_control")] == "/_control"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "broker: encoding response: %v\n", err)
	}
}
