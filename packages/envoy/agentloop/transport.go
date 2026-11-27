package agentloop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/sjawhar/envoy/httpoutage"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/serverwait"
	"github.com/sjawhar/envoy/redirects"
)

// modelTransport carries every model call of a run. It puts the endpoint's token, read again for
// the attempt, on each request; adds the endpoint's own query parameters to it; and makes the
// run's one retry, on an outage status (a 408, a 429, a 5xx) or a transport error, within the
// run's time.
type modelTransport struct {
	base  http.RoundTripper
	kind  Kind
	token func(context.Context) (string, error)
	query url.Values
}

// newModelTransport is the transport one Run or one Decide makes its model calls through, over
// the default transport, for the endpoint whose protocol, token and query parameters it carries.
func newModelTransport(endpoint Endpoint) *modelTransport {
	return &modelTransport{base: http.DefaultTransport, kind: endpoint.Kind, token: endpoint.Token, query: endpoint.Query}
}

// client is the HTTP client one Run or one Decide makes its model calls with: this transport,
// and no redirect ever followed (redirects.Refused, the module's policy for a client that carries
// a credential). The transport puts the endpoint's credential on every request it carries, which
// would include the one the client makes for a 3xx, and Go strips Authorization on a cross-domain
// redirect before a transport runs, so following one would hand the token to whatever server the
// Location names. The transport refuses a redirect itself, by its status, before the client sees
// it (RoundTrip), so the call fails and the caller is told the endpoint redirected:
// anthropic-sdk-go and openai-go read a 3xx a client hands back as a success, and its empty body
// as an empty stream.
func (t *modelTransport) client() *http.Client {
	return redirects.Refused(&http.Client{Transport: t})
}

var errRedirect = errors.New("agentloop: the endpoint answered a redirect, which is never followed")

type tokenFailure struct {
	kind Kind
	err  error
}

func (f *tokenFailure) Error() string { return fmt.Sprintf("reading the %s token: %v", f.kind, f.err) }
func (f *tokenFailure) Unwrap() error { return f.err }

// defaultRetryWait is how long the retry waits when the response names no wait (retryable).
const defaultRetryWait = time.Second

func (t *modelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
	}
	ctx := req.Context()
	for attempt := 0; ; attempt++ {
		var key string
		// Azure signs its own calls: the provider's credential middleware sets the header,
		// and this transport reads no token for it.
		if t.kind != AzureAnthropic {
			var err error
			if key, err = t.token(ctx); err != nil {
				return nil, &tokenFailure{t.kind, err}
			}
		}
		r := req.Clone(ctx)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		switch t.kind {
		case AnthropicMessages:
			r.Header.Set("X-Api-Key", key)
		case OpenAIResponses, OpenAIChat, SystemOne:
			r.Header.Set("Authorization", "Bearer "+key)
		}
		if len(t.query) > 0 {
			q := r.URL.Query()
			for name, values := range t.query {
				// A parameter the provider itself put there is the provider's.
				if _, ok := q[name]; !ok {
					q[name] = values
				}
			}
			r.URL.RawQuery = q.Encode()
		}
		resp, err := t.base.RoundTrip(r)
		if wait, retry := retryable(resp, err); retry && attempt == 0 && waitable(ctx, wait) {
			if resp != nil {
				drain(resp)
			}
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if err != nil {
			return nil, err
		}
		if redirects.Status(resp.StatusCode) {
			// Refused here, before net/http's client reads the Location: it parses one before it
			// asks its redirect policy, and its error names it, one that does not parse quoted
			// whole and one the policy refused as the *url.Error's URL.
			drain(resp)
			return nil, redact.Masked(errRedirect, fmt.Sprintf("the %s endpoint answered %s, a redirect, which is never followed",
				t.kind, redact.StatusLine(resp.StatusCode)))
		}
		return resp, nil
	}
}

// drain reads resp's body to its end as data no one reads (redact.StreamAnswer) and closes it, so
// the connection goes back to the pool.
func drain(resp *http.Response) {
	drained := redact.StreamAnswer(resp, answerLimit)
	_, _ = io.Copy(io.Discard, drained)
	_ = drained.Close()
}

// retryable is whether a model call's outcome is retried, and after how long: a transport error,
// or an outage (httpoutage.Is, RFC 9110: a 408, a 429 or a 5xx), after the wait the response
// names, read through serverwait.Read, or a second when it names none. A wait of an hour is one a
// run bounded under an hour, such as the judge's 20 s modelBound, has no time for, so the call is
// not made again (waitable). The providers this package speaks to name their own limits under
// other headers (OpenAI's x-ratelimit-reset-requests and -tokens, Anthropic's
// anthropic-ratelimit-*), which Read does not read, so for them the wait is Retry-After's or the
// second.
func retryable(resp *http.Response, err error) (time.Duration, bool) {
	if err != nil {
		return defaultRetryWait, !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	}
	if !httpoutage.Is(resp.StatusCode) {
		return 0, false
	}
	if wait, named := serverwait.Read(resp.Header, time.Now()); named {
		return wait, true
	}
	return defaultRetryWait, true
}

// waitable is whether the context has the time to wait out a retry: it is not already done, and
// it either carries no deadline or has more than wait left.
func waitable(ctx context.Context, wait time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) > wait
}
