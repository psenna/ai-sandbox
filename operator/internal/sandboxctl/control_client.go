package sandboxctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ControlClient is the agent's loopback HTTP client to the control API. It
// holds no Kubernetes credential -- only an HTTP connection to 127.0.0.1:9099.
type ControlClient struct {
	addr string
	http *http.Client
}

func NewControlClient(addr string) *ControlClient {
	return &ControlClient{addr: addr, http: &http.Client{}} // timeout via ctx
}

// ApplyServices POSTs a declaration to /v1/services and returns the response.
func (c *ControlClient) ApplyServices(ctx context.Context, req ServicesApplyRequest) (ServicesApplyResponse, error) {
	return doPostJSON[ServicesApplyResponse](ctx, c.http, c.addr, "/v1/services", req)
}

// Exec POSTs an ExecRequest to /v1/exec and returns the response.
func (c *ControlClient) Exec(ctx context.Context, req ExecRequest) (ExecResponse, error) {
	return doPostJSON[ExecResponse](ctx, c.http, c.addr, "/v1/exec", req)
}

// doPostJSON marshals req, POSTs it to addr/path as application/json, and
// decodes either the success body into T or the ErrorEnvelope on a non-2xx. It
// is the shared helper behind every ControlClient POST, eliminating the
// per-method marshal/build/header boilerplate.
func doPostJSON[T any](ctx context.Context, h *http.Client, addr, path string, req any) (T, error) {
	var zero T
	body, err := json.Marshal(req)
	if err != nil {
		return zero, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+path, bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	return doJSON[T](h, httpReq)
}

// doJSON executes req and decodes either the success body into T or the
// ErrorEnvelope on a non-2xx.
func doJSON[T any](h *http.Client, req *http.Request) (T, error) {
	var zero T
	resp, err := h.Do(req)
	if err != nil {
		return zero, fmt.Errorf("control API %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out T
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return zero, fmt.Errorf("decoding control API response: %w", err)
		}
		return out, nil
	}
	var env ErrorEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	msg := env.Error.Message
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return zero, &ControlAPIError{Code: env.Error.Code, Status: resp.StatusCode, Message: msg}
}

// ControlAPIError is returned by ControlClient on a non-2xx response, carrying
// the server's error code so the CLI can print an actionable message.
type ControlAPIError struct {
	Code    string
	Status  int
	Message string
}

func (e *ControlAPIError) Error() string { return e.Message }
