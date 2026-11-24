package redact_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// answerOf is the Answer of a GET of a server that answers status and the body answer makes of the
// request's Authorization, sent as a bearer of redacttest.Token with net/http's own client, as most
// clients of this repository send theirs.
func answerOf(t *testing.T, status int, answer func(auth string) string) redact.Answer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer(r.Header.Get("Authorization")))
	}))
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+redacttest.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	read, err := redact.ReadAnswer(resp, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return read
}

// TestAnAnswersDecodeErrorQuotesNoValue: a 200 answer echoes the request's bearer into a member
// the client decodes as a time, as a number, or into nothing JSON at all, or answers a number
// where the client holds the box, or echoes the bearer as a key of an object the client decodes
// into a map. encoding/json's error, and time.Time's, quote the value, and since Go 1.27 runs json
// v1 on v2 the path of its type error holds the answer's own map keys and its own spelling of a
// member's name; Decode's error names the member as the destination's type names it, a map's key
// and a list's item by a fixed word, and the destination's type, and quotes none of it, the box's
// member among them, whose own type error names no field. Captures redact.answer-decode/<case>.
func TestAnAnswersDecodeErrorQuotesNoValue(t *testing.T) {
	type item struct {
		N int `json:"n"`
	}
	type renewed struct {
		LeaseExpiresAt time.Time         `json:"lease_expires_at"`
		Seconds        int               `json:"seconds"`
		Code           redact.RemoteText `json:"code"`
		Values         map[string]int    `json:"values"`
		Nested         map[string]item   `json:"nested"`
		List           []item            `json:"list"`
	}
	for _, c := range []struct{ name, body, names string }{
		{"time", `{"lease_expires_at":"renewed with {auth}"}`, `"lease_expires_at"`},
		{"number", `{"seconds":"renewed with {auth}"}`, `"seconds"`},
		{"box", `{"code":7}`, `"code"`},
		{"not-json", `renewed with {auth}`, "not JSON"},
		{"map-key", `{"values":{"{auth}":"renewed"}}`, `"values.(key)"`},
		{"map-key-nested", `{"nested":{"{auth}":{"n":"renewed"}}}`, `"nested.(key).n"`},
		{"map-key-name-case", `{"VALUES":{"{auth}":"renewed"}}`, `"values.(key)"`},
		{"list", `{"list":[{"n":1},{"n":"renewed"}]}`, `"list.(item).n"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			answer := answerOf(t, http.StatusOK, func(auth string) string { return strings.ReplaceAll(c.body, "{auth}", auth) })
			var into renewed
			err := answer.Decode(&into)
			if err == nil {
				t.Fatal("Decode took an answer that does not decode")
			}
			redacttest.RequireNone(t, "redact.answer-decode/"+c.name, fmt.Sprintf("%v | %+v | %#v", err, err, err))
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Decode's error is %q, want it to name %s", err, c.names)
			}
			if errors.Unwrap(err) != nil {
				t.Errorf("Decode's error unwraps to %v, whose text quotes the value", errors.Unwrap(err))
			}
		})
	}
}

// TestAnAnswersDecodeErrorNamesABoxMemberAtAnyDepth: a box's own type error, for a member that
// holds no JSON string, names no field, and encoding/json adds none, so Decode finds the member
// that did not decode itself (failingMember). A box in a list's item or as the item is named
// through the list as "(item)", in a map's value or as the value through the map as "(key)", as
// member names those segments, whether the list or the map is a member or the destination itself;
// a member the answer spells in another case, or one promoted from an embedded struct, by the
// destination's own name for it; and each with the JSON kind the box's error names. The answer
// echoes the request's bearer as a map key and as a value that decodes, and the error quotes
// neither. Captures redact.answer-decode-box/<case>.
func TestAnAnswersDecodeErrorNamesABoxMemberAtAnyDepth(t *testing.T) {
	type boxed struct {
		Code redact.RemoteText `json:"code"`
	}
	type Inner struct {
		Reason redact.RemoteText `json:"reason"`
	}
	type holder struct {
		Inner
		Code      redact.RemoteText            `json:"code"`
		Boxes     []boxed                      `json:"boxes"`
		BoxByKey  map[string]boxed             `json:"box_by_key"`
		Codes     []redact.RemoteText          `json:"codes"`
		CodeByKey map[string]redact.RemoteText `json:"code_by_key"`
	}
	for _, c := range []struct {
		name, body, names string
		into              func() any
	}{
		{"list-item-box", `{"boxes":[{"code":"{auth}"},{"code":7}]}`, `"boxes.(item).code"`, func() any { return new(holder) }},
		{"map-value-box", `{"box_by_key":{"{auth}":{"code":7}}}`, `"box_by_key.(key).code"`, func() any { return new(holder) }},
		{"list-of-boxes", `{"codes":["{auth}",7]}`, `"codes.(item)"`, func() any { return new(holder) }},
		{"map-of-boxes", `{"code_by_key":{"{auth}":7}}`, `"code_by_key.(key)"`, func() any { return new(holder) }},
		{"list-destination", `[{"code":"{auth}"},{"code":7}]`, `"(item).code"`, func() any { return new([]boxed) }},
		{"map-destination", `{"{auth}":{"code":7}}`, `"(key).code"`, func() any { return new(map[string]boxed) }},
		{"name-case", `{"CODE":7}`, `"code"`, func() any { return new(holder) }},
		{"promoted", `{"reason":7}`, `"reason"`, func() any { return new(holder) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			answer := answerOf(t, http.StatusOK, func(auth string) string { return strings.ReplaceAll(c.body, "{auth}", auth) })
			err := answer.Decode(c.into())
			if err == nil {
				t.Fatal("Decode took an answer that does not decode")
			}
			redacttest.RequireNone(t, "redact.answer-decode-box/"+c.name, fmt.Sprintf("%v | %+v | %#v", err, err, err))
			if !strings.Contains(err.Error(), "the answer's "+c.names+" is a JSON number ") {
				t.Errorf("Decode's error is %q, want it to name %s and the JSON number it held", err, c.names)
			}
			if errors.Unwrap(err) != nil {
				t.Errorf("Decode's error unwraps to %v, whose text quotes the value", errors.Unwrap(err))
			}
		})
	}
}

// TestAnAnswersWordsAndDecodedBoxesHoldItsRecord: a refusal that echoes the request's bearer. Its
// Words, and every RemoteText Decode fills - a member, one in a slice, one in a map, one behind a
// pointer - print it redacted, and compare as they came. Captures redact.answer-words and
// redact.answer-decoded/<member>.
func TestAnAnswersWordsAndDecodedBoxesHoldItsRecord(t *testing.T) {
	answer := answerOf(t, http.StatusBadGateway, func(auth string) string {
		return fmt.Sprintf(`{"error":"upstream refused %[1]s","list":["refused %[1]s"],"byKey":{"a":"refused %[1]s"},"ptr":"refused %[1]s"}`, auth)
	})
	if answer.Status() != http.StatusBadGateway {
		t.Errorf("Status = %d, want 502", answer.Status())
	}
	redacttest.RequireNone(t, "redact.answer-words", answer.Words().String())
	var envelope struct {
		Error redact.RemoteText            `json:"error"`
		List  []redact.RemoteText          `json:"list"`
		ByKey map[string]redact.RemoteText `json:"byKey"`
		Ptr   *redact.RemoteText           `json:"ptr"`
	}
	if err := answer.Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Ptr == nil || len(envelope.List) != 1 || len(envelope.ByKey) != 1 {
		t.Fatalf("Decode filled %+v, want every member", envelope)
	}
	for member, box := range map[string]redact.RemoteText{"error": envelope.Error, "list": envelope.List[0], "byKey": envelope.ByKey["a"], "ptr": *envelope.Ptr} {
		redacttest.RequireNone(t, "redact.answer-decoded/"+member, box.String())
	}
	if !envelope.Error.Equal("upstream refused Bearer " + redacttest.Token) {
		t.Error("the decoded error does not compare as it came")
	}
}

// TestAnAnswerRefusesADestinationThatHoldsItsBodyWhole: a string, a byte slice, a json.RawMessage
// and an interface value would each hold the body as plain text, and Decode fills none of them.
// encoding/json fills a byte slice only from a base64 string, so the byte slice's answer is the
// bearer in base64, which encoding/json decodes: only Decode's own refusal keeps it out.
func TestAnAnswerRefusesADestinationThatHoldsItsBodyWhole(t *testing.T) {
	quoted := answerOf(t, http.StatusOK, func(auth string) string { return `"` + auth + `"` })
	encodedBody := func(auth string) string { return `"` + base64.StdEncoding.EncodeToString([]byte(auth)) + `"` }
	var control []byte
	if err := json.Unmarshal([]byte(encodedBody("Bearer "+redacttest.Token)), &control); err != nil {
		t.Fatalf("encoding/json does not fill a byte slice from the byte slice's answer: %v", err)
	}
	encoded := answerOf(t, http.StatusOK, encodedBody)
	var s string
	var b []byte
	var raw json.RawMessage
	var a any
	for name, c := range map[string]struct {
		answer redact.Answer
		into   any
	}{"string": {quoted, &s}, "bytes": {encoded, &b}, "raw": {quoted, &raw}, "any": {quoted, &a}} {
		if err := c.answer.Decode(c.into); err == nil {
			t.Errorf("Decode filled a %s with the body whole", name)
		}
	}
	redacttest.RequireNone(t, "redact.answer-whole", fmt.Sprint(s, string(b), string(raw), a))
}

// TestAnAnswerPrintsNothingOfItsBody: an Answer printed by every fmt verb, by encoding/json and by
// slog prints nothing of the body it holds, nor of the credentials its request carried, which
// Words has read into it: not as text, and not as the bytes fmt prints a byte slice as. A verb fmt
// cannot apply to a pointer (%s, %q) prints what the pointer points at. Captures
// redact.answer-printed.
func TestAnAnswerPrintsNothingOfItsBody(t *testing.T) {
	answer := answerOf(t, http.StatusOK, func(string) string { return redacttest.Words })
	_ = answer.Words()
	var out bytes.Buffer
	for _, verb := range []string{"%s", "%q", "%v", "%+v", "%#v", "%x", "%X"} {
		fmt.Fprintf(&out, verb+"\n", answer)
	}
	if data, err := json.Marshal(struct{ A redact.Answer }{answer}); err == nil {
		out.Write(data)
	}
	slog.New(slog.NewTextHandler(&out, nil)).Info("answer", "answer", answer)
	slog.New(slog.NewJSONHandler(&out, nil)).Info("answer", "answer", answer)
	redacttest.RequireNone(t, "redact.answer-printed", out.String())
	for _, secret := range redacttest.Secrets {
		if asBytes := strings.Trim(fmt.Sprint([]byte(secret)), "[]"); strings.Contains(out.String(), asBytes) {
			t.Errorf("REDACT-ASSERT redact.answer-printed: the output prints the secret %q as its bytes: %q", secret, out.String())
		}
	}
}

// TestReadAnswerCutsABodyAtItsLimit: a body longer than the limit is the Answer of its first limit
// bytes and an error that wraps ErrAnswerTooLarge; StreamTo writes those bytes, and a body within
// the limit whole.
func TestReadAnswerCutsABodyAtItsLimit(t *testing.T) {
	for _, c := range []struct {
		body  string
		limit int64
		want  string
		large bool
	}{{"0123456789", 4, "0123", true}, {"0123456789", 10, "0123456789", false}} {
		resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(c.body))}
		answer, err := redact.ReadAnswer(resp, c.limit)
		if errors.Is(err, redact.ErrAnswerTooLarge) != c.large || (!c.large && err != nil) {
			t.Errorf("ReadAnswer of %d bytes within %d: %v", len(c.body), c.limit, err)
		}
		var got bytes.Buffer
		if _, err := answer.StreamTo(&got); err != nil || got.String() != c.want {
			t.Errorf("StreamTo wrote %q, %v; want %q", got.String(), err, c.want)
		}
	}
}

// TestStreamAnswerStopsAtItsLimit: a body within the limit streams whole; one past it streams its
// first limit bytes and then fails with ErrAnswerTooLarge; closing the stream closes the body.
func TestStreamAnswerStopsAtItsLimit(t *testing.T) {
	for _, c := range []struct {
		body  string
		limit int64
		want  string
		large bool
	}{{"0123456789", 4, "0123", true}, {"0123456789", 10, "0123456789", false}} {
		closed := false
		body := closer{Reader: strings.NewReader(c.body), closed: &closed}
		stream := redact.StreamAnswer(&http.Response{StatusCode: http.StatusOK, Body: body}, c.limit)
		got, err := io.ReadAll(stream)
		if string(got) != c.want || errors.Is(err, redact.ErrAnswerTooLarge) != c.large || (!c.large && err != nil) {
			t.Errorf("StreamAnswer of %d bytes within %d read %q, %v", len(c.body), c.limit, got, err)
		}
		if err := stream.Close(); err != nil || !closed {
			t.Errorf("Close = %v, closed the body %t", err, closed)
		}
	}
}

type closer struct {
	io.Reader
	closed *bool
}

func (c closer) Close() error { *c.closed = true; return nil }
