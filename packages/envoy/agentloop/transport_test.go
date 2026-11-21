package agentloop

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logtest"
)

// TestRetryableBoundsTheWaitTheEndpointNames: the one retry of a model call reads the wait the
// endpoint names by the one rule every reader of a server's wait in this module keeps
// (internal/serverwait). A Retry-After of 9999999999 seconds and an X-RateLimit-Reset of
// math.MaxInt64 with no requests left are past what a Duration holds and read as one hour, which
// a run bounded under an hour, such as the judge's 20 s modelBound, has no time for, so the call
// is not made again (this test's run has two minutes): converted as it came, the count read as a
// negative wait, which any run had time for, and the call was made again at once. A negative
// Retry-After reads as no wait, an ordinary value of each header as itself, and an answer naming
// none waits defaultRetryWait. The test counts the rows it drove and fails under the number the
// table must hold.
func TestRetryableBoundsTheWaitTheEndpointNames(t *testing.T) {
	const rowsToDrive = 6
	now := time.Now()
	exhausted := func(reset string) http.Header {
		return http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}}
	}
	rows := []struct {
		name     string
		status   int
		header   http.Header
		min, max time.Duration
		// again is whether a run with two minutes left waits the wait and makes the call again.
		again bool
	}{
		{"a 429 asking 9999999999 seconds", http.StatusTooManyRequests, http.Header{"Retry-After": {"9999999999"}}, time.Hour, time.Hour, false},
		{"a 429 whose X-RateLimit-Reset is math.MaxInt64 with no requests left", http.StatusTooManyRequests, exhausted(strconv.FormatInt(math.MaxInt64, 10)), time.Hour, time.Hour, false},
		{"a 429 asking a negative wait", http.StatusTooManyRequests, http.Header{"Retry-After": {"-30"}}, 0, 0, true},
		{"a 429 asking 30 seconds", http.StatusTooManyRequests, http.Header{"Retry-After": {"30"}}, 30 * time.Second, 30 * time.Second, true},
		// The reset is whole seconds, and retryable reads it against its own clock a moment
		// later, so the wait is up to two seconds short of the 90 it names.
		{"a 503 whose X-RateLimit-Reset is 90 seconds out with no requests left", http.StatusServiceUnavailable, exhausted(strconv.FormatInt(now.Add(90*time.Second).Unix(), 10)), 88 * time.Second, 90 * time.Second, true},
		{"a 503 naming no wait", http.StatusServiceUnavailable, http.Header{}, defaultRetryWait, defaultRetryWait, true},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	driven := 0
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			driven++
			wait, retry := retryable(&http.Response{StatusCode: row.status, Header: row.header}, nil)
			if !retry || wait < row.min || wait > row.max {
				t.Errorf("retryable = %s (retry %t), want %s to %s and a retry", wait, retry, row.min, row.max)
			}
			if again := waitable(ctx, wait); again != row.again {
				t.Errorf("a run with two minutes left makes the call again after %s: %t, want %t", wait, again, row.again)
			}
		})
	}
	if driven < rowsToDrive {
		t.Fatalf("the table drove %d rows, want %d", driven, rowsToDrive)
	}
}

// orderedFinal is a Final schema each of whose objects of more than one key is written out of
// sorted order: the root's keywords, its properties (kind, text, seen, needs), the kind
// property's keywords, and seen, a nested object whose keywords are written as the root's and
// whose own properties are when, by and at. orderedAnswer is a reply of that form.
const (
	orderedFinal = `{"type":"object","properties":{` +
		`"kind":{"type":"string","enum":["answered","not_answered"]},` +
		`"text":{"type":"string"},` +
		`"seen":{"type":"object","properties":{"when":{"type":"string"},"by":{"type":"string"},"at":{"type":"string"}},` +
		`"required":["when","by","at"],"additionalProperties":false},` +
		`"needs":{"type":"string"}},` +
		`"required":["kind","text","seen","needs"],"additionalProperties":false}`
	orderedAnswer = `{"kind":"answered","text":"plum","seen":{"when":"today","by":"the reader","at":"noon"},"needs":""}`
)

// TestTheFinalSchemaReachesTheProviderInTheOrderItIsWritten: on every protocol Run serves, each
// object of the schema the call's body carries holds its keys in the order Request.Final writes
// them, the nested object's included (doc.go, "The structured reply").
func TestTheFinalSchemaReachesTheProviderInTheOrderItIsWritten(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		kind  Kind
		reply reply
		// schema is the path of the schema in the protocol's request body.
		schema []string
	}{
		{kind: AnthropicMessages, schema: []string{"output_config", "format", "schema"},
			reply: anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText(orderedAnswer))},
		{kind: AzureAnthropic, schema: []string{"output_config", "format", "schema"},
			reply: anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText(orderedAnswer))},
		{kind: OpenAIResponses, schema: []string{"text", "format", "schema"},
			reply: openaiStream(openaiUsage{Input: 1, Output: 1}, "", openaiMessage("msg_1", orderedAnswer))},
		{kind: OpenAIChat, schema: []string{"response_format", "json_schema", "schema"},
			reply: chatStream("stop", chatUsage{Prompt: 1, Completion: 1}, orderedAnswer)},
	} {
		t.Run(string(c.kind), func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, c.reply)
			f := newFixture(t, s, c.kind, 1)
			f.req.Final = json.RawMessage(orderedFinal)
			if result, err := Run(context.Background(), f.req); err != nil || result.Fault != "" {
				t.Fatalf("result %+v, %v; want the answer and no fault", result, err)
			}
			got := s.requests()
			if len(got) != 1 {
				t.Fatalf("%d model calls, want 1", len(got))
			}
			schema := json.RawMessage(got[0].body)
			for _, key := range c.schema {
				var object map[string]json.RawMessage
				if err := json.Unmarshal(schema, &object); err != nil {
					t.Fatalf("the request body at %q: %v", key, err)
				}
				if schema = object[key]; schema == nil {
					t.Fatalf("the request body holds no %q on the path %v", key, c.schema)
				}
			}
			holdsFinalsOrder(t, schema, json.RawMessage(orderedFinal), "the schema")
		})
	}
}

// holdsFinalsOrder fails t where an object of sent, the schema a request body carries, does not
// begin with the keys of the same object of final, in final's order. It goes on into the values
// of each object whose keys are in that order and into each array's elements, so it fails once
// for the outermost object out of order on a path, and names that object by its path from at.
func holdsFinalsOrder(t *testing.T, sent, final json.RawMessage, at string) {
	t.Helper()
	switch bytes.TrimSpace(final)[0] {
	case '{':
		sentKeys, sentValues := objectMembers(t, sent)
		finalKeys, finalValues := objectMembers(t, final)
		if len(sentKeys) < len(finalKeys) || !slices.Equal(sentKeys[:len(finalKeys)], finalKeys) {
			t.Errorf("%s reaches the provider with its keys in the order %v, want them to begin with %v, Final's order",
				at, sentKeys, finalKeys)
			return
		}
		for i, key := range finalKeys {
			holdsFinalsOrder(t, sentValues[i], finalValues[i], at+"."+key)
		}
	case '[':
		var sentElements, finalElements []json.RawMessage
		if err := json.Unmarshal(sent, &sentElements); err != nil {
			t.Fatalf("%s: %v", at, err)
		}
		if err := json.Unmarshal(final, &finalElements); err != nil {
			t.Fatalf("%s in Final: %v", at, err)
		}
		if len(sentElements) != len(finalElements) {
			t.Errorf("%s reaches the provider with %d elements, want Final's %d", at, len(sentElements), len(finalElements))
			return
		}
		for i := range finalElements {
			holdsFinalsOrder(t, sentElements[i], finalElements[i], fmt.Sprintf("%s[%d]", at, i))
		}
	}
}

// objectMembers is the keys of the JSON object value in the order its bytes hold them, and each
// key's value.
func objectMembers(t *testing.T, value json.RawMessage) ([]string, []json.RawMessage) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(value))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("not an object: %s", value)
	}
	var keys []string
	var values []json.RawMessage
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		var v json.RawMessage
		if err := decoder.Decode(&v); err != nil {
			t.Fatal(err)
		}
		keys, values = append(keys, token.(string)), append(values, v)
	}
	return keys, values
}

// recordingBase is a base transport that keeps what each attempt of a call carried - its body,
// what its GetBody gives, and its Content-Length - and answers the statuses it was given in turn.
type recordingBase struct {
	statuses []int
	bodies   [][]byte
	again    [][]byte
	lengths  []int64
}

func (b *recordingBase) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	reread, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	again, err := io.ReadAll(reread)
	if err != nil {
		return nil, err
	}
	b.bodies, b.again, b.lengths = append(b.bodies, body), append(b.again, again), append(b.lengths, req.ContentLength)
	status := b.statuses[len(b.bodies)-1]
	// A negative Retry-After reads as no wait, so the retry is made at once.
	return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"-1"}}, Body: http.NoBody}, nil
}

// TestTheRetryAndTheRereadCarryTheSchemaInFinalsOrder: the one retry of a call, and the body the
// base transport reads again through GetBody on a new connection, are the bytes the first attempt
// sent, with their length - the schema in Final's order, a key Final lacks after Final's own, and
// every byte outside the schema as the SDK wrote it, a `schema` key inside a message included.
func TestTheRetryAndTheRereadCarryTheSchemaInFinalsOrder(t *testing.T) {
	t.Parallel()
	const (
		final = `{"type":"object","properties":{"kind":{"type":"string"},"text":{"type":"string"},"needs":{"type":"string"}}}`
		// The SDK's body: the schema's keys sorted, `description` added, and a message that
		// holds a sorted `schema` of its own, which is the conversation's and stays as it is.
		sent = `{"model": "m", "messages":[{"schema":{"b":1,"a":2}}],"output_config":{"format":{"schema":` +
			`{"description":"d","properties":{"kind":{"type":"string"},"needs":{"type":"string"},"text":{"type":"string"}},"type":"object"}` +
			`,"type":"json_schema"}},"stream":true}`
		want = `{"model": "m", "messages":[{"schema":{"b":1,"a":2}}],"output_config":{"format":{"schema":` +
			`{"type":"object","properties":{"kind":{"type":"string"},"text":{"type":"string"},"needs":{"type":"string"}},"description":"d"}` +
			`,"type":"json_schema"}},"stream":true}`
	)
	base := &recordingBase{statuses: []int{http.StatusServiceUnavailable, http.StatusOK}}
	transport := newModelTransport(Endpoint{
		Kind:  AnthropicMessages,
		Token: func(context.Context) (string, error) { return "key", nil },
	}, json.RawMessage(final), nil)
	transport.base = base
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://model.example/v1/messages", strings.NewReader(sent))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("the call: %v, %v; want the retry's 200", resp, err)
	}
	if len(base.bodies) != 2 {
		t.Fatalf("%d attempts, want the call and its one retry", len(base.bodies))
	}
	for attempt := range base.bodies {
		if got := string(base.bodies[attempt]); got != want {
			t.Errorf("attempt %d sent\n%s\nwant\n%s", attempt, got, want)
		}
		if got := string(base.again[attempt]); got != want {
			t.Errorf("attempt %d's GetBody gives\n%s\nwant the bytes it sent\n%s", attempt, got, want)
		}
		if base.lengths[attempt] != int64(len(want)) {
			t.Errorf("attempt %d's Content-Length is %d, want %d", attempt, base.lengths[attempt], len(want))
		}
	}
}

// TestASchemaNotPutBackIsSentAsItCameWithOneWarning: a call of a run with a Final whose body holds
// no schema at its protocol's path, for a member missing or a value on the path that is no object,
// holds one of another JSON kind than Final's, or is no JSON, or whose Final is no JSON the
// transport can read, is sent byte for byte as it came, its retry too, and logs one warning naming
// the protocol and which of these it was, since such a call sends the schema in the SDK's order,
// which the Anthropic and Responses adapters sort (inFinalOrder). A body whose schema is put back
// logs nothing.
func TestASchemaNotPutBackIsSentAsItCameWithOneWarning(t *testing.T) {
	t.Parallel()
	const final = `{"type":"object","properties":{"kind":{"type":"string"},"text":{"type":"string"}}}`
	for _, c := range []struct {
		name string
		kind Kind
		body string
		// final is the run's Final, where that is not the table's.
		final string
		// sent is what each attempt carries, where that is not body itself.
		sent string
		// reason is the warning's, and empty where the schema is put back and nothing is logged.
		reason string
	}{
		{name: "no schema at the path", kind: OpenAIChat,
			body:   `{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"reply","strict":true}}}`,
			reason: `no member "schema" on the path response_format.json_schema.schema`},
		{name: "a value on the path that is no object", kind: OpenAIResponses,
			body: `{"model":"m","text":{"format":"plain"}}`, reason: "text.format is no JSON object"},
		{name: "a schema that is an array where Final is an object", kind: OpenAIResponses,
			body:   `{"model":"m","text":{"format":{"type":"json_schema","schema":[{"type":"string"},{"type":"object"}]}}}`,
			reason: "the schema at text.format.schema is a JSON array where Final is a JSON object"},
		// Final holds the key twice, not the body's schema: the walk to the schema reads the whole
		// body and refuses one whose schema holds a key twice as no JSON object.
		{name: "a Final that holds a key twice", kind: AnthropicMessages,
			final:  `{"type":"object","type":"object","properties":{"kind":{"type":"string"}}}`,
			body:   `{"model":"m","output_config":{"format":{"schema":{"properties":{"kind":{"type":"string"}},"type":"object"}}}}`,
			reason: "Final is no JSON the transport can read"},
		{name: "a body that is no JSON", kind: AzureAnthropic,
			body: `{"model":"m","output_config":{"format":`, reason: "the body is no JSON object"},
		{name: "a schema put back", kind: AnthropicMessages,
			body: `{"model":"m","output_config":{"format":{"schema":` +
				`{"properties":{"kind":{"type":"string"},"text":{"type":"string"}},"type":"object"}}}}`,
			sent: `{"model":"m","output_config":{"format":{"schema":` +
				`{"type":"object","properties":{"kind":{"type":"string"},"text":{"type":"string"}}}}}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			logs := &logtest.Recorder{}
			base := &recordingBase{statuses: []int{http.StatusServiceUnavailable, http.StatusOK}}
			transport := newModelTransport(Endpoint{
				Kind:  c.kind,
				Token: func(context.Context) (string, error) { return "key", nil },
			}, json.RawMessage(cmp.Or(c.final, final)), slog.New(logs))
			transport.base = base
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://model.example/v1/call", strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			if resp, err := transport.RoundTrip(req); err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("the call: %v, %v; want the retry's 200", resp, err)
			}
			if len(base.bodies) != 2 {
				t.Fatalf("%d attempts, want the call and its one retry", len(base.bodies))
			}
			want := cmp.Or(c.sent, c.body)
			for attempt, got := range base.bodies {
				if string(got) != want {
					t.Errorf("attempt %d sent\n%s\nwant\n%s", attempt, got, want)
				}
			}
			var logged []string
			for _, r := range logs.Records() {
				attrs := make(map[string]string)
				r.Attrs(func(a slog.Attr) bool {
					attrs[a.Key] = a.Value.String()
					return true
				})
				logged = append(logged, fmt.Sprintf("%s protocol=%q reason=%q, %d attributes",
					r.Level, attrs["protocol"], attrs["reason"], len(attrs)))
			}
			var wantLogged []string
			if c.reason != "" {
				wantLogged = []string{fmt.Sprintf("%s protocol=%q reason=%q, 2 attributes", slog.LevelWarn, c.kind, c.reason)}
			}
			if !slices.Equal(logged, wantLogged) {
				t.Errorf("the call logged %q, want %q", logged, wantLogged)
			}
		})
	}
}
