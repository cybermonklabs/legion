package going_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// unparsedChatEndpoint is a configured chat endpoint net/url refuses, for its port, while it holds
// the probe's user name, password and access token.
const unparsedChatEndpoint = "https://" + redacttest.User + ":" + redacttest.Password + "@models.example:port/openai/v1?access" + "_token=" + redacttest.Token

// TestAChatEndpointThatDoesNotParseNamesNoCredentialOfIt: NewHTTPClient's refusal of a configured
// endpoint that does not parse names the endpoint only as redact.URL reads it, never in net/url's
// words, which quote it whole with its user name, password and query. Captures
// going.model-words/unparsed.
func TestAChatEndpointThatDoesNotParseNamesNoCredentialOfIt(t *testing.T) {
	_, err := going.NewHTTPClient(going.HTTPConfig{
		Place: "endpoint", Endpoint: unparsedChatEndpoint, Model: "gpt-test", Tokens: staticTokens{token: "chat-bearer"},
	})
	if err == nil {
		t.Fatal("NewHTTPClient took an endpoint that does not parse")
	}
	redacttest.RequireNone(t, "going.model-words/unparsed", err.Error())
	if !strings.Contains(err.Error(), "going: chat endpoint") {
		t.Errorf("NewHTTPClient's refusal is %q, want it to name the chat endpoint", err)
	}
}

// chatCodeBearer is the bearer the chat client presents in TestAChatRefusalsCodeHoldsNoBearerItEchoed:
// made up, holding two of the gate's markers (eyJhbGci and -RDX-).
const chatCodeBearer = "eyJhbGciOiJFZERTQSJ9.chat-RDX-bearer.c2lnbmF0dXJlLWNoYXQ"

// TestAChatRefusalsCodeHoldsNoBearerItEchoed: a chat completions service, or whatever answers in
// its place, refuses a call with an error code that echoes the bearer the call carried. Chat's
// error quotes the code (%q), which prints the code as the rule for remote text reads it, the
// call's own credentials redacted first: the code is a box decoded with the request's record
// (redact.Answer.Decode), so no run of the bearer prints. Captures going.model-words/code.
func TestAChatRefusalsCodeHoldsNoBearerItEchoed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if got := r.Header.Get("Authorization"); got != "Bearer "+chatCodeBearer {
			t.Errorf("the chat call carried %q, want the bearer the refusal echoes", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"`+chatCodeBearer+`","message":"the bearer is refused"}}`)
	}))
	t.Cleanup(server.Close)
	client, err := going.NewHTTPClient(going.HTTPConfig{
		Place: "endpoint", Endpoint: server.URL + "/openai/v1", Model: "gpt-test", Tokens: staticTokens{token: chatCodeBearer},
	})
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	_, err = client.Chat(t.Context(), testRequest(), nil)
	if err == nil {
		t.Fatal("Chat answered no error")
	}
	redacttest.RequireNoPiece(t, "going.model-words/code", err.Error(), chatCodeBearer)
	if want := "HTTP 401 Unauthorized, error code"; !strings.Contains(err.Error(), want) {
		t.Errorf("Chat's error is %q, want %q in it", err, want)
	}
}
