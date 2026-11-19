// Package listenerclient is the load-test harness's client of the Envoy listener: the
// registrations and heartbeats of its sessions, a unit session's exit publish, a session's
// unsubscribe, and the stack check's read of the session list. Every call the harness makes to the
// listener goes through it, each body a listenerwire type, so listenerwire's clients test reads
// this package whole; it cannot read the harness's main package, which also calls Dispatch, NATS
// and Cloud SQL with bodies of their own, and holds that package to naming no listener route.
package listenerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/sjawhar/envoy/excerpt"
	"github.com/sjawhar/envoy/listenerwire"
)

// Client is one listener: its base URL, the bearer a call sends when it is not empty, and the HTTP
// client whose timeout bounds each call.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// Refusal is an answer outside 2xx: the URL of the route that refused, the status and what the
// listener said. Its Error names all three, so a log line that carries only a session id still
// says which listener and route refused: the listener's own error JSON cut to its first 300 bytes,
// and any other body, which a gateway in the listener's place can write echoing the request back,
// in one line with the bearer the call carried redacted (answerText).
type Refusal struct {
	URL    string
	Status int
	Body   []byte
	// token is the bearer the refused call carried, which Error redacts from a foreign body.
	token string
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("%s: %d %s", r.URL, r.Status, answerText(r.Body, "the listener's error JSON", r.token))
}

// Subscribe sends a session's registration, or its heartbeat, which is the same body again
// (POST /v1/interests/subscribe). The answer is nil when a 2xx answer does not decode: the caller
// then knows nothing of the session's delivery, which is the same as an answer naming none.
func (c *Client) Subscribe(ctx context.Context, registration listenerwire.SubscribeRequest) (*listenerwire.SubscribeResponse, error) {
	body, err := c.post(ctx, "/v1/interests/subscribe", registration)
	if err != nil {
		return nil, err
	}
	var answer listenerwire.SubscribeResponse
	if json.Unmarshal(body, &answer) != nil {
		return nil, nil
	}
	return &answer, nil
}

// Publish publishes a message on a topic (POST /v1/messages/publish), reading nothing of the
// answer but its status.
func (c *Client) Publish(ctx context.Context, message listenerwire.PublishRequest) error {
	_, err := c.post(ctx, "/v1/messages/publish", message)
	return err
}

// Unsubscribe hands a session's topics back (POST /v1/interests/unsubscribe) and answers the topics
// the listener says it removed.
func (c *Client) Unsubscribe(ctx context.Context, release listenerwire.UnsubscribeRequest) ([]string, error) {
	body, err := c.post(ctx, "/v1/interests/unsubscribe", release)
	if err != nil {
		return nil, err
	}
	var answer listenerwire.UnsubscribeResponse
	if json.Unmarshal(body, &answer) != nil {
		return nil, fmt.Errorf("unreadable answer: %s", excerpt.BodyForText(string(body), "the listener's unsubscribe answer", c.Token))
	}
	return answer.Removed, nil
}

// SessionsRoute is the listener's session-list route, which Sessions reads and the stack check's
// refusals name.
const SessionsRoute = "/v1/sessions"

// Sessions reads GET /v1/sessions and answers its status and up to 1 MiB of its body, decoding
// nothing: the stack check asks only whether the listener answers its token and refuses another.
func (c *Client) Sessions(ctx context.Context) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+SessionsRoute, nil)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Accept", "application/json")
	c.authorize(request)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, body, err
}

// post sends body as JSON to path and answers what the listener said; an answer outside 2xx is a
// Refusal. Every caller hands it a listenerwire body.
func (c *Client) post(ctx context.Context, path string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	c.authorize(request)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode/100 != 2 {
		return answer, &Refusal{URL: c.URL + path, Status: response.StatusCode, Body: answer, token: c.Token}
	}
	return answer, nil
}

// authorize sets the request's bearer when the client holds one, and no Authorization header when
// it holds none: every call of the client goes through it, so each route sends the same.
func (c *Client) authorize(request *http.Request) {
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

// answerText is body, the listener's answer to a call that carried token, as a message names it:
// the listener's own error JSON (listenerwire.APIError, an object whose error is a string,
// writeJSONError) cut to its first 300 bytes, marked as cut, as the harness's log lines cut an
// answer. Any other body was written by something in the listener's place, such as a gateway,
// which can echo the request back, so it is named as not being notWhat in one line with token and
// any Authorization or Bearer value redacted (excerpt.BodyForText).
func answerText(body []byte, notWhat, token string) string {
	var own listenerwire.APIErrorAs[*string]
	if json.Unmarshal(body, &own) != nil || own.Error == nil {
		return excerpt.BodyForText(string(body), notWhat, token)
	}
	if len(body) > 300 {
		return string(body[:300]) + "…"
	}
	return string(body)
}
