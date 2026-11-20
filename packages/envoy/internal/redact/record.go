package redact

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// RefusalClient is base (nil is http.DefaultClient), its timeout, redirect rule and cookie jar as
// they are, whose transport keeps a record of every request it sends (requestTokens reads each
// one's credentials when an error is read: every value under a name no wire contract declares
// public, of a header, the URL's user information and query, and a form or JSON body) and changes
// nothing of any answer, so the library that sent the request parses the answer as it came. The
// record travels with what the request led to, and nothing matches it by text:
//
//   - an answer carries it on its Request's context (resp.Request), so the boundary reads an
//     *oauth2.RetrieveError x/oauth2 makes of it with the record of its own requests, and a client
//     of this repository that reads a refused answer itself takes it off the answer (Carried);
//   - a hop that fails carries it on its error (hopError), which the *url.Error net/http makes of it
//     unwraps to;
//   - a scope (Scope) the request's context carries, or the one a client CallScope.Client made is
//     bound to, holds it too, for a library that hands back only the text of its error.
//
// Every hop is recorded: each redirect's request, the Location it was sent to among it (record),
// whatever its answer's status and however empty its body, since a status line can echo a request
// as well as a body can. A RefusalClient above another one (with a transport between that sets a
// credential, as oauth2.NewClient's does) joins the record the one beneath made, so one answer has
// one record of every request each saw. It is the one wrapper on every HTTP client this
// repository builds or hands a library; the census in cmd/redact-gate fails on one without it.
func RefusalClient(base *http.Client) *http.Client { return refusalClient(base, nil) }

// refusalClient is RefusalClient with each record also written into bound (nil for none).
func refusalClient(base *http.Client, bound *scope) *http.Client {
	client := new(http.Client)
	if base != nil {
		*client = *base
	}
	client.Transport = refusalTransport{base: client.Transport, bound: bound}
	return client
}

// refusalMade reports whether client is one RefusalClient or CallScope.Client made.
func refusalMade(client *http.Client) bool {
	if client == nil {
		return false
	}
	_, ok := client.Transport.(refusalTransport)
	return ok
}

// refusalTransport sends each request through base (nil is http.DefaultTransport) and records it
// (RefusalClient); bound is the scope a client CallScope.Client made writes each record into.
type refusalTransport struct {
	base  http.RoundTripper
	bound *scope
}

func (t refusalTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(r)
	if err != nil {
		// A RefusalClient beneath this one failed the same hop and recorded it: this request joins
		// that record.
		if inner, ok := err.(*hopError); ok {
			inner.rec.add(r)
			t.note(r, inner.rec)
			return nil, err
		}
		rec := newRecord(r)
		t.note(r, rec)
		return nil, &hopError{err: err, rec: rec}
	}
	if response == nil {
		return nil, nil
	}
	if inner := recorded(response); inner != nil && inner != recordIn(r.Context()) {
		inner.add(r)
		t.note(r, inner)
		return response, nil
	}
	rec := newRecord(r)
	// The request the transport beneath sent, with whatever a transport between set on it (a
	// bearer, a proof).
	rec.add(response.Request)
	t.note(r, rec)
	response.Request = r.WithContext(context.WithValue(r.Context(), recordKey{}, rec))
	return response, nil
}

// note writes rec into the scope r's context carries and into t's bound scope.
func (t refusalTransport) note(r *http.Request, rec *record) {
	if t.bound != nil {
		t.bound.add(rec)
	}
	if s := scopeIn(r.Context()); s != nil && s != t.bound {
		s.add(rec)
	}
}

// record is what RefusalClient keeps of one answer: each request that led to it, as each
// RefusalClient it passed through and the transport beneath them saw it, and the record of the
// redirect that sent it (prev). Its credentials are read only when an error is (tokens).
type record struct {
	mu       sync.Mutex
	requests []*http.Request
	prev     *record
}

// recordKey is the context key an answer's Request carries its record under.
type recordKey struct{}

// newRecord is the record of r: r, and the record of the redirect answer r was sent for.
func newRecord(r *http.Request) *record {
	return &record{requests: []*http.Request{r}, prev: answerRecord(r.Response)}
}

// add joins r, another request of the same answer, to rec.
func (rec *record) add(r *http.Request) {
	if r == nil {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !slices.Contains(rec.requests, r) {
		rec.requests = append(rec.requests, r)
	}
}

// tokens is every credential the answer's requests carried, for Tokens: each request's
// (requestTokens), and for each one a redirect sent, each segment of its path, as sent and
// unescaped, since the Location the answering party wrote can put a value of an earlier request in
// it; then the same of the redirect before it.
func (rec *record) tokens() []string {
	var tokens []string
	for at := rec; at != nil; at = at.prev {
		at.mu.Lock()
		requests := slices.Clone(at.requests)
		at.mu.Unlock()
		for _, r := range requests {
			tokens = append(tokens, requestTokens(r)...)
			if r.Response != nil && r.URL != nil {
				tokens = append(tokens, pathSegments(r.URL)...)
			}
		}
	}
	return tokens
}

// pathSegments is each segment of u's path, as sent and unescaped where that differs.
func pathSegments(u *url.URL) []string {
	var segments []string
	for _, segment := range strings.Split(u.EscapedPath(), "/") {
		if segment == "" {
			continue
		}
		segments = append(segments, segment)
		if unescaped, err := url.PathUnescape(segment); err == nil && unescaped != segment {
			segments = append(segments, unescaped)
		}
	}
	return segments
}

// recordIn is the record ctx carries, or nil.
func recordIn(ctx context.Context) *record {
	rec, _ := ctx.Value(recordKey{}).(*record)
	return rec
}

// recorded is the record a RefusalClient carried on resp, or nil.
func recorded(resp *http.Response) *record {
	if resp == nil || resp.Request == nil {
		return nil
	}
	return recordIn(resp.Request.Context())
}

// answerRecord is the record of the answer resp: the one a RefusalClient carried on it, else one
// of the requests net/http left on it, its Request and the redirect answer each was sent for
// (Request.Response); nil where resp holds no request.
func answerRecord(resp *http.Response) *record {
	if resp == nil || resp.Request == nil {
		return nil
	}
	if rec := recordIn(resp.Request.Context()); rec != nil {
		return rec
	}
	return &record{requests: []*http.Request{resp.Request}, prev: answerRecord(resp.Request.Response)}
}

// hopError is a hop's failure as a RefusalClient hands it on: the transport's error, which it
// prints and unwraps to, and the record of the requests that led to it, which the boundary reads
// it with. Timeout and Temporary answer as the error does, which *url.Error asks of the error it
// holds directly.
type hopError struct {
	err error
	rec *record
}

func (e *hopError) Error() string { return e.err.Error() }

func (e *hopError) Unwrap() error { return e.err }

func (e *hopError) Timeout() bool {
	t, ok := e.err.(interface{ Timeout() bool })
	return ok && t.Timeout()
}

func (e *hopError) Temporary() bool {
	t, ok := e.err.(interface{ Temporary() bool })
	return ok && t.Temporary()
}

// Carried is every credential the requests that led to resp carried (record.tokens), as the
// record a RefusalClient carried on it says, or as the requests net/http left on it say where none
// did: for a client of this repository that reads a refused answer itself, which holds the
// answer's words with them redacted (RemoteText.WithoutTokens) before it prints them.
func Carried(resp *http.Response) []string {
	if rec := answerRecord(resp); rec != nil {
		return rec.tokens()
	}
	return nil
}
