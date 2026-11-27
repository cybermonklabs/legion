package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/redact"
)

// api is a Dispatch HTTP client: agent requests use the stack's shared bearer and name a session
// actor in a write's body; a person's request names the person in the trusted identity header
// (DISPATCH_IDENTITY=header:X-Dispatch-User).
type api struct {
	base   string
	token  string
	client *http.Client
}

func newAPI(base, token string, maxIdle int) *api {
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConns:        maxIdle,
		MaxIdleConnsPerHost: maxIdle,
		IdleConnTimeout:     90 * time.Second,
	}
	return &api{base: strings.TrimRight(base, "/"), token: token, client: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

// who is a request's identity: a person through the identity header when person is set, the
// agent bearer otherwise.
type who struct {
	person string
}

// callResult is one request's outcome. Times are wall-clock Unix nanoseconds, so they compare
// with receipt times and Postgres commit timestamps on the same box.
type callResult struct {
	status int
	// answer is the answer as redact.ReadAnswer read it: a message prints its words through the
	// rule (text), and a decode reads it (Decode), whose error quotes no value. body is its bytes,
	// the data a run measures, hashes and keeps (redact.Answer.StreamTo), which no message prints.
	answer redact.Answer
	body   []byte
	// etag is the answer's ETag header, empty when it carries none.
	etag string
	// replayed is whether the answer carried contracts.IdempotencyReplayedHeader: Dispatch
	// answered it from the first request under its key rather than running it again.
	replayed bool
	// start is when the client began the request; wrote is when the whole request was in the
	// socket (httptrace WroteRequest), 0 when it never got there; end is when the response body
	// was read.
	start, wrote, end int64
	// connWait is the client's own wait for a connection (httptrace GetConn to GotConn).
	connWait time.Duration
	err      error
}

// answerLimit bounds one answer the load test reads: a seeded page is far smaller.
const answerLimit = 64 << 20

func (r callResult) ok() bool { return r.err == nil && r.status/100 == 2 }

// text is the answer's words as a message prints them (answerWords).
func (r callResult) text() string { return answerWords(r.answer) }

// answerWords is answer's words as a message prints them: the rule's reading, with every
// credential the request carried redacted (redact.Answer.Words), cut to 300 bytes (trunc).
func answerWords(answer redact.Answer) string { return trunc([]byte(answer.Words().String())) }

// errorCode is the code a non-2xx answer's body names (writeError writes {"error": ..., "code":
// ...}); empty for a 2xx answer, a transport error, and a body that names none or is not JSON at
// all. It is Dispatch's word, held as redact.RemoteText with every credential the request carried
// redacted (redact.Answer.Decode), since whatever answers in Dispatch's place can echo the bearer
// back: a caller compares it through the box and prints it as it prints itself.
func (r callResult) errorCode() redact.RemoteText {
	if r.err != nil || r.status/100 == 2 {
		return redact.RemoteText{}
	}
	return r.bodyCode()
}

// bodyCode is the code the answer's body names, whatever its status, held as errorCode holds it
// (redact.Answer.Decode); empty for a body that names none or is not JSON at all.
func (r callResult) bodyCode() redact.RemoteText {
	var answered struct {
		Code redact.RemoteText `json:"code"`
	}
	_ = r.answer.Decode(&answered)
	return answered.Code
}

// landed says the keyed write this answers ran and Dispatch gives no copy of its answer: a 409
// KEY_COMMITTED (it landed before its answer was lost) or a replayed 2xx whose body reads
// ANSWER_NOT_KEPT (contracts.IdempotencyKeyCommitted, IdempotencyAnswerNotKept). What it made is
// there, and is read back for anything its caller needs of it; it is no refusal.
func (r callResult) landed() bool {
	if r.err != nil {
		return false
	}
	if r.status == http.StatusConflict {
		return r.bodyCode().Equal(contracts.IdempotencyKeyCommitted)
	}
	return r.replayed && r.status/100 == 2 && r.bodyCode().Equal(contracts.IdempotencyAnswerNotKept)
}

// transient is a failure worth retrying: a transport error, 429 or 5xx.
func (r callResult) transient() bool {
	return r.err != nil || r.status == http.StatusTooManyRequests || r.status >= 500
}

// arrival is when the request reached Dispatch's socket, or its start when the trace never saw
// the write complete.
func (r callResult) arrival() int64 {
	if r.wrote != 0 {
		return r.wrote
	}
	return r.start
}

// call sends one request with a JSON body (none when body is nil) and reads the whole response.
func (a *api) call(ctx context.Context, method, path string, w who, body any) callResult {
	return a.callKeyed(ctx, method, path, w, body, "")
}

// callKeyed is call under an idempotency key (contracts.IdempotencyKeyHeader): Dispatch runs the
// write of a key once and replays its stored answer to every later request under that key, so a
// retry of a write whose answer was lost adds no second version. An empty key sends none, which
// is what every unkeyed caller does.
func (a *api) callKeyed(ctx context.Context, method, path string, w who, body any, key string) callResult {
	if body == nil {
		return a.callRawKeyed(ctx, method, path, w, "", nil, key)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return callResult{err: err}
	}
	return a.callRawKeyed(ctx, method, path, w, "application/json", payload, key)
}

// callRaw sends one request whose body is payload under contentType (a multipart upload, say);
// a nil payload sends no body and no Content-Type.
func (a *api) callRaw(ctx context.Context, method, path string, w who, contentType string, payload []byte) callResult {
	return a.callRawKeyed(ctx, method, path, w, contentType, payload, "")
}

// callRawKeyed is callRaw under an idempotency key; an empty key sends none.
func (a *api) callRawKeyed(ctx context.Context, method, path string, w who, contentType string, payload []byte, key string) callResult {
	var getConn, gotConn, wrote atomic.Int64
	trace := &httptrace.ClientTrace{
		GetConn: func(string) { getConn.Store(time.Now().UnixNano()) },
		GotConn: func(httptrace.GotConnInfo) { gotConn.Store(time.Now().UnixNano()) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(time.Now().UnixNano())
			}
		},
	}
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), method, a.base+path, rdr)
	if err != nil {
		return callResult{err: err}
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if w.person != "" {
		req.Header.Set("X-Dispatch-User", w.person)
	} else {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	if key != "" {
		req.Header.Set(contracts.IdempotencyKeyHeader, key)
	}
	res := callResult{start: time.Now().UnixNano()}
	resp, err := a.client.Do(req)
	if err == nil {
		res.status = resp.StatusCode
		res.etag = resp.Header.Get("ETag")
		res.replayed = resp.Header.Get(contracts.IdempotencyReplayedHeader) == "?1"
		res.answer, err = redact.ReadAnswer(resp, answerLimit)
		_ = resp.Body.Close()
		var body bytes.Buffer
		_, _ = res.answer.StreamTo(&body)
		res.body = body.Bytes()
	}
	res.end = time.Now().UnixNano()
	res.err = err
	res.wrote = wrote.Load()
	if g, h := getConn.Load(), gotConn.Load(); g != 0 && h >= g {
		res.connWait = time.Duration(h - g)
	}
	return res
}

// callRetry retries a transient failure (a transport error, 429 or 5xx) once a second, up to six
// times, as the seed always has; any other answer returns at once.
func (a *api) callRetry(ctx context.Context, method, path string, w who, body any) callResult {
	return a.callRetryKeyed(ctx, method, path, w, body, "")
}

// callRetryKeyed is callRetry under an idempotency key. A repeat that reaches Dispatch while the
// key's first request still runs is 409 KEY_IN_FLIGHT, which this retries too: under a key that
// answer says the write is running once, not that it must be sent again.
func (a *api) callRetryKeyed(ctx context.Context, method, path string, w who, body any, key string) callResult {
	for attempt := 0; ; attempt++ {
		res := a.callKeyed(ctx, method, path, w, body, key)
		transient := res.transient() ||
			(key != "" && res.status == http.StatusConflict && res.errorCode().Equal(contracts.IdempotencyKeyInFlight))
		if res.ok() || !transient || attempt >= 6 || ctx.Err() != nil {
			return res
		}
		time.Sleep(time.Second)
	}
}

// sessionActor is a write's actor body field for a session id, the form a request body and
// multipartFile's form both take.
func sessionActor(id string) map[string]any { return map[string]any{"kind": "session", "id": id} }

// seedPerson is the person a seed creates its projects as, and so the person each seeded project
// names as its default assignee: who an issue of that project nobody holds, and a question on
// one, reaches. Every project names a person (internal/dispatch/store/migrations/
// 20261111210103_projects_default_assignee_not_null.up.sql), and the create takes the login the
// call names, else the person creating it, else the person its credential acts for
// (internal/dispatch/api/projects.go). A seed writes as a session under the stack's shared
// bearer, which acts for nobody, so its project create carries the identity header instead: that
// person is the second step, and the header records them as somebody who has signed in
// (internal/dispatch/identity.HeaderIdentity.Login), which a login named in the call would have
// to be already (allowedLogin).
const seedPerson = "person-seed"

func trunc(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}
