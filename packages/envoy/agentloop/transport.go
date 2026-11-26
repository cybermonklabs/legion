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
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/serverwait"
)

// modelTransport carries every model call of a run. It puts the endpoint's token, read again for
// the attempt, on each request; adds the endpoint's own query parameters to it; takes the run's
// Final schema out of a Messages API body that offers the model a tool and puts it in every other
// body back in the order its caller wrote it (inFinalOrder); and makes the run's one retry, on a
// 429, a 5xx or a transport error, within the run's time.
type modelTransport struct {
	base  http.RoundTripper
	kind  Kind
	token func(context.Context) (string, error)
	query url.Values
	// final is the run's Request.Final, and nil for a run with no Final and for a decision.
	final json.RawMessage
	// log takes the warning for a call whose schema inFinalOrder could not put back. It is
	// never nil: newModelTransport puts slog.Default() in place of a nil one.
	log *slog.Logger
}

// newModelTransport is the transport one Run or one Decide makes its model calls through, over
// the default transport, for the endpoint whose protocol, token and query parameters it carries,
// the run's Final, which a decision has none of, and log, the run's Request.Log, which a decision
// passes as nil since its transport never warns. A nil log is slog.Default(), resolved here once
// for both callers.
func newModelTransport(endpoint Endpoint, final json.RawMessage, log *slog.Logger) *modelTransport {
	return &modelTransport{
		base: http.DefaultTransport, kind: endpoint.Kind, token: endpoint.Token, query: endpoint.Query,
		final: final, log: cmp.Or(log, slog.Default()),
	}
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
	// Once for the call, so the retry sends the very bytes the first attempt sent.
	body = t.inFinalOrder(body)
	ctx := req.Context()
	for attempt := 0; ; attempt++ {
		var key string
		// Azure signs its own calls: the provider's credential middleware sets the header,
		// and this transport reads no token for it.
		if t.kind != AzureAnthropic {
			var err error
			if key, err = t.token(ctx); err != nil {
				return nil, fmt.Errorf("reading the %s token: %w", t.kind, err)
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
		return resp, nil
	}
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
		j := slices.IndexFunc(members, func(m jsonPart) bool { return m.name == name })
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
		if ordered, ok := appendInOrder(append(make([]byte, 0, len(body)), body[:start]...), schema, t.final); ok {
			return append(ordered, body[end:]...)
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
	i := slices.IndexFunc(members, func(m jsonPart) bool { return m.name == "output_config" })
	if i < 0 {
		return body
	}
	config := body[members[i].start:members[i].end]
	inner, _ := jsonParts(config, jsontext.KindBeginObject)
	j := slices.IndexFunc(inner, func(m jsonPart) bool { return m.name == "format" })
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
		if i := slices.IndexFunc(members, func(m jsonPart) bool { return m.name == name }); i >= 0 {
			return body[members[i].start:members[i].end]
		}
		return nil
	}
	if tools, _ := jsonParts(value("tools"), jsontext.KindBeginArray); len(tools) == 0 {
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
			if i := slices.IndexFunc(sentParts, func(p jsonPart) bool { return p.name == f.name }); i >= 0 {
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
