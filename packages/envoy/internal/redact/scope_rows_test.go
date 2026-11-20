package redact_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// scopeRoundTripper answers every request with a status line and a body the server controls.
type scopeRoundTripper struct {
	status int
	reason func(*http.Request) string
	body   func(*http.Request) string
}

func (t scopeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: t.status,
		Status:     fmt.Sprintf("%d %s", t.status, t.reason(r)),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": {"text/plain"}},
		Body:       io.NopCloser(strings.NewReader(t.body(r))),
		Request:    r,
	}, nil
}

func scopedDiscovery(ctx context.Context, client *http.Client) (*gooidc.Provider, error) {
	s := redact.Scope[*gooidc.Provider](ctx)
	return redact.OAuth2(s.Read(gooidc.NewProvider(gooidc.ClientContext(s.Context(), client), "https://idp.example")))
}

// TestScopedLibraryErrorsKeepTheirOwnRecord proves that records travel with the answer, never by
// matching its words. Two separate refusals share a gateway preamble and a later "Forbidden" one
// happens before the first reaches the boundary; the first error redacts only its own bearer.
func TestScopedLibraryErrorsKeepTheirOwnRecord(t *testing.T) {
	const first = "scope-first-RDX-7e19"
	const second = "scope-second-RDX-3c5a"
	page := func(r *http.Request) string {
		return "gateway preamble shared by every refusal; it carried " + r.Header.Get("Authorization")
	}
	refused := func(token string) error {
		s := redact.Scope[*gooidc.Provider](context.Background())
		inner := redact.RefusalClient(&http.Client{Transport: scopeRoundTripper{
			status: http.StatusForbidden,
			reason: func(*http.Request) string { return "Forbidden" },
			body:   page,
		}})
		client := s.Client(oauth2.NewClient(
			context.WithValue(s.Context(), oauth2.HTTPClient, inner),
			oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token, TokenType: "Bearer"}),
		))
		_, err := gooidc.NewProvider(gooidc.ClientContext(s.Context(), client), "https://idp.example")
		if err == nil {
			t.Fatal("discovery answered no error")
		}
		_, err = redact.OAuth2(s.Read(nil, fmt.Errorf("%w; unrelated %s", err, second)))
		return err
	}
	firstErr := refused(first)
	if firstErr == nil {
		t.Fatal("first discovery answered no error")
	}
	// A later short answer has no relationship to the first error. A text lookup would select it.
	later := redact.RefusalClient(&http.Client{Transport: scopeRoundTripper{
		status: http.StatusForbidden,
		reason: func(*http.Request) string { return "Forbidden" },
		body:   func(*http.Request) string { return "Forbidden" },
	}})
	response, err := later.Get("https://other.example/")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	out := firstErr.Error()
	t.Logf("REDACT-CAPTURE redact.record-interleaved %q", out)
	if strings.Contains(out, first) {
		t.Fatalf("REDACT-ASSERT redact.record-interleaved: %q", out)
	}
	if !strings.Contains(out, second) {
		t.Fatalf("the first record redacted a credential another request carried: %q", out)
	}
}

// TestALibraryErrorNeedsTheRecordItCameWith: a request whose credential holds the word
// "metadata" is refused, and another error names the metadata server. The second error has no
// record of a request, so it fails closed rather than borrowing the first refusal's credentials.
// The refusal's own text is read under that refusal's scope and redacts only that request.
func TestALibraryErrorNeedsTheRecordItCameWith(t *testing.T) {
	const credential = "metadata-identity-token-RDX"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "refused: the identity token "+credential+" is not accepted here")
	}))
	defer srv.Close()
	const other = "no metadata server: the metadata server at 127.0.0.1 could not be reached"
	_, err := redact.OAuth2(struct{}{}, errors.New(other))
	if strings.Contains(err.Error(), credential) || !strings.Contains(err.Error(), "message withheld") {
		t.Errorf("an error with no request record = %q, want its class alone", err)
	}

	s := redact.Scope[struct{}](context.Background())
	request := newRequest(t, http.MethodGet, srv.URL+"/subject?identity="+credential, "", nil)
	response, err := s.Client(nil).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(response.Body)
	response.Body.Close()
	_, err = redact.OAuth2(struct{}{}, s.Err(fmt.Errorf("status code 403: %s", answer)))
	if strings.Contains(err.Error(), credential) {
		t.Errorf("a report that carries the refusal's record prints its credential: %q", err)
	}
}

// TestEveryCredentialARequestCarriesIsInItsScopesRecord: the requests of
// TestEveryCredentialARequestCarriesIsRedactedFromItsRefusal, sent through a client a scope made,
// write their records into it, so the library error read under that scope holds none of the
// credentials each carried, in a header, its URL's user information or query, or its form or JSON
// body.
func TestEveryCredentialARequestCarriesIsInItsScopesRecord(t *testing.T) {
	srv := echoServer(t)
	for _, row := range requestEchoRows(srv) {
		t.Run(row.name, func(t *testing.T) {
			s := redact.Scope[struct{}](context.Background())
			client := s.Client(&http.Client{})
			response, err := client.Do(row.send(t, client))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if len(redacttest.Held(string(body))) == 0 {
				t.Fatalf("the control: the answer holds no echo of the request: %q", body)
			}
			_, err = redact.OAuth2(struct{}{}, s.Err(fmt.Errorf("status code %d: %s", response.StatusCode, body)))
			redacttest.RequireNone(t, "redact.request-record/"+row.name, err.Error())
			if strings.Contains(err.Error(), "message withheld") {
				t.Errorf("the scope's record covers the error, which prints its class alone: %q", err)
			}
		})
	}
}
