package redact_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// echoingTransport answers every request as a gateway in a server's place may: a 502 whose reason
// phrase and body each hold the bearer token the request carried, bare.
type echoingTransport struct{}

func (echoingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return &http.Response{
		StatusCode: http.StatusBadGateway,
		Status:     "502 " + token,
		Header:     http.Header{"Content-Type": {"text/html"}},
		Body:       io.NopCloser(strings.NewReader("<pre>\n" + token + "\n</pre>")),
		Request:    r,
	}, nil
}

// bearerRequest is a request that carries the probe's token as its bearer credential.
func bearerRequest(t *testing.T) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:1/token", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+redacttest.Token)
	return request
}

// TestARefusalsEchoOfItsRequestPrintsNoCredentialItCarried: a refusal whose reason phrase and body
// echo the bearer token its own request carried, bare, where the rule for remote text reads no URL
// and so leaves the words as they are. OAuth2's answer (the body and the status x/oauth2's text
// prints) and RefusalClient's response (its status and its body) each hold none of it. Each is a
// designated assertion of the check's mutation gate: redact.tokens-answer-body,
// redact.tokens-answer-status, redact.tokens-refusal-body and redact.tokens-refusal-status.
func TestARefusalsEchoOfItsRequestPrintsNoCredentialItCarried(t *testing.T) {
	t.Run("answer", func(t *testing.T) {
		request := bearerRequest(t)
		for capture, re := range map[string]*oauth2.RetrieveError{
			"redact.tokens-answer-body": {
				Response: &http.Response{StatusCode: http.StatusBadGateway, Status: "502 Bad Gateway", Request: request},
				Body:     []byte("<pre>\n" + redacttest.Token + "\n</pre>"),
			},
			"redact.tokens-answer-status": {
				Response: &http.Response{StatusCode: http.StatusBadGateway, Status: "502 " + redacttest.Token, Request: request},
				Body:     []byte("Bad Gateway"),
			},
		} {
			_, err := redact.OAuth2(0, re)
			if err == nil {
				t.Fatalf("%s: OAuth2 answered no error", capture)
			}
			redacttest.RequireNone(t, capture, err.Error())
		}
	})
	t.Run("refusal client", func(t *testing.T) {
		client := redact.RefusalClient(&http.Client{Transport: echoingTransport{}})
		response, err := client.Do(bearerRequest(t))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		redacttest.RequireNone(t, "redact.tokens-refusal-body", string(body))
		redacttest.RequireNone(t, "redact.tokens-refusal-status", response.Status)
	})
}
