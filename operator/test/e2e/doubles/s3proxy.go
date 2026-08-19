package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// s3ProxyFault is the programmable failure mode for runS3Proxy.
type s3ProxyFault struct {
	mu         sync.RWMutex
	mode       string // "none" | "fail-write" | "corrupt-read" | "truncate-read"
	status     int
	afterBytes int64
	pathSuffix string // when non-empty, read faults apply only to paths ending in it
}

func (f *s3ProxyFault) get() (mode string, status int, afterBytes int64, pathSuffix string) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.mode, f.status, f.afterBytes, f.pathSuffix
}

func (f *s3ProxyFault) set(mode string, status int, afterBytes int64, pathSuffix string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = mode
	f.status = status
	f.afterBytes = afterBytes
	f.pathSuffix = pathSuffix
}

type s3ProxyFaultRequest struct {
	Mode       string `json:"mode"`
	Status     int    `json:"status"`
	AfterBytes int64  `json:"afterBytes"`
	PathSuffix string `json:"pathSuffix"`
}

// s3ProxyState is the request log, separate from brokerState (different
// mode, different process instance of this binary) but the same shape.
type s3ProxyState struct {
	mu  sync.Mutex
	log []RecordedRequest
}

func (s *s3ProxyState) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, RecordedRequest{Method: r.Method, Path: r.URL.Path, Time: time.Now().UTC()})
}

func (s *s3ProxyState) requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RecordedRequest, len(s.log))
	copy(out, s.log)
	return out
}

// s3ProxyUpstream returns the real in-cluster MinIO this proxy sits in
// front of.
func s3ProxyUpstream() string {
	if v := os.Getenv("S3_UPSTREAM"); v != "" {
		return v
	}
	return "http://minio.ai-sandbox-e2e.svc.cluster.local:9000"
}

// runS3Proxy is a programmable reverse proxy in front of the e2e MinIO. It
// exists for exactly one thing the broker/model doubles could not do:
// inject a backend failure MID-UPLOAD, so #28's freeze failure path is
// tested against a real S3 client and a real streaming multipart upload,
// not a unit-test fake.
//
//	PUT    /_control/fault  {"mode":"none|fail-write|corrupt-read|truncate-read","status":503,"afterBytes":N,"pathSuffix":"..."}
//	DELETE /_control/fault
//	GET    /_control/requests
//
// mode=fail-write: any PUT/POST first DRAINS afterBytes of the request body
// -- so the client really is mid-stream -- then answers with the configured
// status (default 503) and an S3-shaped XML <Error><Code>InternalError
// </Code> body, which internal/storage's classifyS3Error maps to
// ErrUnreachable and therefore to a RETRYABLE failure. GET/HEAD/DELETE
// always pass through unmodified, so listing/reading the (uncorrupted)
// upstream state is unaffected by the fault.
//
// mode=corrupt-read / truncate-read (#29): applied to GET responses via
// ModifyResponse, so the upstream object is never touched -- the fault is
// purely on the wire, exactly where a real network corruption would land.
// corrupt-read flips one byte of the response body at offset afterBytes;
// truncate-read serves only the first afterBytes bytes and closes the
// stream early, exercising precisely the truncated-tail case
// internal/storage's RestoreFrom drain-before-Verify ordering exists for.
// pathSuffix, when non-empty, restricts the fault to GETs whose path ends
// in it -- so a spec can corrupt workspace.tar.zst while leaving
// manifest.json intact (a corrupted manifest would fail at PARSE time with
// a different reason; the checksum path must be proven on its own).
func runS3Proxy() error {
	upstream := s3ProxyUpstream()
	upstreamURL, err := url.Parse(upstream)
	if err != nil {
		return fmt.Errorf("s3proxy: parsing upstream %q: %w", upstream, err)
	}

	fault := &s3ProxyFault{mode: "none"}
	state := &s3ProxyState{}
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)

	proxy.ModifyResponse = func(resp *http.Response) error {
		mode, _, afterBytes, pathSuffix := fault.get()
		if mode != "corrupt-read" && mode != "truncate-read" {
			return nil
		}
		if pathSuffix != "" && !strings.HasSuffix(resp.Request.URL.Path, pathSuffix) {
			return nil
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		switch mode {
		case "corrupt-read":
			if int64(len(body)) <= afterBytes {
				return nil // nothing to corrupt at/after the offset
			}
			body[afterBytes] ^= 0xff
		case "truncate-read":
			if int64(len(body)) <= afterBytes {
				return nil // nothing to truncate
			}
			body = body[:afterBytes]
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeS3ProxyJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("PUT /_control/fault", func(w http.ResponseWriter, r *http.Request) {
		var in s3ProxyFaultRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if in.Mode == "" {
			http.Error(w, `"mode" is required`, http.StatusBadRequest)
			return
		}
		status := in.Status
		if status == 0 {
			status = http.StatusServiceUnavailable
		}
		fault.set(in.Mode, status, in.AfterBytes, in.PathSuffix)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /_control/fault", func(w http.ResponseWriter, _ *http.Request) {
		fault.set("none", 0, 0, "")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /_control/requests", func(w http.ResponseWriter, _ *http.Request) {
		writeS3ProxyJSON(w, http.StatusOK, state.requests())
	})

	// Catch-all: every S3 request (GET/HEAD/PUT/POST/DELETE, any path --
	// bucket-rooted, path-style) not matched by a more specific pattern
	// above.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		state.record(r)

		mode, status, afterBytes, _ := fault.get()
		if mode == "fail-write" && (r.Method == http.MethodPut || r.Method == http.MethodPost) {
			if afterBytes > 0 {
				_, _ = io.CopyN(io.Discard, r.Body, afterBytes)
			}
			_ = r.Body.Close()
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>InternalError</Code><Message>s3proxy: injected fault</Message></Error>`)
			return
		}
		proxy.ServeHTTP(w, r)
	})

	addr := ":8082"
	log.Printf("s3proxy: listening on %s, upstream %s", addr, upstream)
	return http.ListenAndServe(addr, mux)
}

func writeS3ProxyJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "s3proxy: encoding response: %v\n", err)
	}
}
