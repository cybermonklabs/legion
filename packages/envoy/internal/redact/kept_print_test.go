package redact_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// keptCases are the credentials a request carries in each way a client of this repository sends
// one - a bearer, a Basic pair and a URL's user information, which net/http's client sends as a
// Basic pair - and echo, what keptAnswer's stand-in answers to it.
var keptCases = []struct {
	name    string
	request func(base string) (*http.Request, error)
	echo    string
}{
	{"bearer", func(base string) (*http.Request, error) {
		r, err := http.NewRequest(http.MethodGet, base+"/v1/x", nil)
		if err == nil {
			r.Header.Set("Authorization", "Bearer "+redacttest.Token)
		}
		return r, err
	}, "refused Bearer " + redacttest.Token},
	{"basic", func(base string) (*http.Request, error) {
		r, err := http.NewRequest(http.MethodGet, base+"/v1/x", nil)
		if err == nil {
			r.SetBasicAuth(redacttest.User, redacttest.Password)
		}
		return r, err
	}, "refused " + redacttest.User + ":" + redacttest.Password},
	{"url-user", func(base string) (*http.Request, error) {
		target, err := url.Parse(base + "/v1/x")
		if err != nil {
			return nil, err
		}
		target.User = url.UserPassword(redacttest.User, redacttest.Password)
		return http.NewRequest(http.MethodGet, target.String(), nil)
	}, "refused " + redacttest.User + ":" + redacttest.Password},
}

// keptAnswer is the Answer of the request request makes, sent with net/http's own client to a
// stand-in that refuses it, 401, echoing the credential it was sent, as whatever answers in a
// server's place can: a Basic pair as the user and password it decodes, any other Authorization as
// it came. The answer's Words are read, so the answer keeps its request's credentials
// (Answer.Words reads them once) as every print of its words leaves it.
func keptAnswer(t *testing.T, request func(base string) (*http.Request, error), echo string) redact.Answer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		if user, password, ok := r.BasicAuth(); ok {
			_, _ = io.WriteString(w, "refused "+user+":"+password)
			return
		}
		_, _ = io.WriteString(w, "refused "+r.Header.Get("Authorization"))
	}))
	t.Cleanup(srv.Close)
	req, err := request(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	answer, err := redact.ReadAnswer(resp, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !answer.Words().Equal(echo) {
		t.Fatalf("the stand-in did not echo the credential it was sent: %v", answer.Words())
	}
	return answer
}

// TestAKeptCredentialPrintsUnderNoVerbAtAnyHolding: a box keeps the credentials of the request
// its answer answers (RemoteText.WithoutTokens, which Answer.Words and Decode call), an Answer
// keeps them once its words are read, and a request's record keeps the request itself, whose
// headers carry them, on a hop's error and in a call's scope: real credentials - a bearer, a Basic
// pair, a URL's user information - that no printer may reach. Each is printed under every fmt verb
// and a bad verb, as a value, behind a pointer, in a slice and as a map's value, each inside a
// struct's unexported field too (redacttest.KeptPrintTest). fmt reads an unexported field's
// storage, and under a verb a pointer does not take it spells out at the top level what the
// pointer points at: a box that kept them behind a pointer to a slice printed them there. The box
// keeps them behind a func, which fmt prints as an address; an Answer, a hop's error and a scope
// keep them two pointers below themselves, and fmt spells out only the first. Captures
// kept/<value>/<holding>/<verb>.
func TestAKeptCredentialPrintsUnderNoVerbAtAnyHolding(t *testing.T) {
	echo := "refused the token " + redacttest.Token
	t.Run("box/without-tokens", func(t *testing.T) {
		redacttest.KeptPrintTest(t, "box/without-tokens", redact.NewRemoteText(echo).WithoutTokens(redacttest.Token), echo)
	})
	for _, c := range keptCases {
		answer := keptAnswer(t, c.request, c.echo)
		t.Run("box/"+c.name, func(t *testing.T) { redacttest.KeptPrintTest(t, "box/"+c.name, answer.Words(), c.echo) })
		t.Run("answer/"+c.name, func(t *testing.T) { redacttest.KeptPrintTest(t, "answer/"+c.name, answer, c.echo) })
	}
	bearer := keptCases[0]
	t.Run("record/hop-error", func(t *testing.T) {
		closed := httptest.NewServer(http.NotFoundHandler())
		closed.Close()
		req, err := bearer.request(closed.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := redact.RefusalClient(nil).Do(req)
		if err == nil {
			resp.Body.Close()
			t.Fatal("a closed server answered")
		}
		redacttest.KeptPrintTest(t, "record/hop-error", err, bearer.echo)
	})
	t.Run("record/scope", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "refused "+r.Header.Get("Authorization"))
		}))
		t.Cleanup(srv.Close)
		scope := redact.Scope[struct{}](t.Context())
		req, err := bearer.request(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := scope.Client(nil).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		redacttest.KeptPrintTest(t, "record/scope", scope, bearer.echo)
	})
}
