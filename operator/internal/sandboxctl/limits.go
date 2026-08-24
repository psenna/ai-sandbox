package sandboxctl

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenBucket is a minimal, dependency-free rate limiter with the same
// Allow() bool surface golang.org/x/time/rate.Limiter offers. Hand-rolled
// deliberately: `go mod tidy` in this sandbox cannot promote x/time from
// // indirect to a direct require without pulling in a CVE-denied
// golang.org/x/mod version through an unrelated test-only transitive path
// (see the verification notes in the PR/issue), so per the issue's own
// documented fallback, rate limiting here uses no new dependency at all.
type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	max      float64
	perSec   float64
	lastFill time.Time
	now      func() time.Time
}

// newTokenBucket builds a bucket that refills at ratePerSec tokens/second up
// to burst tokens, starting full.
func newTokenBucket(ratePerSec float64, burst int) *tokenBucket {
	return &tokenBucket{
		tokens:   float64(burst),
		max:      float64(burst),
		perSec:   ratePerSec,
		lastFill: time.Now(),
		now:      time.Now,
	}
}

// Allow reports whether a single token is available, consuming it if so.
func (b *tokenBucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	elapsed := now.Sub(b.lastFill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.perSec
		if b.tokens > b.max {
			b.tokens = b.max
		}
		b.lastFill = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Rate limits, one global bucket per route group -- see the issue's
// design table (a single loopback client, no per-IP keying needed).
const (
	waitDoneRatePerSec = 1.0
	waitDoneBurst      = 5
	progressRatePerSec = 0.5
	progressBurst      = 10
	statusRatePerSec   = 5.0
	statusBurst        = 20
	servicesRatePerSec = 0.5 // apply is rare (on edits); allow a burst then throttle
	servicesBurst      = 3
	execRatePerSec     = 2.0 // exec is interactive-frequency; keep it modest
	execBurst          = 5
)

// payload size limits (bytes).
const (
	maxWaitDoneBodyBytes = 16 << 10
	maxProgressBodyBytes = 4 << 10
	maxServicesBodyBytes = 256 << 10 // 256KiB: a declaration with many services/runtimes
	maxExecBodyBytes     = 1 << 20   // 1MiB: command + (text) stdin
)

// recoverer converts a panic in a handler into a 500, so a bug in one
// request can never take down the whole control channel (a genuinely
// long-lived process for the pod's entire lifetime).
func recoverer(log func(format string, args ...any)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log("panic recovered in handler %s %s: %v", r.Method, r.URL.Path, rec)
					writeError(w, http.StatusInternalServerError, CodeInternal, "internal error", "", nil)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// requestLog logs one structured line per request on completion.
func requestLog(log func(format string, args ...any)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			log("request method=%s path=%s status=%d duration=%s", r.Method, r.URL.Path, sw.status, time.Since(start))
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// jsonContentType rejects a POST whose Content-Type is not application/json
// (ignoring parameters) with 415.
func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			ct := r.Header.Get("Content-Type")
			mediaType := strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])
			if mediaType != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType, "Content-Type must be application/json", "", nil)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// limitBody wraps the request body in http.MaxBytesReader so an oversized
// payload fails fast with an actionable 413, naming the limit.
func limitBody(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// rateLimit applies bucket, returning 429 with Retry-After when exhausted.
func rateLimit(bucket *tokenBucket) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !bucket.Allow() {
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusTooManyRequests, CodeRateLimited, "rate limit exceeded, retry shortly", "", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// chain composes middleware outermost-first: chain(h, a, b, c) runs
// a(b(c(h))).
func chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// decodeStrict decodes exactly one JSON value from body into dst, rejecting
// unknown fields and a second trailing JSON value. body should be r.Body,
// already wrapped by limitBody's http.MaxBytesReader -- a *http.MaxBytesError
// surfaces through the returned error via isMaxBytesError.
func decodeStrict(body io.Reader, dst any) error {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if _, ok := isMaxBytesError(err); ok {
			return err
		}
		if strings.Contains(err.Error(), "unknown field") {
			return &decodeError{code: CodeUnknownField, message: err.Error()}
		}
		return &decodeError{code: CodeBadJSON, message: err.Error()}
	}
	if dec.More() {
		return &decodeError{code: CodeBadJSON, message: "body must contain exactly one JSON value"}
	}
	return nil
}

type decodeError struct {
	code    string
	message string
}

func (e *decodeError) Error() string { return e.message }

// isMaxBytesError reports whether err (or something it wraps) is the
// stdlib's *http.MaxBytesError, so the caller can return 413 naming the
// limit rather than a generic 400.
func isMaxBytesError(err error) (*http.MaxBytesError, bool) {
	var mbErr *http.MaxBytesError
	ok := errors.As(err, &mbErr)
	return mbErr, ok
}
