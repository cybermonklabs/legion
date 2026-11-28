package agentloop

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
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
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/serverwait"
)

// modelTransport carries every model call of a run. It puts the endpoint's token, read again for
// the attempt, on each request; adds the endpoint's own query parameters to it; takes the run's
// Final schema out of a Messages API body that offers the model a tool and puts it in every other
// body back in the order its caller wrote it (inFinalOrder); and makes the run's one retry, on an
// outage status (a 408, a 429, a 5xx) or a transport error, within the run's time. It logs each
// 429 at WARN, once for the attempt it answered (rateLimited), which is the one place a caller's
// 429 is logged: the agent's runs and the server's short answers each make their calls here. It
// keeps the status of the last response it handed back, which is how a provider error is given
// the HTTP status the provider answered with, whether that response's body names the provider's
// content filter and what the body says of the refusal; and it cuts a response past streamLimit
// bytes.
type modelTransport struct {
	base  http.RoundTripper
	kind  Kind
	token func(context.Context) (string, error)
	query url.Values
	// endpoint is the configured base URL, and posted the URL of the last request the
	// transport carried, as the SDK or Decide built it: a model call's failure is printed
	// over both (failure).
	endpoint string
	posted   atomic.Pointer[string]
	// deployment is the model name the requests name, which a 429 is logged with: the Azure
	// deployment where the endpoint names one, else the model id (deploymentOf).
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
	// final is the run's Request.Final, and nil for a run with no Final and for a decision.
	final json.RawMessage
	// log takes the warning for a call whose schema inFinalOrder could not put back. It is
	// never nil: newModelTransport puts slog.Default() in place of a nil one.
	log *slog.Logger
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
// the default transport, for the endpoint whose protocol, token, query parameters and base URL it
// carries, the run's Final, which a decision has none of, and log, the run's Request.Log, which a
// decision passes as nil since its transport never warns. A nil log is slog.Default(), resolved
// here once for both callers. Run and Decide set the deployment a 429 is logged with
// (deploymentOf).
func newModelTransport(endpoint Endpoint, final json.RawMessage, log *slog.Logger) *modelTransport {
	return &modelTransport{
		base: http.DefaultTransport, kind: endpoint.Kind, token: endpoint.Token, query: endpoint.Query,
		endpoint: endpoint.BaseURL, final: final, log: cmp.Or(log, slog.Default()),
	}
}

// deploymentOf is the model name model's requests name, which a 429 is logged with: the Azure
// deployment where the endpoint names one, else the model id.
func deploymentOf(model Model) string {
	return cmp.Or(model.Endpoint.Deployment, model.ID)
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
		// client wraps the error in a *url.Error that names the Location in full,
		// which failure leaves out.)
		return &ownFailure{fmt.Errorf("the %s endpoint answered %s, a redirect to %s, which is never followed",
			t.kind, req.Response.Status, req.URL.Host)}
	}}
}

// ownFailure is a model call's failure in this package's own words: a token the transport could
// not read for an attempt, and a redirect the client refused. client.Do wraps it in a *url.Error,
// as it wraps every failure of a call, and the SDK may wrap that again; failure prints its words
// alone, which name no part of the request's URL: a refused redirect names the host its Location
// points at and no more of it, and a token's failure is the endpoint's Token's own error.
type ownFailure struct{ err error }

func (f *ownFailure) Error() string { return f.err.Error() }
func (f *ownFailure) Unwrap() error { return f.err }

// failure is err, a model call's failure as client.Do, or the SDK over it, returned it, as a
// run's or a decision's Detail prints it. client.Do wraps every failure of a call in a
// *url.Error, whose text quotes the request URL with only a password masked, so an endpoint
// holding a token before "@" printed it there. A failure in this package's own words (ownFailure)
// prints those words, without the quote. Any other failure under a *url.Error, net/http's,
// prints through redact.CauseAt: its own text when the endpoint is a clean http(s) URL and the
// URL net/http names is the one the transport carried (a refused dial's address, a TLS failure's
// x509 cause), and its class alone otherwise, a redirect's target or an endpoint holding a
// credential among them. A failure with no *url.Error under it, where the last attempt was
// refused with a status (failedStatus), is that status as the provider's SDK words it.
// Anthropic's SDK quotes the request URL whole in those words, password and all, so where the
// endpoint is no clean URL (redact.Clean) every copy of the URL the transport carried prints as
// redact.NotPrinted. Every other failure, of a call the transport never carried or of a response
// that broke after its 2xx, and a status's words beside a clean endpoint, whose URL holds no
// credential since the endpoint's query parameters go on the attempt's copy alone, print as
// shownFailure prints them.
func (t *modelTransport) failure(err error) error {
	var posted string
	if p := t.posted.Load(); p != nil {
		posted = *p
	}
	var own *ownFailure
	var quoted *url.Error
	switch {
	case errors.As(err, &own):
		return own
	case errors.As(err, &quoted):
		return redact.CauseAt(err, t.endpoint, posted)
	case posted != "" && t.failedStatus() != 0 && !redact.Clean(t.endpoint):
		return errors.New(strings.ReplaceAll(err.Error(), posted, redact.NotPrinted))
	}
	return shownFailure(t.endpoint, err)
}

// shownFailure is err, the failure of a model call to the endpoint at base, as a Result's Detail
// prints it. net/http's error quotes the request URL with only a password masked, a dial's or a
// resolver's error names the host, which is a password's tail where the password holds an
// unencoded "@", and a URL that does not parse is quoted whole. So beside a base that is no clean
// http(s) URL (redact.Clean: one with a user name or a password among them) err prints as
// redact.Error prints it: the call's operation, its URL as redact.NotPrinted, and the failure's
// class, with no word of the URL. A clean base holds no credential, so beside one err prints
// whole. A failure of a call the transport carried prints through failure, which prints here
// every failure it does not word itself.
func shownFailure(base string, err error) error {
	if redact.Clean(base) {
		return err
	}
	return redact.Error(err, base)
}

// defaultRetryWait is how long the retry waits when the response names no wait (retryable).
const defaultRetryWait = time.Second

func (t *modelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// The URL net/http's *url.Error names if this request fails: the request as the SDK or
	// Decide built it, before an attempt's copy takes the endpoint's query parameters.
	posted := req.URL.String()
	t.posted.Store(&posted)
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
	}
	// Once for the call, so the retry sends the very bytes the first attempt sent.
	body = t.inFinalOrder(body)
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
				return nil, &ownFailure{fmt.Errorf("reading the %s token: %w", t.kind, err)}
			}
		}
		r := req.Clone(ctx)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		// The base transport reads the body again through GetBody when it sends the request
		// again on a new connection, after a kept-alive one closed under it, and the clone's
		// GetBody gives the SDK's own bytes, not these.
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
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

// finalSchemaPath is where a request body of the protocol carries the run's Final schema, as the
// SDK's request builders write it: output_config.format.schema on the Messages API, which
// azure-anthropic speaks as well, text.format.schema on the Responses API, and
// response_format.json_schema.schema on Chat Completions. A decision carries no schema.
func finalSchemaPath(kind Kind) []string {
	switch kind {
	case AnthropicMessages, AzureAnthropic:
		return []string{"output_config", "format", "schema"}
	case OpenAIResponses:
		return []string{"text", "format", "schema"}
	case OpenAIChat:
		return []string{"response_format", "json_schema", "schema"}
	default:
		return nil
	}
}

// inFinalOrder is body with the schema it carries at its protocol's path (finalSchemaPath) put
// back in the order of Request.Final: every byte of the body stays as it came but for that
// schema's objects, each of which takes the order of the same object in Final (appendInOrder);
// why is doc.go's "The structured reply". On the Messages API a body that offers the model a tool
// (offersTools) goes out with no schema at all, and logs nothing (withoutFormat); why is the same
// section's. A run with no Final and a decision are sent byte for byte as they came. So are a
// body that carries no schema at the path, a schema of another JSON kind than Final's, a Final
// that is no JSON the transport can read, and a body that is no JSON, which is the provider's to
// refuse; but every other call of a run with a Final carries its schema there, so each of these
// logs one warning for the call.
func (t *modelTransport) inFinalOrder(body []byte) []byte {
	path := finalSchemaPath(t.kind)
	if t.final == nil || path == nil {
		return body
	}
	// Such a call sends the schema as the SDK wrote it, sorted on the Anthropic and Responses
	// adapters, which is the order this function is here to undo, so it must not go unseen.
	unordered := func(reason string) []byte {
		t.log.Warn("agentloop: the final schema was not put back in its caller's order, so this call sends it as the SDK wrote it",
			"protocol", string(t.kind), "reason", reason)
		return body
	}
	// The body's own members are read once: they say whether the body offers a tool, and they
	// are the walk's first step.
	members, ok := jsonParts(body, jsontext.KindBeginObject)
	if !ok {
		return unordered("the body is no JSON object")
	}
	if anthropicKind(t.kind) && offersTools(body, members) {
		return withoutFormat(body, members)
	}
	start, end := 0, len(body)
	for i, name := range path {
		if i > 0 {
			if members, ok = jsonParts(body[start:end], jsontext.KindBeginObject); !ok {
				return unordered(strings.Join(path[:i], ".") + " is no JSON object")
			}
		}
		j := memberIndex(members, name)
		if j < 0 {
			return unordered(fmt.Sprintf("no member %q on the path %s", name, strings.Join(path, ".")))
		}
		start, end = start+members[j].start, start+members[j].end
	}
	// appendInOrder appends a value whose kind is not Final's as it stands, which for the
	// schema itself would put nothing back.
	schema := body[start:end]
	sent, want := jsontext.Value(schema).Kind(), jsontext.Value(t.final).Kind()
	if sent == want {
		if ordered, ok := appendInOrder(make([]byte, 0, len(schema)), schema, t.final); ok {
			// A schema the SDK sent in Final's order already leaves the body as it came, and
			// no copy of it is made.
			if bytes.Equal(ordered, schema) {
				return body
			}
			return slices.Concat(body[:start], ordered, body[end:])
		}
	} else if name, ok := jsonKinds[want]; ok {
		return unordered(fmt.Sprintf("the schema at %s is a JSON %s where Final is a JSON %s",
			strings.Join(path, "."), jsonKinds[sent], name))
	}
	// The walk to the schema read every value of the body, so what could not be read here is
	// Final: a key it holds twice, or bytes that are no JSON.
	return unordered("Final is no JSON the transport can read")
}

// withoutFormat is body, a Messages API request body that offers the model a tool (offersTools)
// and whose members are members, with its output_config.format taken out. Every other byte of the
// body stays as it came, the rest of output_config, the model's effort, among them; an
// output_config that held the format and nothing else goes too, as the SDK writes none for a
// request with neither. A body that holds no format is sent as it came.
func withoutFormat(body []byte, members []jsonPart) []byte {
	i := memberIndex(members, "output_config")
	if i < 0 {
		return body
	}
	config := body[members[i].start:members[i].end]
	inner, _ := jsonParts(config, jsontext.KindBeginObject)
	j := memberIndex(inner, "format")
	switch {
	case j < 0:
		return body
	case len(inner) == 1:
		return withoutMember(body, members, i)
	}
	return slices.Concat(body[:members[i].start], withoutMember(config, inner, j), body[members[i].end:])
}

// offersTools is whether a Messages API request body, a JSON object whose members are members,
// offers the model a tool: its tools array holds one, and its tool_choice is absent, which the
// API reads as auto, or is an object whose type is a string other than none. The SDK writes a
// step's tools whatever that step's tool choice, and writes the ToolChoiceNone prepareStep gives
// the run's last call as {"type":"none"}, so the tools array alone does not tell that call from
// the others.
func offersTools(body []byte, members []jsonPart) bool {
	value := func(name string) []byte {
		if i := memberIndex(members, name); i >= 0 {
			return body[members[i].start:members[i].end]
		}
		return nil
	}
	// The body was read whole as JSON (jsonParts), so a tools array holds a tool where what
	// follows its opening bracket is not its closing one.
	tools := jsontext.NewDecoder(bytes.NewBuffer(value("tools")))
	if open, err := tools.ReadToken(); err != nil || open.Kind() != jsontext.KindBeginArray ||
		tools.PeekKind() == jsontext.KindEndArray {
		return false
	}
	choice := value("tool_choice")
	if choice == nil {
		return true
	}
	var read struct {
		Type *string `json:"type"`
	}
	return json.Unmarshal(choice, &read) == nil && read.Type != nil && *read.Type != "none"
}

// withoutMember is object, a JSON object whose members are parts, with its member j taken out
// together with the comma that parted it from its neighbour: the one before it, or for the first
// of several the one after it. Every other byte stays as it came.
func withoutMember(object []byte, parts []jsonPart, j int) []byte {
	from, to := bytes.IndexByte(object, '{')+1, parts[j].end
	switch {
	case j > 0:
		from = parts[j-1].end
	case len(parts) > 1:
		to += bytes.IndexByte(object[to:], ',') + 1
	}
	return slices.Concat(object[:from], object[to:])
}

// jsonKinds names the kind each JSON value begins with, for a warning's reason.
var jsonKinds = map[jsontext.Kind]string{
	jsontext.KindBeginObject: "object", jsontext.KindBeginArray: "array", jsontext.KindString: "string",
	jsontext.KindNumber: "number", jsontext.KindTrue: "true", jsontext.KindFalse: "false",
	jsontext.KindNull: "null",
}

// appendInOrder appends sent, a value of the schema a request body carries, to dst with each
// object in it in the order of final, the value at the same place in Request.Final: the keys
// final holds come first, in final's order, and a key final lacks keeps its place after them.
// It goes on into each member's value with final's value at that key, and into an array element
// by element with final's element of the same index. Every other value, and a value final holds
// nothing of the same kind for, is appended as its bytes stand, so strings and numbers reach the
// provider as the SDK wrote them. A key is written again with no HTML escaping, as RFC 8785
// writes a string. It is false where sent or final is no JSON.
func appendInOrder(dst, sent, final []byte) ([]byte, bool) {
	kind := jsontext.Value(sent).Kind()
	if (kind != jsontext.KindBeginObject && kind != jsontext.KindBeginArray) || jsontext.Value(final).Kind() != kind {
		return append(dst, sent...), true
	}
	sentParts, ok := jsonParts(sent, kind)
	if !ok {
		return nil, false
	}
	finalParts, ok := jsonParts(final, kind)
	if !ok {
		return nil, false
	}
	// Each part of sent in the order it is written, beside final's value at the same place.
	type pair struct {
		sent  jsonPart
		final []byte
	}
	pairs := make([]pair, 0, len(sentParts))
	end := jsontext.KindEndArray
	if kind == jsontext.KindBeginArray {
		for i, p := range sentParts {
			var f []byte
			if i < len(finalParts) {
				f = final[finalParts[i].start:finalParts[i].end]
			}
			pairs = append(pairs, pair{sent: p, final: f})
		}
	} else {
		end = jsontext.KindEndObject
		placed := make([]bool, len(sentParts))
		for _, f := range finalParts {
			if i := memberIndex(sentParts, f.name); i >= 0 {
				pairs = append(pairs, pair{sent: sentParts[i], final: final[f.start:f.end]})
				placed[i] = true
			}
		}
		for i, p := range sentParts {
			if !placed[i] {
				pairs = append(pairs, pair{sent: p})
			}
		}
	}
	dst = append(dst, byte(kind))
	for i, p := range pairs {
		if i > 0 {
			dst = append(dst, ',')
		}
		if kind == jsontext.KindBeginObject {
			var err error
			if dst, err = jsontext.AppendQuote(dst, p.sent.name); err != nil {
				return nil, false
			}
			dst = append(dst, ':')
		}
		if dst, ok = appendInOrder(dst, sent[p.sent.start:p.sent.end], p.final); !ok {
			return nil, false
		}
	}
	return append(dst, byte(end)), true
}

// jsonPart is one member of a JSON object, its name and where its value lies in the object's
// bytes, or one element of a JSON array, which has no name.
type jsonPart struct {
	name       string
	start, end int
}

// memberIndex is the index in parts, the members of a JSON object, of the member named name, and
// -1 where the object holds none.
func memberIndex(parts []jsonPart, name string) int {
	return slices.IndexFunc(parts, func(p jsonPart) bool { return p.name == name })
}

// jsonParts is the members of value, a JSON object, or the elements of value, a JSON array, in
// the order its bytes hold them. kind is the kind value begins with; ok is false where value
// begins with another or is no JSON.
func jsonParts(value []byte, kind jsontext.Kind) (all []jsonPart, ok bool) {
	// A bytes.Buffer is read in place: no byte of value is copied, and an offset the decoder
	// reports is an offset into value itself.
	dec := jsontext.NewDecoder(bytes.NewBuffer(value))
	if open, err := dec.ReadToken(); err != nil || open.Kind() != kind {
		return nil, false
	}
	for {
		switch dec.PeekKind() {
		case jsontext.KindEndObject, jsontext.KindEndArray:
			return all, true
		}
		var p jsonPart
		if kind == jsontext.KindBeginObject {
			name, err := dec.ReadToken()
			if err != nil {
				return nil, false
			}
			p.name = name.String()
		}
		v, err := dec.ReadValue()
		if err != nil {
			return nil, false
		}
		p.end = int(dec.InputOffset())
		p.start = p.end - len(v)
		all = append(all, p)
	}
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
