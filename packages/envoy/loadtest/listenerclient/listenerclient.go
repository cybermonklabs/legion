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
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/urljoin"
	"github.com/sjawhar/envoy/listenerwire"
)

// Client is one listener: its base URL, the bearer a call sends when it is not empty, and the HTTP
// client whose timeout bounds each call, which follows no redirect (redirects.Refused), since
// every call carries the bearer.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// Refusal is an answer outside 2xx: the URL of the route that refused, as redact.Call prints it
// beside the listener's URL (whole for a clean URL, NotPrinted for one with user information or a
// query, which can carry a credential), the status and what the listener said, as the rule prints
// it with every credential the request carried redacted (redact.Answer.Words). Its Error names all
// three, the words cut to their first 300 bytes, so a log line that carries only a session id
// still says which listener and route refused.
type Refusal struct {
	URL    string
	Status int
	Said   string
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("%s: %d %s", r.URL, r.Status, truncate(r.Said))
}

// answerLimit bounds one answer of the listener this client reads.
const answerLimit = 1 << 20

// Subscribe sends a session's registration, or its heartbeat, which is the same body again
// (POST /v1/interests/subscribe). The answer is nil when a 2xx answer does not decode: the caller
// then knows nothing of the session's delivery, which is the same as an answer naming none.
func (c *Client) Subscribe(ctx context.Context, registration listenerwire.SubscribeRequest) (*listenerwire.SubscribeResponse, error) {
	body, err := c.post(ctx, "/v1/interests/subscribe", registration)
	if err != nil {
		return nil, err
	}
	var answer listenerwire.SubscribeResponse
	if body.Decode(&answer) != nil {
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
	if err := body.Decode(&answer); err != nil {
		return nil, fmt.Errorf("unreadable answer: %w", err)
	}
	return answer.Removed, nil
}

// SessionsRoute is the listener's session-list route, which Sessions reads and the stack check's
// refusals name.
const SessionsRoute = "/v1/sessions"

// Sessions reads GET /v1/sessions and answers its status and up to 1 MiB of its answer, decoding
// nothing: the stack check asks only whether the listener answers its token and refuses another.
func (c *Client) Sessions(ctx context.Context) (int, redact.Answer, error) {
	request, err := c.newRequest(ctx, http.MethodGet, SessionsRoute, nil)
	if err != nil {
		return 0, redact.Answer{}, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return 0, redact.Answer{}, redact.Cause(err, c.URL)
	}
	defer response.Body.Close()
	body, err := redact.ReadAnswer(response, answerLimit)
	if errors.Is(err, redact.ErrAnswerTooLarge) {
		err = nil
	}
	return response.StatusCode, body, err
}

// post sends body as JSON to path and answers what the listener said, read through
// redact.ReadAnswer; an answer outside 2xx is a Refusal. Every caller hands it a listenerwire body.
func (c *Client) post(ctx context.Context, path string, body any) (redact.Answer, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return redact.Answer{}, err
	}
	request, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(data))
	if err != nil {
		return redact.Answer{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return redact.Answer{}, redact.Cause(err, c.URL)
	}
	defer response.Body.Close()
	answer, _ := redact.ReadAnswer(response, answerLimit)
	if response.StatusCode/100 != 2 {
		return answer, &Refusal{URL: redact.Call(c.URL, request.URL.String()), Status: response.StatusCode, Said: answer.Words().String()}
	}
	return answer, nil
}

// newRequest is a request of method with body for route, joined onto the listener's URL
// (urljoin.Join). It carries the client's bearer (authorize). A URL that does not join, or whose
// joined URL does not parse again (url.URL.String can write text url.Parse refuses, such as an
// IPv6 zone escape), is refused in fixed words: url.Parse's error, which http.NewRequest's
// carries, quotes the URL whole, user information and query included.
func (c *Client) newRequest(ctx context.Context, method, route string, body io.Reader) (*http.Request, error) {
	joined, err := urljoin.Join(c.URL, route)
	if err != nil {
		return nil, errors.New("the listener URL is not an absolute URL with a host (it is not printed)")
	}
	request, err := http.NewRequestWithContext(ctx, method, joined.String(), body)
	if err != nil {
		return nil, errors.New("the listener URL joined with the route does not parse (it is not printed)")
	}
	c.authorize(request)
	return request, nil
}

// authorize sets the request's bearer when the client holds one, and no Authorization header when
// it holds none: every call of the client goes through it (newRequest), so each route sends the
// same.
func (c *Client) authorize(request *http.Request) {
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

// truncate is s cut to its first 300 bytes, marked as cut, as the harness's log lines cut an
// answer.
func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
