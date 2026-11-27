package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/httpoutage"
	"github.com/sjawhar/envoy/internal/serverwait"
)

// modelTransport carries every model call of a run. It puts the endpoint's token, read again for
// the attempt, on each request; adds the endpoint's own query parameters to it; and makes the
// run's one retry, on an outage status (a 408, a 429, a 5xx) or a transport error, within the
// run's time. It logs each 429 at WARN, once for the attempt it answered (rateLimited), which is
// the one place a caller's 429 is logged: the agent's runs and the server's short answers each
// make their calls here. It keeps the status of the last response it handed back, which is how a
// provider error is given the HTTP status the provider answered with, whether that response's body
// names the provider's content filter and what the body says of the refusal; and it cuts a
// response past streamLimit bytes.
type modelTransport struct {
	base  http.RoundTripper
	kind  Kind
	token func(context.Context) (string, error)
	query url.Values
	// deployment is the model name the requests name, which a 429 is logged with: the Azure
	// deployment where the endpoint names one, else the model id.
	deployment string
	// streamLimit is the most bytes of one response's body the transport hands on, 0 for no
	// limit (Request.StreamLimit); a read past it fails with errStreamTooLarge and sets
	// tooLarge.
	streamLimit int64
	tooLarge    atomic.Bool
	// status is the last attempt's response status where it was not a 2xx, and zero where
	// it was, or where the attempt drew no response at all.
	status atomic.Int32
	// filtered is set where the last attempt's response was not a 2xx and the first
	// filterPeek bytes of its body name the provider's content filter (contentFilterMark), and
	// filterWords holds what those bytes say of the refusal (ContentFilterRefusal), "" where
	// they say nothing of the shape it reads.
	filtered    atomic.Bool
	filterWords atomic.Pointer[string]
}

// filterPeek is how many bytes of a refused response's body the transport reads for whether it
// names the provider's content filter, before it hands the whole body on.
const filterPeek = 64 << 10

// contentFilterMark is how a provider's refused response names its content filter: Chat
// Completions' finish reason or error code content_filter, as Azure's endpoints answer a prompt
// their filter blocked (an error body, or a stream chunk sent with the error status), and Azure
// OpenAI's inner code for the same refusal.
var contentFilterMark = regexp.MustCompile(`"(?:finish_reason|code)"\s*:\s*"content_filter"|"ResponsibleAIPolicyViolation"`)

// The bounds of what ContentFilterRefusal keeps of a refusal: each error's code and message at
// most filterErrorLimit bytes, and all it keeps at most filterWordsLimit, so a log line and a
// run's reason carry the provider's verdict and never a long body.
const (
	filterErrorLimit = 200
	filterWordsLimit = 400
)

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

// contentFilterWords is what the last attempt's refusal says of the provider's content filter
// (ContentFilterRefusal), "" where it was no such refusal or says nothing of the shape read.
func (t *modelTransport) contentFilterWords() string {
	if words := t.filterWords.Load(); words != nil {
		return *words
	}
	return ""
}

// newModelTransport is the transport one Run or one Decide makes its model calls through, over
// the default transport, for model's endpoint, whose protocol, token and query parameters it
// carries, and the deployment its requests name.
func newModelTransport(model Model) *modelTransport {
	deployment := model.Endpoint.Deployment
	if deployment == "" {
		deployment = model.ID
	}
	return &modelTransport{base: http.DefaultTransport, kind: model.Endpoint.Kind, token: model.Endpoint.Token,
		query: model.Endpoint.Query, deployment: deployment}
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
		t.filterWords.Store(nil)
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
		wait, retry := retryable(resp, err)
		retry = retry && attempt == 0 && waitable(ctx, wait)
		if err == nil && resp.StatusCode == http.StatusTooManyRequests {
			t.rateLimited(attempt, wait, retry)
		}
		if retry {
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
// and filterWords where they name the provider's content filter, and puts them back in front of
// the rest, so the SDK reads the body as the provider sent it.
func (t *modelTransport) peekFilter(resp *http.Response) {
	head, _ := io.ReadAll(io.LimitReader(resp.Body, filterPeek))
	if words, filtered := ContentFilterRefusal(head); filtered {
		t.filtered.Store(true)
		t.filterWords.Store(&words)
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
}

// ContentFilterRefusal reports whether body, or the start of it, the body of a response a
// provider refused a call with, names the provider's content filter (contentFilterMark), and
// what it says of the refusal: each error's code and message, as Azure's endpoints give them (an
// error object, or a stream chunk's content_filter_results.error), and the labels the filter
// flagged (innererror.content_filter_result, content_filter_results, prompt_filter_results), such
// as "content_filter: Response content blocked by label 'Jailbreak'." or "filtered: jailbreak",
// within filterWordsLimit bytes. It keeps no other text of the body, which may echo what the
// call held; the words are "" where the body says nothing of that shape. The body is one JSON
// object, or a stream of server-sent events whose data lines each are; a line the read cut short
// says nothing.
func ContentFilterRefusal(body []byte) (string, bool) {
	if !contentFilterMark.Match(body) {
		return "", false
	}
	var words filterVerdict
	if trimmed := bytes.TrimSpace(body); bytes.HasPrefix(trimmed, []byte("{")) {
		words.read(trimmed)
	} else {
		for line := range bytes.Lines(body) {
			if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
				words.read(bytes.TrimSpace(data))
			}
		}
	}
	return words.String(), true
}

// filterVerdict is what a refused body says of the provider's content filter: its errors, each
// as "code: message", and the labels the filter flagged, each once, in the order read.
type filterVerdict struct {
	errors, labels []string
}

// read adds what one JSON value says of the filter: every error object and every set of the
// filter's results it holds, at any depth, its keys read in order.
func (v *filterVerdict) read(data []byte) {
	var value any
	if json.Unmarshal(data, &value) == nil {
		v.walk(value)
	}
}

func (v *filterVerdict) walk(value any) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			v.walk(item)
		}
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(value)) {
			child, _ := value[key].(map[string]any)
			switch key {
			case "error":
				v.addError(child)
			case "content_filter_result", "content_filter_results":
				v.addLabels(child)
			}
			v.walk(value[key])
		}
	}
}

// addError adds an error object's code and message, the message cut at filterErrorLimit bytes.
func (v *filterVerdict) addError(object map[string]any) {
	code, _ := object["code"].(string)
	message, _ := object["message"].(string)
	text := strings.TrimSpace(code)
	if message = strings.Join(strings.Fields(message), " "); message != "" {
		if text != "" {
			text += ": "
		}
		text += message
	}
	if text = strings.TrimSpace(cutBytes(text, filterErrorLimit)); text != "" && !slices.Contains(v.errors, text) {
		v.errors = append(v.errors, text)
	}
}

// addLabels adds each label of a set of the filter's results that it filtered: {"jailbreak":
// {"filtered": true, "detected": true}} flags jailbreak. A label it only detected, or rated safe,
// refused nothing.
func (v *filterVerdict) addLabels(results map[string]any) {
	for _, label := range slices.Sorted(maps.Keys(results)) {
		if result, ok := results[label].(map[string]any); ok && result["filtered"] == true && !slices.Contains(v.labels, label) {
			v.labels = append(v.labels, label)
		}
	}
}

func (v *filterVerdict) String() string {
	parts := slices.Clone(v.errors)
	if len(v.labels) > 0 {
		parts = append(parts, "filtered: "+strings.Join(v.labels, ", "))
	}
	return cutBytes(strings.Join(parts, "; "), filterWordsLimit)
}

// cutBytes is text cut to at most limit bytes, at a rune's start.
func cutBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// rateLimited logs a 429 the model service answered attempt (0 for the first) with, at WARN:
// the deployment the request named, the attempt, from 1, the wait before the retry (the
// Retry-After the service gave, or defaultRetryWait where it gave none), and whether the call is
// made again after it, which it is not on the retry itself or where the run has no time for the
// wait.
func (t *modelTransport) rateLimited(attempt int, wait time.Duration, retry bool) {
	slog.Warn("agentloop: the model service answered 429, its rate limit",
		"deployment", t.deployment, "attempt", attempt+1, "wait", wait, "retry", retry)
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
