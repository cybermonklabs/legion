package agentloop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/serverwait"
)

// modelTransport carries every model call of a run. It puts the endpoint's token, read again for
// the attempt, on each request; adds the endpoint's own query parameters to it; and makes the
// run's one retry, on a 429, a 5xx or a transport error, within the run's time. It keeps the
// status of the last response it handed back, which is how a provider error is given the HTTP
// status the provider answered with, and whether that response's body names the provider's
// content filter; and it cuts a response past streamLimit bytes.
type modelTransport struct {
	base  http.RoundTripper
	kind  Kind
	token func(context.Context) (string, error)
	query url.Values
	// streamLimit is the most bytes of one response's body the transport hands on, 0 for no
	// limit (Request.StreamLimit); a read past it fails with errStreamTooLarge and sets
	// tooLarge.
	streamLimit int64
	tooLarge    atomic.Bool
	// status is the last attempt's response status where it was not a 2xx, and zero where
	// it was, or where the attempt drew no response at all.
	status atomic.Int32
	// filtered is set where the last attempt's response was not a 2xx and the first
	// filterPeek bytes of its body name the provider's content filter (contentFilterMark).
	filtered atomic.Bool
}

// filterPeek is how many bytes of a refused response's body the transport reads for whether it
// names the provider's content filter, before it hands the whole body on.
const filterPeek = 64 << 10

// contentFilterMark is how a provider's refused response names its content filter: Chat
// Completions' finish reason or error code content_filter, as Azure's endpoints answer a prompt
// their filter blocked (an error body, or a stream chunk sent with the error status), and Azure
// OpenAI's inner code for the same refusal.
var contentFilterMark = regexp.MustCompile(`"(?:finish_reason|code)"\s*:\s*"content_filter"|"ResponsibleAIPolicyViolation"`)

// errStreamTooLarge is a read of a response past the transport's streamLimit.
var errStreamTooLarge = errors.New("the model's response passed its stream limit")

// overLimit reports whether a response of this transport was cut at streamLimit.
func (t *modelTransport) overLimit() bool {
	return t.tooLarge.Load()
}

// failedStatus is the status the last attempt was refused with, or zero where the last attempt
// was answered with a 2xx or drew no response.
func (t *modelTransport) failedStatus() int {
	return int(t.status.Load())
}

// contentFiltered reports whether the last attempt was refused with a response whose body names
// the provider's content filter.
func (t *modelTransport) contentFiltered() bool {
	return t.filtered.Load()
}

// newModelTransport is the transport one Run or one Decide makes its model calls through, over
// the default transport, for the endpoint whose protocol, token and query parameters it carries.
func newModelTransport(endpoint Endpoint) *modelTransport {
	return &modelTransport{base: http.DefaultTransport, kind: endpoint.Kind, token: endpoint.Token, query: endpoint.Query}
}

// client is the HTTP client one Run or one Decide makes its model calls with: this transport,
// and no redirect ever followed. The transport puts the endpoint's credential on every request
// it carries, which would include the one the client makes for a 3xx, and Go strips
// Authorization on a cross-domain redirect before a transport runs, so following one would hand
// the token to whatever server the Location names. A redirect fails the call instead, with an
// error client.Do returns above this transport, so the retry never sees it and the caller is
// told the endpoint redirected rather than left to read an empty stream.
func (t *modelTransport) client() *http.Client {
	return &http.Client{Transport: t, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		// The host alone, since a Location can carry a query: this message names no
		// more of it than the server a credential would have gone to. (Go's own
		// client wraps the error in a *url.Error that names the Location in full.)
		return fmt.Errorf("the %s endpoint answered %s, a redirect to %s, which is never followed",
			t.kind, req.Response.Status, req.URL.Host)
	}}
}

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
		// Each attempt starts with no status of its own, so a token that could not be read,
		// or a request that drew no response, is never given an earlier attempt's.
		t.status.Store(0)
		t.filtered.Store(false)
		var key string
		// Azure signs its own calls: the provider's credential middleware sets the header,
		// and this transport reads no token for it.
		if t.kind != AzureAnthropic {
			var err error
			if key, err = t.readToken(ctx); err != nil {
				return nil, fmt.Errorf("reading the %s token: %w", t.kind, err)
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
		if err == nil && resp.StatusCode/100 != 2 {
			t.status.Store(int32(resp.StatusCode))
		}
		if wait, retry := retryable(resp, err); retry && attempt == 0 && waitable(ctx, wait) {
			if resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
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
		if resp.StatusCode/100 != 2 {
			t.peekFilter(resp)
		}
		if t.streamLimit > 0 {
			resp.Body = &limitedBody{ReadCloser: resp.Body, left: t.streamLimit, over: &t.tooLarge}
		}
		return resp, nil
	}
}

// peekFilter reads the first filterPeek bytes of resp's body, a refused response's, sets filtered
// where they name the provider's content filter, and puts them back in front of the rest, so the
// SDK reads the body as the provider sent it.
func (t *modelTransport) peekFilter(resp *http.Response) {
	head, _ := io.ReadAll(io.LimitReader(resp.Body, filterPeek))
	t.filtered.Store(contentFilterMark.Match(head))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
}

// readToken reads the endpoint's token for one attempt. A Token that panics is recovered and
// answered as an error naming the panic, so the attempt fails and the process goes on: the SDK
// makes its calls on goroutines of its own, where a panic would end the process.
func (t *modelTransport) readToken(ctx context.Context) (key string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("agentloop: an endpoint's token callback panicked", "panic", recovered, "stack", string(debug.Stack()))
			key, err = "", fmt.Errorf("the token callback panicked: %v", recovered)
		}
	}()
	return t.token(ctx)
}

// limitedBody is a response body cut at left bytes: a read past them fails with
// errStreamTooLarge and sets over. A body that ends exactly at the limit ends as it would.
type limitedBody struct {
	io.ReadCloser
	left int64
	over *atomic.Bool
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		if n, err := b.ReadCloser.Read(one[:]); n == 0 && err != nil {
			return 0, err
		}
		b.over.Store(true)
		return 0, errStreamTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
}

// retryable is whether a model call's outcome is retried, and after how long: a transport error,
// 429 or a 5xx, after the wait the response names, read through serverwait.Read, or a second when
// it names none. A wait of an hour is one a run bounded under an hour, such as the judge's 20 s
// modelBound, has no time for, so the call is not made again (waitable). The providers this
// package speaks to name their own limits under other headers (OpenAI's
// x-ratelimit-reset-requests and -tokens, Anthropic's anthropic-ratelimit-*), which Read does not
// read, so for them the wait is Retry-After's or the second.
func retryable(resp *http.Response, err error) (time.Duration, bool) {
	if err != nil {
		return defaultRetryWait, !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	}
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
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
