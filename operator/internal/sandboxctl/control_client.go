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
	body, err := json.Marshal(req)
	if err != nil {
		return ServicesApplyResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.addr+"/v1/services", bytes.NewReader(body))
	if err != nil {
		return ServicesApplyResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	return doJSON[ServicesApplyResponse](c.http, httpReq)
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
