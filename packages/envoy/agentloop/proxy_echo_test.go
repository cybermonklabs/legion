package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
)

// proxyEcho is a gateway standing in the provider's place that answers every request with a page
// of its own, as a proxy's error or debug page can, echoing the request's credentials back. It
// records each credential the requests carried, so a test can tell a page that echoed the token
// from one that had none to echo.
type proxyEcho struct {
	// s is the stand-in the run and decision fixtures are built on; it serves the echo and
	// records nothing of its own.
	s *standIn

	mu          sync.Mutex
	credentials []string
}

// echoAnswer is what a proxyEcho answers one request with: a status, the reason phrase its
// status line carries (the status's own text where it is empty), a content type and a body built
// from the request and the credentials it carried.
type echoAnswer struct {
	status      int
	reason      string
	contentType string
	page        func(r *http.Request, bare []string) string
}

func newProxyEcho(t *testing.T, answer echoAnswer) *proxyEcho {
	t.Helper()
	e := &proxyEcho{s: &standIn{}}
	e.s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		bare := carried(r.Header)
		e.mu.Lock()
		e.credentials = append(e.credentials, bare...)
		e.mu.Unlock()
		body := ""
		if answer.page != nil {
			body = answer.page(r, bare)
		}
		if answer.reason == "" {
			w.Header().Set("Content-Type", answer.contentType)
			w.WriteHeader(answer.status)
			_, _ = io.WriteString(w, body)
			return
		}
		// net/http writes a status's own text as its reason phrase, so a phrase of the
		// gateway's own is written on the raw connection.
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack the connection: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(buffered, "HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			answer.status, answer.reason, answer.contentType, len(body), body)
		_ = buffered.Flush()
	}))
	t.Cleanup(e.s.server.Close)
	return e
}

// received is every credential the echo's requests carried, in order.
func (e *proxyEcho) received() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.credentials)
}

// carried is each credential a request's headers carry, bare: the X-Api-Key header's value and
// the token after the Authorization header's scheme.
func carried(header http.Header) []string {
	var bare []string
	if key := header.Get("X-Api-Key"); key != "" {
		bare = append(bare, key)
	}
	if _, token, ok := strings.Cut(header.Get("Authorization"), " "); ok && token != "" {
		bare = append(bare, token)
	}
	return bare
}

// echoPage is a proxy's HTML page echoing the request: each credential bare on a line of its own
// first, where only the redaction of the tokens the request carried can hide it (no Authorization,
// Bearer or DPoP stands before it), and then every header as it arrived.
func echoPage(r *http.Request, bare []string) string {
	var page strings.Builder
	page.WriteString("<html><head><title>403 Forbidden</title></head><body>\n")
	for _, credential := range bare {
		page.WriteString(credential + "\n")
	}
	page.WriteString("<pre>\n")
	_ = r.Header.Write(&page)
	page.WriteString("</pre></body></html>\n")
	return page.String()
}

// echoJSON is a proxy's JSON echo of the request: each credential bare, as a JSON string on a line
// of its own, and every header as it arrived. It is JSON, and no answer and no error of any API
// this package calls.
func echoJSON(r *http.Request, bare []string) string {
	var echo strings.Builder
	echo.WriteString("{\"echo\":[\n")
	for i, credential := range bare {
		if i > 0 {
			echo.WriteString(",\n")
		}
		echo.WriteString(strconv.Quote(credential) + "\n")
	}
	headers, _ := json.Marshal(r.Header)
	echo.WriteString("],\"headers\":" + string(headers) + "}\n")
	return echo.String()
}

// leaked is the first run of eight characters of token that text holds, and "" where it holds
// none: eight is the shortest piece of a token an excerpt redacts wherever it stands
// (packages/envoy/excerpt), so any piece of the credential at all is caught.
func leaked(text, token string) string {
	for i := range len(token) - 7 {
		if piece := token[i : i+8]; strings.Contains(text, piece) {
			return piece
		}
	}
	return ""
}

// A run whose model endpoint is answered by a proxy's page echoing the request names no piece of
// the credential the request carried, on every kind Run serves. On anthropic-messages and
// azure-anthropic, whose SDK error appends the whole body to its message, the page is named as a
// body that is not the provider's error JSON, its credential redacted; on the two OpenAI kinds
// the SDK prints only the status and the server's own message field, so no page reaches the
// detail at all. The stand-in records the credential each request carried, so a page that echoed
// nothing is told from one whose echo was hidden.
func TestAProxyEchoOfARunNamesNoCredential(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		kind  Kind
		token string
		// named is how the detail names the page, and is empty for a kind that prints no
		// body at all.
		named string
	}{
		{AnthropicMessages, "pxecho-am-run-7f3a9c0b1d2e4f56a8b9", "a body that is not Anthropic's error JSON (\""},
		{AzureAnthropic, "azure-entra-proxyecho-run-0123456789abcdef", "a body that is not Azure Foundry's error JSON (\""},
		{OpenAIResponses, "pxecho-or-run-7f3a9c0b1d2e4f56a8b9", ""},
		{OpenAIChat, "sk-chat-proxyecho-run-0123456789abcdef", ""},
	} {
		t.Run(string(c.kind), func(t *testing.T) {
			t.Parallel()
			e := newProxyEcho(t, echoAnswer{status: http.StatusForbidden, contentType: "text/html", page: echoPage})
			f := newFixture(t, e.s, c.kind, 1)
			writeToken(t, f.token, c.token)
			result, err := Run(context.Background(), f.req)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("the detail: %s", result.Detail)
			if got := e.received(); !slices.Contains(got, c.token) {
				t.Fatalf("the echo got the credentials %q; want the request's token, which the page then echoes", got)
			}
			if result.Fault != FaultProviderError {
				t.Errorf("fault %q, want %q: %s", result.Fault, FaultProviderError, result.Detail)
			}
			if piece := leaked(result.Detail, c.token); piece != "" {
				t.Errorf("the detail holds %q of the request's token: %s", piece, result.Detail)
			}
			if !strings.Contains(result.Detail, "403") {
				t.Errorf("the detail is %q, want the status the page came with named in it", result.Detail)
			}
			if c.named == "" {
				return
			}
			if !strings.Contains(result.Detail, c.named) || !strings.Contains(result.Detail, "[redacted]") {
				t.Errorf("the detail is %q, want the page named as %s…) with the token redacted", result.Detail, c.named)
			}
		})
	}
}

// The provider's own error JSON is the server's own words and prints as it did: the whole body,
// as the provider wrote it, at the end of the SDK's message, whether it is Anthropic's shape or
// the one Azure answers an error in.
func TestAProviderErrorJSONOfARunPrintsAsItDid(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		kind Kind
		body string
	}{
		{AnthropicMessages, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`},
		{AzureAnthropic, `{"error":{"code":"PermissionDenied","message":"The principal lacks the data action."}}`},
	} {
		t.Run(string(c.kind), func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, reply{status: http.StatusUnauthorized, body: []byte(c.body)})
			result, err := Run(context.Background(), newFixture(t, s, c.kind, 1).req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Fault != FaultProviderError || !strings.Contains(result.Detail, "401") || !strings.HasSuffix(result.Detail, " "+c.body) {
				t.Errorf("result %+v; want %q naming the status and ending with the provider's own body, %s", result, FaultProviderError, c.body)
			}
		})
	}
}

// A provider error whose message does not end in its body, which is not the message
// anthropic-sdk-go writes today, keeps none of that message, since it may hold the body in some
// other form: the detail is the status and the body as the excerpt names it.
func TestAProviderErrorWhoseMessageDoesNotEndInItsBodyKeepsNoneOfIt(t *testing.T) {
	t.Parallel()
	const token = "pxecho-am-unit-7f3a9c0b1d2e4f56a8b9"
	apiErr := &anthropicsdk.Error{
		StatusCode: http.StatusForbidden,
		Request:    httptest.NewRequest(http.MethodPost, "https://api.anthropic.example/v1/messages", nil),
		Response:   &http.Response{StatusCode: http.StatusForbidden},
	}
	if err := apiErr.UnmarshalJSON([]byte("<html><body>\n" + token + "\n</body></html>")); err != nil {
		t.Fatal(err)
	}
	detail := providerDetail(fmt.Errorf("%w (and words after the body)", apiErr), AnthropicMessages, []string{token})
	if want := `the anthropic-messages endpoint answered 403 with a body that is not Anthropic's error JSON ("[redacted]")`; detail != want {
		t.Errorf("the detail is %q; want %q", detail, want)
	}
}

// A decision whose endpoint is answered by a proxy echoing the request names no piece of the
// bearer the request carried, whatever the echo came with: an error status and a page, a status
// line whose reason phrase is the gateway's own, a 2xx page that is no JSON, and a 2xx JSON echo
// that is no answer. Each body is named as one that is not System One's, its credential redacted,
// and a reason phrase that is not System One's reads through the same excerpt.
func TestAProxyEchoOfADecisionNamesNoCredential(t *testing.T) {
	t.Parallel()
	const token = "typesafe-proxyecho-decide-0123456789abcdef"
	for _, c := range []struct {
		name   string
		answer echoAnswer
		fault  Fault
		named  string
	}{
		{
			name:   "an error status",
			answer: echoAnswer{status: http.StatusForbidden, contentType: "text/html", page: echoPage},
			fault:  FaultProviderError, named: "a body that is not System One's error JSON (\"",
		},
		{
			name: "a reason phrase of the gateway's own",
			answer: echoAnswer{status: http.StatusForbidden, reason: "Forbidden for Bearer " + token + " " + token,
				contentType: "text/plain"},
			fault: FaultProviderError, named: "403 Forbidden for Bearer [redacted] [redacted]: an empty body",
		},
		{
			name:   "a 2xx that is no JSON",
			answer: echoAnswer{status: http.StatusOK, contentType: "text/html", page: echoPage},
			fault:  FaultMalformed, named: "a body that is not a System One answer (\"",
		},
		{
			name:   "a 2xx that is no answer",
			answer: echoAnswer{status: http.StatusOK, contentType: "application/json", page: echoJSON},
			fault:  FaultMalformed, named: "the answer holds no answers or no usage: a body that is not a System One answer (\"",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newProxyEcho(t, c.answer)
			req := newDecideFixture(t, e.s)
			req.Model.Endpoint.Token = func(context.Context) (string, error) { return token, nil }
			result, err := Decide(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("the detail: %s", result.Detail)
			if got := e.received(); !slices.Contains(got, token) {
				t.Fatalf("the echo got the credentials %q; want the request's bearer, which the answer then echoes", got)
			}
			if result.Fault != c.fault || len(result.Answers) != 0 {
				t.Errorf("result %+v, want %q and no answers", result, c.fault)
			}
			if piece := leaked(result.Detail, token); piece != "" {
				t.Errorf("the detail holds %q of the request's bearer: %s", piece, result.Detail)
			}
			if !strings.Contains(result.Detail, c.named) {
				t.Errorf("the detail is %q, want %s in it", result.Detail, c.named)
			}
			if c.answer.page != nil && !strings.Contains(result.Detail, "[redacted]") {
				t.Errorf("the detail is %q, want the echoed bearer redacted in it", result.Detail)
			}
		})
	}
}

// System One's own error JSON, an object with a detail member (the API's HTTPBadRequestError and
// HTTPValidationError), is the server's own words and prints as it did: the status line and the
// body quoted, up to 500 runes.
func TestASystemOneErrorJSONPrintsAsItDid(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"detail":"Not authenticated"}`,
		`{"detail":[{"loc":["body","state"],"msg":"Field required","type":"missing"}]}`,
	} {
		s := newStandIn(t, reply{status: http.StatusBadRequest, body: []byte(body)})
		result, err := Decide(context.Background(), newDecideFixture(t, s))
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("400 Bad Request: %q", body); result.Fault != FaultProviderError || result.Detail != want {
			t.Errorf("result %+v; want %q with the detail %s", result, FaultProviderError, want)
		}
	}
}
