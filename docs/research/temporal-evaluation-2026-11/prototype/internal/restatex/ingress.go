package restatex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Ingress is the Restate server's user-facing HTTP API: the way into a handler from
// outside. The experiments speak it with net/http rather than through the SDK's
// ingress client, so what a fact reports is exactly what crossed the wire.
type Ingress struct {
	base   string
	client *http.Client
}

// NewIngress addresses the ingress at a loopback HOST:PORT.
func NewIngress(address string) *Ingress {
	return &Ingress{
		base:   "http://" + address,
		client: &http.Client{Timeout: 2 * time.Minute},
	}
}

// Send starts an invocation and returns its id without waiting for it to finish. It is
// `POST /{service}/{handler}/send`, the one-way form of the ingress call, which is what
// a driver that then wants to watch the invocation's state needs.
func (i *Ingress) Send(ctx context.Context, service, handler string, input any) (string, error) {
	var out struct {
		InvocationID string `json:"invocationId"`
		Status       string `json:"status"`
	}
	if err := i.do(ctx, fmt.Sprintf("/%s/%s/send", service, handler), input, &out); err != nil {
		return "", err
	}
	if out.InvocationID == "" {
		return "", fmt.Errorf("ingress accepted %s/%s but returned no invocation id", service, handler)
	}
	return out.InvocationID, nil
}

// Call invokes a handler and waits for its result, decoding it into out.
func (i *Ingress) Call(ctx context.Context, service, handler string, input, out any) error {
	return i.do(ctx, fmt.Sprintf("/%s/%s", service, handler), input, out)
}

// ResolveAwakeable completes an awakeable from outside any handler, over plain HTTP.
// This is the call a person's approval would make in Legion: nothing of Restate's SDK
// is involved, only an id that was published and a value.
func (i *Ingress) ResolveAwakeable(ctx context.Context, id string, value any) error {
	return i.do(ctx, fmt.Sprintf("/restate/awakeables/%s/resolve", id), value, nil)
}

// Output is an invocation's result once it has one: `GET /restate/invocation/{id}/output`.
// It returns the raw body and the HTTP status, because a 470 for a result that is not
// ready yet is itself a reading.
func (i *Ingress) Output(ctx context.Context, id string) (status string, body string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/restate/invocation/%s/output", i.base, id), nil)
	if err != nil {
		return "", "", err
	}
	resp, err := i.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	return resp.Status, oneLine(string(payload)), nil
}

// do is one ingress POST with a JSON body and an optional JSON reply.
func (i *Ingress) do(ctx context.Context, path string, input, out any) error {
	encoded, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.base+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := i.client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: %s: %s", path, resp.Status, oneLine(string(payload)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("POST %s: decode %s: %w", path, oneLine(string(payload)), err)
	}
	return nil
}
