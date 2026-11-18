package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// echoAgentBearer is the stack's agent bearer the rows' requests carry: long enough that excerpt
// redacts every run of eight of its characters, and holding no word, so no piece of it can turn up
// in a message's own prose.
const echoAgentBearer = "ltab-7f3c9a1e5b2d40861c"

// proxyEcho is a gateway standing in a server's place, as a misrouted proxy or a captive portal
// can: it answers the requests echo picks with a page of its own that echoes the request back
// (echoPage), at status, and every other request with what next writes (the server's own answer).
// It keeps every credential the requests it echoed carried, so a row knows what must not be
// printed even where it is minted per request.
type proxyEcho struct {
	srv    *httptest.Server
	echo   func(r *http.Request) bool
	status int
	next   http.HandlerFunc

	mu   sync.Mutex
	seen []string
}

func newProxyEcho(t *testing.T, status int, echo func(r *http.Request) bool, next http.HandlerFunc) *proxyEcho {
	t.Helper()
	p := &proxyEcho{echo: echo, status: status, next: next}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *proxyEcho) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if p.echo != nil && !p.echo(r) {
		r.Body = io.NopCloser(bytes.NewReader(body))
		p.next(w, r)
		return
	}
	carried := carriedBy(r)
	p.mu.Lock()
	for _, c := range carried {
		if !slices.Contains(p.seen, c) {
			p.seen = append(p.seen, c)
		}
	}
	p.mu.Unlock()
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(p.status)
	_, _ = io.WriteString(w, echoPage(r, body, carried))
}

// credentials is every credential the requests the gateway echoed carried.
func (p *proxyEcho) credentials() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.seen)
}

// carriedBy is every credential r carries: its Authorization's value after the scheme (a Basic
// one's encoding, user name and password each), and a password query parameter.
func carriedBy(r *http.Request) []string {
	var carried []string
	if scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok {
		carried = append(carried, value)
		if strings.EqualFold(scheme, "basic") {
			if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
				user, password, _ := strings.Cut(string(decoded), ":")
				carried = append(carried, user, password)
			}
		}
	}
	if password := r.URL.Query().Get("password"); password != "" {
		carried = append(carried, password)
	}
	return carried
}

// echoPage is the gateway's page for r: each credential r carried bare on a line of its own first,
// so excerpt's Authorization and Bearer patterns cannot hide it and only the redaction of the
// tokens a message is handed can, then r's request line, headers and body as they came.
func echoPage(r *http.Request, body []byte, carried []string) string {
	var page strings.Builder
	page.WriteString("<!DOCTYPE html>\n<html><head><title>Gateway</title><style>p{margin:0}</style></head><body>\n<h1>Gateway</h1>\n")
	for _, c := range carried {
		page.WriteString("<p>\n" + c + "\n</p>\n")
	}
	fmt.Fprintf(&page, "<pre>\n%s %s %s\n", r.Method, r.URL.RequestURI(), r.Proto)
	for _, name := range slices.Sorted(maps.Keys(r.Header)) {
		for _, value := range r.Header[name] {
			fmt.Fprintf(&page, "%s: %s\n", name, value)
		}
	}
	page.WriteString("\n" + string(body) + "\n</pre>\n</body></html>\n")
	return page.String()
}

// leaked is the first piece of credential that out holds: a run of eight of its characters, the
// shortest run excerpt redacts wherever it stands, or credential whole where it is shorter; ""
// where out holds none.
func leaked(out, credential string) string {
	runes := []rune(credential)
	length := min(8, len(runes))
	for at := 0; length > 0 && at+length <= len(runes); at++ {
		if piece := string(runes[at : at+length]); strings.Contains(out, piece) {
			return piece
		}
	}
	return ""
}

// requireRedacted fails t unless out, what a message printed after the gateway's page, holds no
// piece of any of credentials and names the page in excerpt's form, form.
func requireRedacted(t *testing.T, what, out string, credentials []string, form string) {
	t.Helper()
	if len(credentials) == 0 {
		t.Fatalf("%s: the gateway echoed no request carrying a credential, so nothing was checked:\n%s", what, out)
	}
	for _, c := range credentials {
		if piece := leaked(out, c); piece != "" {
			t.Errorf("%s printed %q, a piece of a credential its request carried:\n%s", what, piece, out)
		}
	}
	if !strings.Contains(out, form) {
		t.Errorf("%s does not name the gateway's page as %q:\n%s", what, form, out)
	}
	t.Logf("%s printed:\n%s", what, out)
}

// dispatchStub answers a seed's and a loop writer's requests as Dispatch would, enough for each to
// reach the request the gateway answers instead.
func dispatchStub(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/api/v1/projects":
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	case r.Method == http.MethodPost && path == "/api/v1/issues":
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"key":"LGA-1","primary_artifact_id":"00000000-0000-4000-8000-000000000001"}`)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/comments"):
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"00000000-0000-4000-8000-000000000002"}`)
	default:
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}
}

// TestProxyEchoLoopWriterAnswers: every line the loop writer logs about an answer of Dispatch's
// names a gateway's page in one line with the agent bearer its request carried redacted: a write
// (and the review page a verdict reads), a requirement's part document, and a status thread's
// resolve. The gateway answers 403, which no retry repeats.
func TestProxyEchoLoopWriterAnswers(t *testing.T) {
	ctx := context.Background()
	is := func(method, suffix string) func(r *http.Request) bool {
		return func(r *http.Request) bool { return r.Method == method && strings.HasSuffix(r.URL.Path, suffix) }
	}
	every := func(*http.Request) bool { return true }
	const refused = `a body that is not Dispatch's error JSON (`
	for _, row := range []struct {
		name  string
		echo  func(r *http.Request) bool
		drive func(w *writer)
	}{
		{"events.go verdict and its page read", every, func(w *writer) {
			w.do(ctx, eventPlan{marker: 3, kind: kindVerdict, issue: 0, page: 1})
		}},
		{"events.go requirement's part document", is(http.MethodPost, "/artifacts"), func(w *writer) {
			for i := range 2 {
				w.do(ctx, eventPlan{marker: int64(i + 1), kind: kindRequirement, issue: 1, body: 300})
			}
		}},
		{"events.go status resolve", is(http.MethodPost, "/resolve"), func(w *writer) {
			w.do(ctx, eventPlan{marker: 5, kind: kindStatus, issue: 0, replyTo: "open-a1", body: 124})
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			gateway := newProxyEcho(t, http.StatusForbidden, row.echo, dispatchStub)
			var out bytes.Buffer
			ls := loopTestSeed(t)
			w := newWriter(newAPI(gateway.srv.URL, echoAgentBearer, 4), ls.seedFile(), ls, 42, log.New(&out, "", 0))
			row.drive(w)
			requireRedacted(t, row.name, out.String(), gateway.credentials(), refused)
		})
	}
}

// TestBodyTextKeepsDispatchsOwnRefusal: Dispatch's own error JSON is named as it always was, its
// first 300 bytes after the status, and a request that got no answer names no body.
func TestBodyTextKeepsDispatchsOwnRefusal(t *testing.T) {
	const own = `{"error":"issue LGA-7 has no references document","code":"NOT_FOUND"}`
	a := newAPI("http://127.0.0.1:1", echoAgentBearer, 1)
	if got := a.bodyText(callResult{status: http.StatusNotFound, body: []byte(own)}); got != own {
		t.Errorf("bodyText of Dispatch's own refusal = %q, want it whole", got)
	}
	if got := a.bodyText(callResult{body: []byte("unread")}); got != "" {
		t.Errorf("bodyText of a request that got no answer = %q, want nothing", got)
	}
}
