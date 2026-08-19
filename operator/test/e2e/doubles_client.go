package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	gomega "github.com/onsi/gomega"
)

// The types below are duplicated (not shared) with test/e2e/doubles'
// types.go on purpose: broker and model are independent HTTP contracts
// between two separate Go modules, and JSON on the wire -- not a shared Go
// type -- is the actual contract.

// CheckRun is a single named check within a CheckSummary.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// WorkflowRun is a single named workflow run within a CheckSummary.
type WorkflowRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url"`
}

// CheckSummary is the fake broker's canned CI status for a ref.
type CheckSummary struct {
	Overall   string        `json:"overall"`
	Checks    []CheckRun    `json:"checks"`
	Workflows []WorkflowRun `json:"workflows"`
}

// PRState is the fake broker's canned pull-request state.
type PRState struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Mergeable *bool  `json:"mergeable"`
	Head      string `json:"head"`
	Base      string `json:"base"`
	URL       string `json:"url"`
}

// IssueState is the fake broker's canned issue state.
type IssueState struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	State  string   `json:"state"`
	Body   string   `json:"body"`
	URL    string   `json:"url"`
	Labels []string `json:"labels"`
}

// RecordedRequest is one entry in a double's request log
// (GET /_control/requests).
type RecordedRequest struct {
	Method string    `json:"method"`
	Path   string    `json:"path"`
	Time   time.Time `json:"time"`
}

func (h *Harness) brokerBaseURL() string {
	return fmt.Sprintf("http://%s:%d", h.Cfg.Host, h.Cfg.BrokerPort)
}

func (h *Harness) modelBaseURL() string {
	return fmt.Sprintf("http://%s:%d", h.Cfg.Host, h.Cfg.ModelPort)
}

// S3ProxyBaseURL is the s3proxy double's own control API, reachable from
// the TEST process (outside the cluster) via the NodePort. Not to be
// confused with S3ProxyInClusterEndpoint, which is the S3-shaped address a
// SandboxClass running INSIDE the cluster is pointed at.
func (h *Harness) S3ProxyBaseURL() string {
	return fmt.Sprintf("http://%s:%d", h.Cfg.Host, h.Cfg.S3ProxyPort)
}

// S3ProxyInClusterEndpoint is the address a SandboxClass's
// storage.backend.s3.endpoint must use to route S3 traffic through the
// fault-injecting proxy instead of talking to MinIO directly -- see
// WithS3Endpoint.
func (h *Harness) S3ProxyInClusterEndpoint() string {
	return fmt.Sprintf("http://platform-doubles.%s.svc.cluster.local:8082", h.Cfg.ServicesNamespace)
}

type controlPutBody struct {
	Path   string `json:"path"`
	Status int    `json:"status"`
	Body   any    `json:"body"`
}

func (h *Harness) putControlState(ctx context.Context, path string, body any) {
	h.doJSON(ctx, http.MethodPut, h.brokerBaseURL()+"/_control/state", controlPutBody{
		Path:   path,
		Status: http.StatusOK,
		Body:   body,
	}, nil)
}

// BrokerSetCheckSummary programs the fake broker's GET
// /{repo}/checks/{ref} response.
func (h *Harness) BrokerSetCheckSummary(ctx context.Context, repo, ref, overall string, checks ...CheckRun) {
	h.putControlState(ctx, fmt.Sprintf("/%s/checks/%s", repo, ref), CheckSummary{
		Overall: overall,
		Checks:  checks,
	})
}

// BrokerSetPR programs the fake broker's GET /{repo}/prs/{number} response.
func (h *Harness) BrokerSetPR(ctx context.Context, repo string, number int, state string, mergeable *bool) {
	h.putControlState(ctx, fmt.Sprintf("/%s/prs/%d", repo, number), PRState{
		Number:    number,
		State:     state,
		Mergeable: mergeable,
	})
}

// BrokerSetIssue programs the fake broker's GET /{repo}/issues/{number}
// response.
func (h *Harness) BrokerSetIssue(ctx context.Context, repo string, number int, state string, labels ...string) {
	h.putControlState(ctx, fmt.Sprintf("/%s/issues/%d", repo, number), IssueState{
		Number: number,
		State:  state,
		Labels: labels,
	})
}

// BrokerReset clears every canned response programmed on the fake broker.
func (h *Harness) BrokerReset(ctx context.Context) {
	h.doJSON(ctx, http.MethodDelete, h.brokerBaseURL()+"/_control/state", nil, nil)
}

// BrokerRequests returns the fake broker's request log.
func (h *Harness) BrokerRequests(ctx context.Context) []RecordedRequest {
	var out []RecordedRequest
	h.doJSON(ctx, http.MethodGet, h.brokerBaseURL()+"/_control/requests", nil, &out)
	return out
}

// ModelSetReply sets the stub model endpoint's canned reply text.
func (h *Harness) ModelSetReply(ctx context.Context, text string) {
	h.doJSON(ctx, http.MethodPut, h.modelBaseURL()+"/_control/reply", map[string]string{"text": text}, nil)
}

// ModelRequests returns the stub model endpoint's request log.
func (h *Harness) ModelRequests(ctx context.Context) []RecordedRequest {
	var out []RecordedRequest
	h.doJSON(ctx, http.MethodGet, h.modelBaseURL()+"/_control/requests", nil, &out)
	return out
}

type s3ProxyFaultBody struct {
	Mode       string `json:"mode"`
	Status     int    `json:"status"`
	AfterBytes int64  `json:"afterBytes"`
	PathSuffix string `json:"pathSuffix"`
}

// S3ProxySetFault programs the s3proxy double to fail every PUT/POST once
// afterBytes of the request body have been drained, answering with status
// (an S3-shaped XML error body) instead of forwarding to MinIO. GET/HEAD/
// DELETE are never affected.
func (h *Harness) S3ProxySetFault(ctx context.Context, mode string, status int, afterBytes int64) {
	h.S3ProxySetFaultOn(ctx, mode, status, afterBytes, "")
}

// S3ProxySetFaultOn is S3ProxySetFault with a pathSuffix filter: when
// pathSuffix is non-empty, read faults (corrupt-read/truncate-read) apply
// only to GETs whose path ends in it, so a spec can corrupt
// workspace.tar.zst while leaving manifest.json intact.
func (h *Harness) S3ProxySetFaultOn(ctx context.Context, mode string, status int, afterBytes int64, pathSuffix string) {
	h.doJSON(ctx, http.MethodPut, h.S3ProxyBaseURL()+"/_control/fault", s3ProxyFaultBody{
		Mode: mode, Status: status, AfterBytes: afterBytes, PathSuffix: pathSuffix,
	}, nil)
}

// S3ProxyClearFault restores normal (pass-through) proxying.
func (h *Harness) S3ProxyClearFault(ctx context.Context) {
	h.doJSON(ctx, http.MethodDelete, h.S3ProxyBaseURL()+"/_control/fault", nil, nil)
}

// S3ProxyRequests returns the s3proxy double's request log.
func (h *Harness) S3ProxyRequests(ctx context.Context) []RecordedRequest {
	var out []RecordedRequest
	h.doJSON(ctx, http.MethodGet, h.S3ProxyBaseURL()+"/_control/requests", nil, &out)
	return out
}

// doJSON is a thin JSON HTTP client helper for the doubles' unauthenticated
// /_control/* routes. Fails the current spec on any transport or status
// error.
func (h *Harness) doJSON(ctx context.Context, method, url string, in, out any) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		gomega.ExpectWithOffset(2, err).NotTo(gomega.HaveOccurred())
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	gomega.ExpectWithOffset(2, err).NotTo(gomega.HaveOccurred())
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	gomega.ExpectWithOffset(2, err).NotTo(gomega.HaveOccurred(), "%s %s", method, url)
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	gomega.ExpectWithOffset(2, err).NotTo(gomega.HaveOccurred())
	gomega.ExpectWithOffset(2, resp.StatusCode).To(gomega.BeNumerically("<", 300), "%s %s: %s", method, url, respBody)
	if out != nil && len(respBody) > 0 {
		gomega.ExpectWithOffset(2, json.Unmarshal(respBody, out)).To(gomega.Succeed())
	}
}
