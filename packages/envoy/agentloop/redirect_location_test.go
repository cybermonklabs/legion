package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	aisdk "github.com/grafana/ai-sdk"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// echoCredential is the credential the rows' endpoints are called with, which their stand-ins
// echo.
const echoCredential = "pw3ky-not-a-secret-RDX-6d0a"

// echoAnswer is how a stand-in answers a call, given the credential the call carried.
type echoAnswer func(w http.ResponseWriter, credential string)

// redirectTo answers a call with a redirect to the Location location makes of its credential.
func redirectTo(location func(credential string) string) echoAnswer {
	return func(w http.ResponseWriter, credential string) {
		w.Header().Set("Location", location(credential))
		w.WriteHeader(http.StatusFound)
	}
}

// refusedEcho answers a call 401 with an error body in the shape Anthropic and OpenAI answer one,
// whose message quotes the credential the call carried.
func refusedEcho(w http.ResponseWriter, credential string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"authentication_error","code":"invalid_api_key","message":"invalid key %s"}}`, credential)
}

// echoEndpoint is an endpoint of kind at a stand-in that answers every call as answer does, with
// the credential the call carried in kind's header, read from a file as a caller's is.
func echoEndpoint(t *testing.T, kind Kind, answer echoAnswer) Endpoint {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credential := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if kind == AnthropicMessages {
			credential = r.Header.Get("X-Api-Key")
		}
		answer(w, credential)
	}))
	t.Cleanup(server.Close)
	base := server.URL
	if kind == OpenAIResponses || kind == OpenAIChat {
		base += "/v1"
	}
	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, echoCredential)
	return Endpoint{Kind: kind, BaseURL: base, Token: fileToken(path)}
}

// TestADecisionsFailedCallNamesNoLocationOfIt: whatever answers at the decision endpoint answers
// the call, which carries the endpoint's bearer, with a redirect whose Location quotes that bearer.
// One net/http cannot parse (unparsed) is net/http's error whatever the client's redirect check,
// since Go parses a Location before it asks CheckRedirect; one that parses, which the check
// refuses, is named in full by Go's *url.Error, whether the bearer is in its query (elsewhere) or
// in its host (host). The decision's Detail, which the judge logs, holds no run of the bearer and
// no word of the Location. The twin shows the stand-in's echo reaches the client's own error.
// Captures agentloop.decide-redirect/<case>.
func TestADecisionsFailedCallNamesNoLocationOfIt(t *testing.T) {
	t.Parallel()
	dead := redirectstest.Dead(t)
	cases := []struct {
		name     string
		location func(credential string) string
		want     string
	}{
		{"unparsed", redirectstest.Unparsed, redact.ClassRequestFailed},
		{"elsewhere", func(credential string) string { return redirectstest.To(dead, credential) }, "302 Found, a redirect, which is never followed"},
		{"host", func(credential string) string { return "http://" + credential + ".invalid/moved" }, "302 Found, a redirect, which is never followed"},
	}
	t.Run("twin", func(t *testing.T) {
		t.Parallel()
		for _, c := range cases {
			endpoint := echoEndpoint(t, SystemOne, redirectTo(c.location))
			post, err := http.NewRequest(http.MethodPost, endpoint.BaseURL+decidePath, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			response, err := newModelTransport(endpoint).client().Do(post)
			if err == nil {
				_ = response.Body.Close()
				t.Fatalf("%s: the client answered the redirect with no error: the row cannot see one", c.name)
			}
			if !strings.Contains(err.Error(), echoCredential) {
				t.Fatalf("%s: the client's own error holds no echo of the bearer, so the row cannot see a leak: %q", c.name, err)
			}
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			result, err := Decide(context.Background(), DecideRequest{
				Model:     Model{Endpoint: echoEndpoint(t, SystemOne, redirectTo(c.location)), ID: "systemone-standin", Price: systemOnePrice},
				State:     json.RawMessage(`{"note":"The note says plum."}`),
				Questions: decideQuestions(),
				Bounds:    Bounds{Time: runnerTime(t)},
			})
			if err != nil {
				t.Fatal(err)
			}
			redacttest.RequireNoPiece(t, "agentloop.decide-redirect/"+c.name, result.Detail, echoCredential)
			if strings.Contains(result.Detail, "moved") {
				t.Errorf("the detail names the Location: %q", result.Detail)
			}
			if result.Fault != FaultProviderError || !strings.Contains(result.Detail, c.want) {
				t.Errorf("result %+v, want %q naming %q", result, FaultProviderError, c.want)
			}
		})
	}
}

// TestARunsProviderFailureNamesNoLocationOrAnswerOfIt: whatever answers at a run's endpoint, on
// each protocol whose SDK client Run calls a stand-in through, answers the call, which carries the
// endpoint's credential, with a redirect whose Location quotes that credential and does not parse
// (unparsed), with one the client's redirect check refuses, on another host (elsewhere), or with
// a 401 whose error message quotes it (refused). The SDK's stream error holds net/http's error
// for the first two and the answer's own words for the third: anthropic-sdk-go's error prints the
// whole body, and the ai-sdk's OpenAI providers put the message in theirs. The run's Detail, which
// the judge logs, holds no run of the credential and no word of the Location, and names a refusal
// by its status. The twin shows the stand-in's echo reaches the SDK's own error in every case.
// Captures agentloop.run-failure/<kind>-<case>.
func TestARunsProviderFailureNamesNoLocationOrAnswerOfIt(t *testing.T) {
	t.Parallel()
	dead := redirectstest.Dead(t)
	type runCase struct {
		name   string
		kind   Kind
		answer echoAnswer
		want   string
	}
	var cases []runCase
	for _, kind := range []Kind{AnthropicMessages, OpenAIResponses, OpenAIChat} {
		cases = append(cases,
			runCase{string(kind) + "-unparsed", kind, redirectTo(redirectstest.Unparsed), redact.ClassRequestFailed},
			runCase{string(kind) + "-elsewhere", kind, redirectTo(func(credential string) string { return redirectstest.To(dead, credential) }),
				"302 Found, a redirect, which is never followed"},
			runCase{string(kind) + "-refused", kind, refusedEcho, "the " + string(kind) + " endpoint answered 401 Unauthorized"},
		)
	}
	request := func(t *testing.T, endpoint Endpoint) Request {
		return Request{
			Model:  Model{Endpoint: endpoint, ID: "standin-model", Price: anthropicPrice},
			Input:  []Message{{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "What does note.txt say?"}}}},
			Bounds: Bounds{Time: runnerTime(t), Calls: 1, OutputTokens: 100},
		}
	}
	t.Run("twin", func(t *testing.T) {
		t.Parallel()
		for _, c := range cases {
			err := sdkStreamError(t, request(t, echoEndpoint(t, c.kind, c.answer)))
			if err == nil {
				t.Fatalf("%s: the SDK's stream ended with no error: the row cannot see one", c.name)
			}
			if !strings.Contains(err.Error(), echoCredential) {
				t.Fatalf("%s: the SDK's own error holds no echo of the credential, so the row cannot see a leak: %q", c.name, err)
			}
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			result, err := Run(context.Background(), request(t, echoEndpoint(t, c.kind, c.answer)))
			if err != nil {
				t.Fatal(err)
			}
			redacttest.RequireNoPiece(t, "agentloop.run-failure/"+c.name, result.Detail, echoCredential)
			if strings.Contains(result.Detail, "moved") || strings.Contains(result.Detail, "invalid key") {
				t.Errorf("the detail names the Location or the answer's words: %q", result.Detail)
			}
			if result.Fault != FaultProviderError || !strings.Contains(result.Detail, c.want) {
				t.Errorf("result %+v, want %q naming %q", result, FaultProviderError, c.want)
			}
		})
	}
}

// sdkStreamError is the SDK's own error for one call of req, as Run's stream holds it before Run
// says it in a Detail: the same client, model and options Run builds, streamed to its end.
func sdkStreamError(t *testing.T, req Request) error {
	t.Helper()
	model, options, err := languageModel(req, newModelTransport(req.Model.Endpoint).client())
	if err != nil {
		t.Fatal(err)
	}
	messages, err := toProvider(req.Input, req.Model.Endpoint.Kind)
	if err != nil {
		t.Fatal(err)
	}
	stream := aisdk.StreamText(context.Background(), model, aisdk.WithModelMessages(messages...), aisdk.WithProviderOptions(options),
		aisdk.WithMaxRetries(0), aisdk.WithMaxOutputTokens(req.Bounds.OutputTokens))
	partials := make(chan struct{})
	go func() {
		defer close(partials)
		for range stream.PartialOutputStream() {
		}
	}()
	for range stream.FullStream() {
	}
	<-partials
	return stream.Err()
}
