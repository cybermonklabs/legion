package agentloop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/ai-sdk/provider"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The credentials the rows below hand their runs. Each is made up and holds two of the gate's
// markers (eyJhbGci, the start of every JWS, and -RDX-), so the output a row captures names a
// marker wherever it holds a credential.
const (
	// wordsBearer is the bearer the run's transport puts on every openai-chat request.
	wordsBearer = "eyJhbGciOiJFUzI1NiJ9.agentloop-RDX-bearer.c2lnbmF0dXJlLWJlYXJlcg"
	// wordsAzureToken is the token azure-anthropic's credential middleware sets.
	wordsAzureToken = "eyJhbGciOiJSUzI1NiJ9.azure-RDX-token.c2lnbmF0dXJlLWF6dXJl"
	// wordsQueryValue is a query value the endpoint is configured to add to every request.
	wordsQueryValue = "query-RDX-value-5e1f9b0c7d2a"
	// wordsFirstToken and wordsRetryToken are the tokens a run reads for its first attempt and
	// for the transport's one retry of it.
	wordsFirstToken = "eyJhbGciOiJFUzM4NCJ9.first-RDX-token.c2lnbmF0dXJlLWZpcnN0"
	wordsRetryToken = "eyJhbGciOiJFUzUxMiJ9.retry-RDX-token.c2lnbmF0dXJlLXJldHJ5"
)

// verdictFinal is the shape the judge asks of its model's last reply: a verdict of grant or deny
// and a reason, and no other property.
var verdictFinal = json.RawMessage(`{"type":"object","properties":{"verdict":{"type":"string","enum":["grant","deny"]},"reason":{"type":"string"}},"required":["verdict","reason"],"additionalProperties":false}`)

// echoing is words that echo a URL holding a password, credential whole and a run of it, as
// whatever answers in the model service's place can write them. The rule for remote text reads
// them as clean, inside a JSON reply too, so what keeps the credential out of a Detail is the run's
// own record of what it sent (Result.Words), and what keeps the password out is the rule.
func echoing(credential string) string {
	return "the issuer at " + passwordURL + ", then echo " + credential + ", a run " + runOf(credential)
}

// passwordURL is a URL whose user information holds the probe's user name and password.
const passwordURL = "https://" + redacttest.User + ":" + redacttest.Password + "@issuer.example/path"

// runOf is a run of 12 of credential's characters, from its fifth: past the 8 a credential's run
// must reach for redact.Tokens to take it wherever it stands (redact.TokenPiece).
func runOf(credential string) string { return credential[4:16] }

// echoedReply is a last reply that is no object of verdictFinal (its verdict is neither word)
// and whose reason echoes credential.
func echoedReply(credential string) string {
	return `{"verdict":"maybe","reason":` + quote(echoing(credential)) + `}`
}

// TestAModelServicesWordsInADetailHoldNoCredential: a run whose model service, or whatever
// answers in its place, ends the run with words of its own - a refusal, a last reply that is no
// object of the form the request asked for, a reply with a property the form forbids, which the
// SDK's error quotes - and echoes a credential the run sent it whole, a run of it, and a URL that
// holds a password. The run's Detail holds none of them: the bearer the transport sets (refusal,
// malformed, forbidden-key), the token azure-anthropic's middleware sets (azure-key), a query
// value the endpoint is configured to add (query-value), and the token the run read for its
// transport's one retry (retried). Captures agentloop.model-words/<case>.
func TestAModelServicesWordsInADetailHoldNoCredential(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		kind Kind
		// replies are the stand-in's answers in turn.
		replies []reply
		// tokens are the tokens the endpoint reads, one for each call of Token, the last one
		// again for any call after; echoed is the credential the reply echoes.
		tokens []string
		query  url.Values
		echoed string
		fault  Fault
	}{
		{name: "refusal", kind: OpenAIChat, tokens: []string{wordsBearer}, echoed: wordsBearer, fault: FaultRefusal,
			replies: []reply{chatRefusalStream(echoing(wordsBearer), false)}},
		{name: "malformed", kind: OpenAIChat, tokens: []string{wordsBearer}, echoed: wordsBearer, fault: FaultMalformed,
			replies: []reply{chatStream("stop", chatUsage{Prompt: 2, Completion: 1}, echoedReply(wordsBearer))}},
		{name: "forbidden-key", kind: OpenAIChat, tokens: []string{wordsBearer}, echoed: wordsBearer, fault: FaultMalformed,
			replies: []reply{chatStream("stop", chatUsage{Prompt: 2, Completion: 1},
				`{"verdict":"grant","reason":`+quote("the issuer at "+passwordURL+", then a run "+runOf(wordsBearer))+`,`+quote(wordsBearer)+`:1}`)}},
		{name: "azure-key", kind: AzureAnthropic, tokens: []string{wordsAzureToken}, echoed: wordsAzureToken, fault: FaultMalformed,
			replies: []reply{anthropicStream("end_turn", anthropicUsage{Input: 2, Output: 1}, anthropicText(echoedReply(wordsAzureToken)))}},
		{name: "query-value", kind: OpenAIChat, tokens: []string{wordsBearer}, echoed: wordsQueryValue, fault: FaultMalformed,
			query:   url.Values{"key": {wordsQueryValue}},
			replies: []reply{chatStream("stop", chatUsage{Prompt: 2, Completion: 1}, echoedReply(wordsQueryValue))}},
		{name: "retried", kind: OpenAIChat, tokens: []string{wordsFirstToken, wordsRetryToken}, echoed: wordsRetryToken, fault: FaultMalformed,
			replies: []reply{
				{status: http.StatusServiceUnavailable, body: []byte(`{"error":{"message":"the stand-in is busy"}}`), headers: map[string]string{"Retry-After": "0"}},
				chatStream("stop", chatUsage{Prompt: 2, Completion: 1}, echoedReply(wordsRetryToken)),
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, c.replies...)
			f := newFixture(t, s, c.kind, 1)
			f.req.Tools, f.req.Final = nil, verdictFinal
			f.req.Model.Endpoint.Query = c.query
			f.req.Model.Endpoint.Token = tokensInTurn(c.tokens...)
			result, err := Run(context.Background(), f.req)
			if err != nil {
				t.Fatalf("the run: %v", err)
			}
			redacttest.RequireNoPiece(t, "agentloop.model-words/"+c.name, result.Detail, c.echoed)
			if result.Fault != c.fault {
				t.Errorf("the run ended in %q (%s), want %q", result.Fault, result.Detail, c.fault)
			}
			// The stand-in got the credential its reply echoes, so the row is about a credential
			// the run sent it.
			if !sent(s.attempts(), c.echoed) {
				t.Errorf("the stand-in got no request carrying %q, so its echo is of nothing the run sent", c.echoed)
			}
		})
	}
}

// tokensInTurn reads each of tokens in turn, one for each call, and the last one for every call
// after.
func tokensInTurn(tokens ...string) func(context.Context) (string, error) {
	var mu sync.Mutex
	next := 0
	return func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		token := tokens[min(next, len(tokens)-1)]
		next++
		return token, nil
	}
}

// sent reports whether any of requests carried credential in its Authorization header or as a
// query value.
func sent(requests []recorded, credential string) bool {
	for _, r := range requests {
		if r.header.Get("Authorization") == "Bearer "+credential {
			return true
		}
		if query, err := url.ParseQuery(r.query); err == nil {
			for _, values := range query {
				for _, v := range values {
					if v == credential {
						return true
					}
				}
			}
		}
	}
	return false
}

// TestADecisionsUnparsedEndpointNamesNoCredentialOfIt: a decision whose configured endpoint does
// not parse - its port is not a number - while it holds a user name, a password and an access
// token in its query fails before any call, and its error names the endpoint only as redact.URL
// reads it, never in net/url's words, which quote it whole. Decide has no production caller, so
// its returned error is where this prints. Captures agentloop.model-words/decide-unparsed.
func TestADecisionsUnparsedEndpointNamesNoCredentialOfIt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, "the-decision-token")
	_, err := Decide(context.Background(), DecideRequest{
		Model: Model{
			Endpoint: Endpoint{Kind: SystemOne, BaseURL: unparsedEndpoint, Token: fileToken(path)},
			ID:       "systemone-standin", Price: systemOnePrice,
		},
		State:     json.RawMessage(`{"note":"The note says plum."}`),
		Questions: decideQuestions(),
		Bounds:    Bounds{Time: 30 * time.Second},
	})
	if err == nil {
		t.Fatal("the decision on an endpoint that does not parse answered no error")
	}
	redacttest.RequireNone(t, "agentloop.model-words/decide-unparsed", err.Error())
}

// unparsedEndpoint is a configured URL net/url refuses, for its port, while it holds the probe's
// user name, password and access token.
const unparsedEndpoint = "https://" + redacttest.User + ":" + redacttest.Password + "@issuer.example:port/path?access" + "_token=" + redacttest.Token

// TestAFinishReasonNoSDKReportsPrintsAsTheModelsWords: the provider's own finish reason, which a
// Detail names for a reply cut off for its length, stopped by a filter or ended in an error, is one
// of a closed set in the pinned SDKs, each of which maps every other raw value to Other, which the
// run does not name; so no stand-in can make it echo anything, and this row hands finish such a
// reason directly, as an SDK that reported one would. A reason none of them reports prints as the
// model's words are read, holding no secret; one they report prints as it came. Captures
// agentloop.finish-reason/<case>.
func TestAFinishReasonNoSDKReportsPrintsAsTheModelsWords(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		unified provider.UnifiedFinishReason
		known   string
		fault   Fault
	}{
		{"length", provider.FinishReasonLength, "max_tokens", FaultLength},
		{"content-filter", provider.FinishReasonContentFilter, "content_filter", FaultRefusal},
		{"error", provider.FinishReasonError, "error", FaultProviderError},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			finished := func(raw string) Result {
				r := &run{req: Request{Bounds: Bounds{Calls: 1, OutputTokens: 1024}}, kind: AnthropicMessages,
					reason: provider.FinishReason{Unified: c.unified, Raw: raw}}
				result := Result{Steps: []Step{{}}}
				r.finish(&result, nil)
				return result
			}
			unknown := finished(redacttest.Words)
			redacttest.RequireNone(t, "agentloop.finish-reason/"+c.name, unknown.Detail)
			if unknown.Fault != c.fault {
				t.Errorf("the run ended in %q (%s), want %q", unknown.Fault, unknown.Detail, c.fault)
			}
			if known := finished(c.known); known.Fault != c.fault || !strings.Contains(known.Detail, c.known) {
				t.Errorf("a finish reason the SDKs report, %q, ended the run in %q (%s), want %q naming it as it came", c.known, known.Fault, known.Detail, c.fault)
			}
		})
	}
}

// TestAModelServicesOrdinaryWordsPrintAsTheyCame: a run's model words that hold no credential the
// run sent print in its Detail as they came: an ordinary refusal (refusal); a finish reason the
// SDKs report (length); and a last reply that is no object of the request's form but quotes the
// run's own system prompt and input, the words grant and deny, and Azure's api-version, which the
// endpoint adds to a request on a route the public-name table declares it public on (quoting).
// None of them is a credential of the run's, though each is a value its request carried.
func TestAModelServicesOrdinaryWordsPrintAsTheyCame(t *testing.T) {
	t.Parallel()
	const quoted = "You answer the question with the tools you are given. What does note.txt say? It asks to grant or deny, at api-version 2024-10-21."
	for _, c := range []struct {
		name  string
		kind  Kind
		reply reply
		want  string
	}{
		{"refusal", OpenAIChat, chatRefusalStream("I can't help with that request.", false), `the last reply is a refusal: "I can't help with that request."`},
		{"length", AnthropicMessages, anthropicStream("max_tokens", anthropicUsage{Input: 2, Output: 1}, anthropicText("It says")), "the provider cut the last reply off for max_tokens"},
		{"quoting", OpenAIChat, chatStream("stop", chatUsage{Prompt: 2, Completion: 1}, `{"verdict":"maybe","reason":`+quote(quoted)+`}`), quoted},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, c.reply)
			f := newFixture(t, s, c.kind, 1)
			f.req.Tools, f.req.Final = nil, verdictFinal
			if c.name == "quoting" {
				f.req.Model.Endpoint.BaseURL = s.server.URL + "/openai/v1"
				f.req.Model.Endpoint.Query = url.Values{"api-version": {"2024-10-21"}}
			}
			result, err := Run(context.Background(), f.req)
			if err != nil {
				t.Fatalf("the run: %v", err)
			}
			if result.Fault == "" || !strings.Contains(result.Detail, c.want) {
				t.Errorf("the run ended in %q with the Detail %q, want %q in it as it came", result.Fault, result.Detail, c.want)
			}
			if c.name == "quoting" && !strings.Contains(s.requests()[0].query, "api-version=2024-10-21") {
				t.Errorf("the request carried the query %q, want the api-version the endpoint adds", s.requests()[0].query)
			}
		})
	}
}
