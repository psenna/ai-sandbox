package sandboxctl

import (
	"net/http"
	"time"
)

// maxHeaderBytes bounds request header size (gosec G114 also flags a
// *http.Server with any zero timeout left unset; every field below is set
// explicitly for that reason too).
const maxHeaderBytes = 8 << 10

// NewServer builds the control API's *http.Server. It does not start
// listening -- call Serve/ListenAndServe or run.go's own accept loop.
func NewServer(cfg Config, store Store, poll *Poller, env EnvironmentRef, sets serviceSetApplier, execer Execer, now func() time.Time, log func(format string, args ...any)) *http.Server {
	if log == nil {
		log = func(string, ...any) {}
	}
	h := &handlers{store: store, poll: poll, env: env, sets: sets, execer: execer, now: now, log: log}

	waitDoneBucket := newTokenBucket(waitDoneRatePerSec, waitDoneBurst)
	progressBucket := newTokenBucket(progressRatePerSec, progressBurst)
	statusBucket := newTokenBucket(statusRatePerSec, statusBurst)
	servicesBucket := newTokenBucket(servicesRatePerSec, servicesBurst)
	execBucket := newTokenBucket(execRatePerSec, execBurst)

	mux := http.NewServeMux()

	mux.Handle("POST /v1/wait", postChain(http.HandlerFunc(h.handleWait), log, waitDoneBucket, maxWaitDoneBodyBytes))
	mux.Handle("POST /v1/done", postChain(http.HandlerFunc(h.handleDone), log, waitDoneBucket, maxWaitDoneBodyBytes))
	mux.Handle("POST /v1/progress", postChain(http.HandlerFunc(h.handleProgress), log, progressBucket, maxProgressBodyBytes))
	mux.Handle("POST /v1/services", postChain(http.HandlerFunc(h.handleServicesApply), log, servicesBucket, maxServicesBodyBytes))
	mux.Handle("POST /v1/exec", postChain(http.HandlerFunc(h.handleExec), log, execBucket, maxExecBodyBytes))
	mux.Handle("GET /v1/status", chain(http.HandlerFunc(h.handleStatus), recoverer(log), requestLog(log), rateLimit(statusBucket)))
	// /healthz is exempt from rate limiting (the kubelet probes it every
	// second) and keeps answering even once freezing is latched.
	mux.Handle("GET /healthz", chain(http.HandlerFunc(h.handleHealthz), recoverer(log), requestLog(log)))

	// Bare, method-agnostic registrations for the same paths: net/http's
	// ServeMux (1.22+) prefers a method-specific pattern for a matching
	// method, but falls back to these for any OTHER method on the same
	// path -- giving a real 405, not a silent 404 fall-through to the
	// catch-all below.
	methodNotAllowed := func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method "+r.Method+" is not allowed on "+r.URL.Path, "", nil)
	}
	mux.HandleFunc("/v1/wait", methodNotAllowed)
	mux.HandleFunc("/v1/done", methodNotAllowed)
	mux.HandleFunc("/v1/progress", methodNotAllowed)
	mux.HandleFunc("/v1/services", methodNotAllowed)
	mux.HandleFunc("/v1/exec", methodNotAllowed)
	mux.HandleFunc("/v1/status", methodNotAllowed)
	mux.HandleFunc("/healthz", methodNotAllowed)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such endpoint: "+r.Method+" "+r.URL.Path, "", nil)
	})

	return &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}

// postChain is the middleware chain shared by every mutating (POST)
// endpoint: recoverer -> requestLog -> jsonContentType -> limitBody ->
// rateLimit -> handler (outermost first).
func postChain(h http.Handler, log func(format string, args ...any), bucket *tokenBucket, maxBytes int64) http.Handler {
	return chain(h, recoverer(log), requestLog(log), jsonContentType, limitBody(maxBytes), rateLimit(bucket))
}
