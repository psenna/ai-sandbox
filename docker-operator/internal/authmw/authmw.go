// Package authmw is the operator's HTTP authentication middleware: one static
// Bearer token, enforced on the REST API and the terminal-WebSocket routes.
//
// It is deliberately tiny and imports nothing from the rest of the operator so
// that cmd/docker-operator (which wires it around both internal/api and
// internal/wsbridge handlers) pulls in no dependency cycle.
package authmw

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// unauthorizedBody is the 401 payload. Its shape matches internal/api's
// ErrorEnvelope -- {"error":{"code","message"}} -- so a client switches on the
// same structure for an auth failure as for every other API error.
const unauthorizedBody = `{"error":{"code":"unauthorized","message":"a valid operator API token is required (Authorization: Bearer <token>)"}}`

// RequireBearer wraps next so every request must present token as
// "Authorization: Bearer <token>". A WebSocket upgrade handshake may instead
// pass it as "?token=<token>" in the query string, because a browser cannot
// set a header on the handshake.
//
// An empty token disables the check entirely: RequireBearer returns next
// unchanged, so an operator running unauthenticated pays nothing for the
// wrapper (it logs a startup warning in that case -- see cmd/docker-operator).
func RequireBearer(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if presented, ok := presentedToken(r); ok {
			got := sha256.Sum256([]byte(presented))
			// Hash-then-compare so the compare is constant-time regardless of
			// how the presented token's length relates to the real one.
			if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(unauthorizedBody))
	})
}

// presentedToken pulls the caller's token from the Authorization header, or --
// only for a WebSocket upgrade handshake -- from the "token" query parameter.
// Gating the query fallback on the upgrade keeps an ordinary API call from
// smuggling a credential through the URL, where proxies and access logs would
// capture it.
func presentedToken(r *http.Request) (string, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		rest, ok := strings.CutPrefix(h, "Bearer ")
		if !ok {
			return "", false
		}
		rest = strings.TrimSpace(rest)
		return rest, rest != ""
	}
	if isWebSocketUpgrade(r) {
		t := r.URL.Query().Get("token")
		return t, t != ""
	}
	return "", false
}

// isWebSocketUpgrade reports whether r is a WebSocket upgrade handshake
// (Connection: Upgrade + Upgrade: websocket, both matched case-insensitively).
func isWebSocketUpgrade(r *http.Request) bool {
	return headerListHas(r.Header.Get("Connection"), "upgrade") &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

// headerListHas reports whether the comma-separated header value contains want
// (case-insensitively), e.g. Connection: "keep-alive, Upgrade".
func headerListHas(headerValue, want string) bool {
	for _, part := range strings.Split(headerValue, ",") {
		if strings.EqualFold(strings.TrimSpace(part), want) {
			return true
		}
	}
	return false
}
