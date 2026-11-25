package agentloop

import (
	"context"
	"encoding/json"
	"errors"
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

// refusedRedirect is the transport's words for a redirect it refused, after the endpoint's kind:
// its status, by its code and standard phrase.
const refusedRedirect = "endpoint answered 302 Found, a redirect, which is never followed"

// credentialRequest is a POST to endpoint that carries echoCredential in each header a model call
// carries a credential in (X-Api-Key on anthropic-messages, a bearer on the rest), for the twins
// to send with net/http's own client or transport.
func credentialRequest(t *testing.T, endpoint Endpoint) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, endpoint.BaseURL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+echoCredential)
	request.Header.Set("X-Api-Key", echoCredential)
	return request
}

// requireLocationEcho is the twin of the redirect rows: endpoint's stand-in, asked as a model call
// asks it, answers a Location that quotes the credential the call carried, as whatever answers in
// the endpoint's place can. It is read through net/http's transport alone, which reads no Location
// and follows none.
func requireLocationEcho(t *testing.T, endpoint Endpoint) {
	t.Helper()
	response, err := http.DefaultTransport.RoundTrip(credentialRequest(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if location := response.Header.Get("Location"); !strings.Contains(location, echoCredential) {
		t.Fatalf("the stand-in answered %d with the Location %q, which holds no echo of the credential: the row cannot see a leak",
			response.StatusCode, location)
	}
}

// TestADecisionsFailedCallNamesNoLocationOfIt: whatever answers at the decision endpoint answers
// the call, which carries the endpoint's bearer, with a redirect whose Location quotes that bearer:
// one net/http cannot parse (unparsed), one to another host with the bearer in its query
// (elsewhere), and one with the bearer as its host (host). The transport refuses each by its
// status, so the decision's Detail, which the judge logs, holds no run of the bearer and no word
// of the Location, and names the status. The twin shows the stand-in's Location holds the echo.
// Captures agentloop.decide-redirect/<case>.
func TestADecisionsFailedCallNamesNoLocationOfIt(t *testing.T) {
	t.Parallel()
	dead := redirectstest.Dead(t)
	cases := []struct {
		name     string
		location func(credential string) string
	}{
		{"unparsed", redirectstest.Unparsed},
		{"elsewhere", func(credential string) string { return redirectstest.To(dead, credential) }},
		{"host", func(credential string) string { return "http://" + credential + ".invalid/moved" }},
	}
	t.Run("twin", func(t *testing.T) {
		t.Parallel()
		for _, c := range cases {
			requireLocationEcho(t, echoEndpoint(t, SystemOne, redirectTo(c.location)))
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
			if want := "the " + string(SystemOne) + " " + refusedRedirect; result.Fault != FaultProviderError || !strings.Contains(result.Detail, want) {
				t.Errorf("result %+v, want %q naming %q", result, FaultProviderError, want)
			}
		})
	}
}

// runRequest is a run of one call to endpoint, which asks for text.
func runRequest(t *testing.T, endpoint Endpoint) Request {
	return Request{
		Model:  Model{Endpoint: endpoint, ID: "standin-model", Price: anthropicPrice},
		Input:  []Message{{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "What does note.txt say?"}}}},
		Bounds: Bounds{Time: runnerTime(t), Calls: 1, OutputTokens: 100},
	}
}

// TestARunsProviderFailureNamesNoLocationOrAnswerOfIt: whatever answers at a run's endpoint, on
// each protocol whose SDK client Run calls a stand-in through, answers the call, which carries the
// endpoint's credential, with a redirect whose Location quotes that credential and does not parse
// (unparsed), with one to another host (elsewhere), which the transport refuses by its status, or
// with a 401 whose error message quotes it (refused). The SDK's stream error holds the answer's
// own words for the 401: anthropic-sdk-go's error prints the whole body, and the ai-sdk's OpenAI
// providers put the message in theirs. The run's Detail, which the judge logs, holds no run of the
// credential and no word of the Location, and names a refusal by its status. The twin shows the
// stand-in's Location holds the echo, and the SDK's own error holds the 401's.
// Captures agentloop.run-failure/<kind>-<case>.
func TestARunsProviderFailureNamesNoLocationOrAnswerOfIt(t *testing.T) {
	t.Parallel()
	dead := redirectstest.Dead(t)
	type runCase struct {
		name       string
		kind       Kind
		answer     echoAnswer
		redirected bool
		want       string
	}
	var cases []runCase
	for _, kind := range []Kind{AnthropicMessages, OpenAIResponses, OpenAIChat} {
		redirect := "the " + string(kind) + " " + refusedRedirect
		cases = append(cases,
			runCase{string(kind) + "-unparsed", kind, redirectTo(redirectstest.Unparsed), true, redirect},
			runCase{string(kind) + "-elsewhere", kind, redirectTo(func(credential string) string { return redirectstest.To(dead, credential) }),
				true, redirect},
			runCase{string(kind) + "-refused", kind, refusedEcho, false, "the " + string(kind) + " endpoint answered 401 Unauthorized"},
		)
	}
	t.Run("twin", func(t *testing.T) {
		t.Parallel()
		for _, c := range cases {
			endpoint := echoEndpoint(t, c.kind, c.answer)
			if c.redirected {
				requireLocationEcho(t, endpoint)
				continue
			}
			err := sdkStreamError(t, runRequest(t, endpoint))
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
			result, err := Run(context.Background(), runRequest(t, echoEndpoint(t, c.kind, c.answer)))
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

// requireRefusalEcho is the twin of the redirect-error rows: endpoint's stand-in, asked by
// net/http's own client under a redirect policy that fails the call, answers net/http's error,
// which names the Location the stand-in wrote: one that does not parse quoted whole, any other as
// the *url.Error's URL.
func requireRefusalEcho(t *testing.T, endpoint Endpoint) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("refused") }}
	response, err := client.Do(credentialRequest(t, endpoint))
	if err == nil {
		_ = response.Body.Close()
		t.Fatalf("net/http answered status %d and no error: the row cannot see one", response.StatusCode)
	}
	if !strings.Contains(err.Error(), echoCredential) {
		t.Fatalf("net/http's own error holds no echo of the credential, so the row cannot see a leak: %q", err)
	}
}

// TestAModelCallsOwnErrorNamesNoLocationOfARedirect: whatever answers at a model endpoint answers
// the call, which carries the endpoint's credential, with a redirect whose Location quotes it: one
// net/http cannot parse (unparsed), one to another host (elsewhere) and, for a decision, one
// whose host is the credential (host). The transport refuses the redirect by its status before
// net/http's client reads the Location, so the error the decision's client returns, and the SDK's
// own stream error of a run on each protocol, hold no run of the credential and name the status.
// The twin shows net/http's own client, under a redirect policy that fails the call, names the
// Location in its error. Captures agentloop.redirect-error/<case>.
func TestAModelCallsOwnErrorNamesNoLocationOfARedirect(t *testing.T) {
	t.Parallel()
	dead := redirectstest.Dead(t)
	elsewhere := func(credential string) string { return redirectstest.To(dead, credential) }
	type errorCase struct {
		name     string
		kind     Kind
		location func(credential string) string
	}
	cases := []errorCase{
		{"decide-unparsed", SystemOne, redirectstest.Unparsed},
		{"decide-elsewhere", SystemOne, elsewhere},
		{"decide-host", SystemOne, func(credential string) string { return "http://" + credential + ".invalid/moved" }},
	}
	for _, kind := range []Kind{AnthropicMessages, OpenAIResponses, OpenAIChat} {
		cases = append(cases,
			errorCase{string(kind) + "-unparsed", kind, redirectstest.Unparsed},
			errorCase{string(kind) + "-elsewhere", kind, elsewhere})
	}
	t.Run("twin", func(t *testing.T) {
		t.Parallel()
		for _, c := range cases {
			requireRefusalEcho(t, echoEndpoint(t, c.kind, redirectTo(c.location)))
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			endpoint := echoEndpoint(t, c.kind, redirectTo(c.location))
			var err error
			if c.kind == SystemOne {
				post, built := http.NewRequest(http.MethodPost, endpoint.BaseURL+decidePath, strings.NewReader("{}"))
				if built != nil {
					t.Fatal(built)
				}
				var response *http.Response
				if response, err = newModelTransport(endpoint).client().Do(post); err == nil {
					_ = response.Body.Close()
				}
			} else {
				err = sdkStreamError(t, runRequest(t, endpoint))
			}
			if err == nil {
				t.Fatalf("the call answered the redirect with no error")
			}
			redacttest.RequireNoPiece(t, "agentloop.redirect-error/"+c.name, err.Error(), echoCredential)
			if want := "the " + string(c.kind) + " " + refusedRedirect; !strings.Contains(err.Error(), want) {
				t.Errorf("the call's error is %q, want it naming %q", err, want)
			}
		})
	}
}

// headerLineEcho answers a call with a header line net/http cannot parse, one with no colon, that
// quotes the credential the call carried: net/http's error for the answer quotes the line whole.
func headerLineEcho(w http.ResponseWriter, credential string) {
	conn, written, err := http.NewResponseController(w).Hijack()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	_, _ = written.WriteString("HTTP/1.1 200 OK\r\nEcho " + credential + "\r\nContent-Length: 0\r\n\r\n")
	_ = written.Flush()
}

// TestAFailedCallNamesNoneOfAHeaderLineItCannotRead: whatever answers at a model endpoint answers
// the call, which carries the endpoint's credential, with a header line net/http cannot parse that
// quotes it: net/http's error quotes the line, and each SDK hands that error on. The Detail of a
// decision and of a run on each protocol holds no run of the credential and names the failure's
// class (redact.Cause). The twin shows net/http's own error quotes the line. Captures
// agentloop.header-line/<case>.
func TestAFailedCallNamesNoneOfAHeaderLineItCannotRead(t *testing.T) {
	t.Parallel()
	cases := []Kind{SystemOne, AnthropicMessages, OpenAIResponses, OpenAIChat}
	t.Run("twin", func(t *testing.T) {
		t.Parallel()
		for _, kind := range cases {
			redirectstest.RequireEcho(t, credentialRequest(t, echoEndpoint(t, kind, headerLineEcho)), echoCredential)
		}
	})
	for _, kind := range cases {
		name := string(kind)
		if kind == SystemOne {
			name = "decide"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			endpoint := echoEndpoint(t, kind, headerLineEcho)
			var fault Fault
			var detail string
			if kind == SystemOne {
				result, err := Decide(context.Background(), DecideRequest{
					Model:     Model{Endpoint: endpoint, ID: "systemone-standin", Price: systemOnePrice},
					State:     json.RawMessage(`{"note":"The note says plum."}`),
					Questions: decideQuestions(),
					Bounds:    Bounds{Time: runnerTime(t)},
				})
				if err != nil {
					t.Fatal(err)
				}
				fault, detail = result.Fault, result.Detail
			} else {
				result, err := Run(context.Background(), runRequest(t, endpoint))
				if err != nil {
					t.Fatal(err)
				}
				fault, detail = result.Fault, result.Detail
			}
			redacttest.RequireNoPiece(t, "agentloop.header-line/"+name, detail, echoCredential)
			if fault != FaultProviderError || detail != redact.ClassRequestFailed {
				t.Errorf("fault %q, detail %q; want %q, %q", fault, detail, FaultProviderError, redact.ClassRequestFailed)
			}
		})
	}
}
