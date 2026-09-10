package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/distribution/reference"
)

// The sentinel errors ListTags wraps. Callers branch with errors.Is:
//
//   - ErrOffline: the registry could not be reached at all, or answered in a
//     way that says "try again later" -- a transport error, a timeout, a
//     refused connection, HTTP 429, or any 5xx. The last known tag list should
//     be kept.
//   - ErrAuth: the registry was reached but refused the request even after the
//     anonymous token dance -- HTTP 401/403 on the retry, or a token endpoint
//     that would not issue one. A configuration problem, not a transient one.
//   - ErrRepoNotFound: the registry was reached and authenticated but has no
//     such repository -- HTTP 404. Almost always a mistyped image reference.
var (
	ErrOffline      = errors.New("registry is unreachable")
	ErrAuth         = errors.New("registry authentication failed")
	ErrRepoNotFound = errors.New("registry repository not found")
)

// Client is the tag-discovery surface the operator depends on. *HTTPClient is
// the real implementation; registrytest.Fake is the test double.
type Client interface {
	// ListTags returns every tag of the configured repository, exactly as the
	// registry reported them: not sorted, not filtered, not de-duplicated.
	// The error, when non-nil, wraps one of the sentinels above or is
	// ctx.Err().
	ListTags(ctx context.Context) ([]string, error)
}

// Options configures New.
type Options struct {
	// Image is the agent image reference (e.g.
	// "ghcr.io/psenna/ai-sandbox-agent:latest"). Its registry domain and
	// repository path are parsed out with github.com/distribution/reference;
	// any tag or digest on it is ignored.
	Image string
	// BaseURL overrides the registry root derived from Image's domain. Empty
	// means "https://<domain>", with docker.io special-cased to
	// "https://registry-1.docker.io". A trailing slash is trimmed.
	BaseURL string
	// AuthToken, when set, is sent as "Authorization: Bearer <token>" on the
	// tag-list request up front (skipping the anonymous challenge) and is
	// also forwarded to the token endpoint if a challenge happens anyway.
	AuthToken string
	// UserAgent is the User-Agent header on every request. Empty sends none.
	UserAgent string
	// HTTP is the client to use. Nil means a fresh
	// &http.Client{Timeout: 30 * time.Second}.
	HTTP *http.Client
}

// HTTPClient talks to a real Distribution v2 API.
type HTTPClient struct {
	repo      string // repository path, e.g. "psenna/ai-sandbox-agent"
	base      string // registry root, no trailing slash
	authToken string
	userAgent string
	http      *http.Client
}

var _ Client = (*HTTPClient)(nil)

// maxPages caps Link-header pagination so a misbehaving registry that keeps
// pointing "next" at itself cannot spin forever. GHCR returns the whole tag
// list of the agent image in one page today; 100 is pure headroom. Reaching
// the cap is an ErrOffline error, not a short list -- see ListTags.
const maxPages = 100

// New builds an HTTPClient from opts, parsing opts.Image for the registry
// domain and repository path.
func New(opts Options) (*HTTPClient, error) {
	named, err := reference.ParseNormalizedNamed(opts.Image)
	if err != nil {
		return nil, fmt.Errorf("parsing the agent image reference %q: %w", opts.Image, err)
	}
	domain := reference.Domain(named)
	repo := reference.Path(named)

	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		if domain == "docker.io" {
			base = "https://registry-1.docker.io"
		} else {
			base = "https://" + domain
		}
	}

	hc := opts.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}

	return &HTTPClient{
		repo:      repo,
		base:      base,
		authToken: opts.AuthToken,
		userAgent: opts.UserAgent,
		http:      hc,
	}, nil
}

// tagListPayload is the body of a v2 tags/list response.
type tagListPayload struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// ListTags implements Client.
func (c *HTTPClient) ListTags(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	next := c.base + "/v2/" + c.repo + "/tags/list"
	token := c.authToken

	var out []string
	for page := 0; page < maxPages; page++ {
		names, link, err := c.getPage(ctx, next, &token)
		if err != nil {
			return nil, err
		}
		out = append(out, names...)
		if link == "" {
			return out, nil
		}
		next = link
	}
	// The cap was reached with a "next" link still pending, so what we have is
	// a truncated prefix of the tag list. Returning it as if it were complete
	// would let the caller persist it wholesale and prune tags that do exist,
	// so this is an error -- and an ErrOffline one, because "the registry is
	// misbehaving, keep the last-known list" is exactly the right response.
	return nil, fmt.Errorf("the registry kept paginating the tag list past %d pages: %w", maxPages, ErrOffline)
}

// getPage fetches one tags/list page, running the anonymous token dance on a
// 401 (updating *token in place so later pages reuse it), and returns the
// page's tags plus the resolved "next" URL ("" when there is none).
func (c *HTTPClient) getPage(ctx context.Context, rawURL string, token *string) (names []string, nextURL string, err error) {
	resp, err := c.authedGet(ctx, rawURL, token)
	if err != nil {
		return nil, "", err
	}
	defer drainClose(resp.Body)

	if err := classifyStatus(resp); err != nil {
		return nil, "", err
	}

	var payload tagListPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, "", fmt.Errorf("decoding the tag list from %s: %w", rawURL, err)
	}
	return payload.Tags, c.nextLink(resp), nil
}

// authedGet issues GET rawURL with the current token; if the registry answers
// 401 with a parseable Bearer challenge, it fetches a token, stores it in
// *token, and retries the request once.
func (c *HTTPClient) authedGet(ctx context.Context, rawURL string, token *string) (*http.Response, error) {
	resp, err := c.rawGet(ctx, rawURL, *token)
	if err != nil {
		return nil, c.transportErr(ctx, rawURL, err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	challenge := parseBearerChallenge(resp.Header.Get("WWW-Authenticate"))
	drainClose(resp.Body)
	if challenge.realm == "" {
		return nil, fmt.Errorf("the registry demanded authentication for %s but sent no Bearer challenge: %w", rawURL, ErrAuth)
	}

	tok, err := c.fetchToken(ctx, challenge)
	if err != nil {
		return nil, err
	}
	*token = tok

	resp, err = c.rawGet(ctx, rawURL, *token)
	if err != nil {
		return nil, c.transportErr(ctx, rawURL, err)
	}
	return resp, nil
}

// rawGet performs one GET with no retry logic.
func (c *HTTPClient) rawGet(ctx context.Context, rawURL, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.http.Do(req)
}

// bearerChallenge is the parsed content of a
// `WWW-Authenticate: Bearer realm="...",service="...",scope="..."` header.
type bearerChallenge struct{ realm, service, scope string }

// parseBearerChallenge extracts realm/service/scope from a WWW-Authenticate
// header value. A header that is not a Bearer challenge yields the zero value
// (realm == "").
func parseBearerChallenge(header string) bearerChallenge {
	h := strings.TrimSpace(header)
	if len(h) < 7 || !strings.EqualFold(h[:7], "Bearer ") {
		return bearerChallenge{}
	}
	var ch bearerChallenge
	for _, part := range splitParams(h[7:]) {
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "realm":
			ch.realm = val
		case "service":
			ch.service = val
		case "scope":
			ch.scope = val
		}
	}
	return ch
}

// splitParams splits a challenge parameter list on commas that are not inside
// double quotes.
func splitParams(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

// tokenResponse is the body of a token-endpoint response. Distribution's spec
// uses "token"; the OAuth2 flavour uses "access_token". "token" wins.
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
}

// fetchToken calls the challenge's realm with service+scope and returns the
// issued token.
func (c *HTTPClient) fetchToken(ctx context.Context, ch bearerChallenge) (string, error) {
	u, err := url.Parse(ch.realm)
	if err != nil {
		return "", fmt.Errorf("parsing the registry auth realm %q: %w", ch.realm, err)
	}
	q := u.Query()
	if ch.service != "" {
		q.Set("service", ch.service)
	}
	if ch.scope != "" {
		q.Set("scope", ch.scope)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", c.transportErr(ctx, u.String(), err)
	}
	defer drainClose(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("the registry auth endpoint refused to issue a token (HTTP %d): %w", resp.StatusCode, ErrAuth)
	default:
		return "", fmt.Errorf("the registry auth endpoint returned HTTP %d: %w", resp.StatusCode, ErrOffline)
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decoding the registry auth token response: %w", err)
	}
	tok := tr.Token
	if tok == "" {
		tok = tr.AccessToken
	}
	if tok == "" {
		return "", fmt.Errorf("the registry auth endpoint issued an empty token: %w", ErrAuth)
	}
	return tok, nil
}

// origin returns the scheme+host this client is configured to talk to. The
// bool is false when c.base is not a usable absolute URL.
func (c *HTTPClient) origin() (*url.URL, bool) {
	u, err := url.Parse(c.base)
	if err != nil || u.Host == "" {
		return nil, false
	}
	return u, true
}

// nextLink returns the absolute URL of the `rel="next"` Link header, resolved
// (per RFC 3986) against the request that produced resp, or "" when there is
// none.
//
// A next link that resolves to a different scheme or host than the configured
// registry is IGNORED rather than followed: the value comes straight out of a
// response header, and authedGet would attach the Authorization header -- the
// operator's configured registry token, or the anonymous token just minted
// for this one repository -- to whatever host it named. Pagination is not a
// reason to hand a credential to an unconfigured host, or to turn the
// operator into a request forwarder for one.
func (c *HTTPClient) nextLink(resp *http.Response) string {
	origin, ok := c.origin()
	if !ok {
		return ""
	}
	base := origin
	if resp.Request != nil && resp.Request.URL != nil {
		base = resp.Request.URL
	}
	for _, header := range resp.Header.Values("Link") {
		for _, part := range strings.Split(header, ",") {
			part = strings.TrimSpace(part)
			lt := strings.IndexByte(part, '<')
			gt := strings.IndexByte(part, '>')
			if lt != 0 || gt <= lt {
				continue
			}
			if !linkRelIsNext(part[gt+1:]) {
				continue
			}
			ref, err := url.Parse(part[1:gt])
			if err != nil {
				continue
			}
			next := base.ResolveReference(ref)
			if !strings.EqualFold(next.Scheme, origin.Scheme) || !strings.EqualFold(next.Host, origin.Host) {
				continue
			}
			return next.String()
		}
	}
	return ""
}

// linkRelIsNext reports whether the parameter tail of one Link value carries
// rel="next" (or rel=next).
func linkRelIsNext(params string) bool {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || strings.ToLower(strings.TrimSpace(k)) != "rel" {
			continue
		}
		if strings.EqualFold(strings.Trim(strings.TrimSpace(v), `"`), "next") {
			return true
		}
	}
	return false
}

// classifyStatus turns a non-2xx tags/list response into a wrapped sentinel.
// It reads a short body snippet for the generic case only, and never touches
// the request's Authorization header.
func classifyStatus(resp *http.Response) error {
	sc := resp.StatusCode
	switch {
	case sc >= 200 && sc < 300:
		return nil
	case sc == http.StatusNotFound:
		return fmt.Errorf("the registry has no repository for this image (HTTP 404): %w", ErrRepoNotFound)
	case sc == http.StatusUnauthorized || sc == http.StatusForbidden:
		return fmt.Errorf("the registry refused the tag-list request (HTTP %d): %w", sc, ErrAuth)
	case sc == http.StatusTooManyRequests || sc >= 500:
		return fmt.Errorf("the registry is unavailable (HTTP %d): %w", sc, ErrOffline)
	default:
		return fmt.Errorf("the registry returned HTTP %d: %s", sc, bodySnippet(resp.Body))
	}
}

// transportErr maps a *http.Client.Do error to ctx.Err() when the context is
// done, or a wrapped ErrOffline otherwise.
func (c *HTTPClient) transportErr(ctx context.Context, rawURL string, err error) error {
	if ce := ctx.Err(); ce != nil {
		return ce
	}
	return fmt.Errorf("contacting the registry at %s: %w (%v)", rawURL, ErrOffline, err)
}

// bodySnippet reads up to 200 bytes of a response body for an error message.
func bodySnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 200))
	return strings.TrimSpace(string(b))
}

// drainClose drains a bounded amount of a response body and closes it, so the
// underlying connection can be reused.
func drainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<16))
	_ = rc.Close()
}
