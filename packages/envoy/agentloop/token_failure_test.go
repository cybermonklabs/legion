package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// tokenRefusal is the caller's own words for a token it could not read, as a key store or a mint
// that refused the caller says them.
const tokenRefusal = "the key store refused the caller's read of the model key"

// unreadEndpoint is an endpoint of kind whose Token fails with tokenRefusal, at a stand-in that
// counts the calls it gets. With user its address carries user information, as one a person
// configured with a key in it does, which net/http's *url.Error keeps in the URL it names; the
// Azure provider refuses such an address, so its endpoint carries none.
func unreadEndpoint(t *testing.T, kind Kind, user bool) (Endpoint, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	base := server.URL
	if user {
		base = "http://" + redacttest.User + "@" + strings.TrimPrefix(base, "http://")
	}
	if kind == OpenAIResponses || kind == OpenAIChat {
		base += "/v1"
	}
	return Endpoint{Kind: kind, BaseURL: base, Token: func(context.Context) (string, error) {
		return "", errors.New(tokenRefusal)
	}}, &calls
}

// TestATokenTheCallerCouldNotReadIsNamedInItsOwnWords: the endpoint's Token, the caller's own
// function, fails, so the call never goes out. A run's Detail on each protocol, the Azure
// provider's credential included, and a decision's name that failure in the caller's words after
// "reading the <kind> token", since no server has answered and none of those words is another
// party's: the judge's audit row reads why its mint was refused there. The Detail holds nothing
// of net/http's error or the SDK's around those words, whose URL keeps the user information of
// the endpoint's address. The twin shows net/http's error, as the SDK hands it on in a run and as
// the client returns it in a decision, names that user. Captures agentloop.token-failure/<kind>.
func TestATokenTheCallerCouldNotReadIsNamedInItsOwnWords(t *testing.T) {
	t.Parallel()
	type tokenCase struct {
		kind Kind
		user bool
	}
	cases := []tokenCase{{AnthropicMessages, true}, {OpenAIResponses, true}, {OpenAIChat, true}, {AzureAnthropic, false}, {SystemOne, true}}
	request := func(t *testing.T, endpoint Endpoint) Request {
		return Request{
			Model:  Model{Endpoint: endpoint, ID: "standin-model", Price: anthropicPrice},
			Input:  []Message{{Role: User, Blocks: []Block{{Kind: TextBlock, Text: "What does note.txt say?"}}}},
			Bounds: Bounds{Time: runnerTime(t), Calls: 1, OutputTokens: 100},
		}
	}
	decision := func(t *testing.T, endpoint Endpoint) DecideRequest {
		return DecideRequest{
			Model:     Model{Endpoint: endpoint, ID: "systemone-standin", Price: systemOnePrice},
			State:     json.RawMessage(`{"note":"The note says plum."}`),
			Questions: decideQuestions(),
			Bounds:    Bounds{Time: runnerTime(t)},
		}
	}
	t.Run("twin", func(t *testing.T) {
		t.Parallel()
		for _, c := range cases {
			if !c.user {
				continue
			}
			endpoint, _ := unreadEndpoint(t, c.kind, true)
			var err error
			if c.kind == SystemOne {
				post, perr := http.NewRequest(http.MethodPost, endpoint.BaseURL+decidePath, strings.NewReader("{}"))
				if perr != nil {
					t.Fatal(perr)
				}
				var response *http.Response
				if response, err = newModelTransport(endpoint).client().Do(post); err == nil {
					_ = response.Body.Close()
				}
			} else {
				err = sdkStreamError(t, request(t, endpoint))
			}
			if err == nil {
				t.Fatalf("%s: the call ended with no error: the row cannot see one", c.kind)
			}
			if !strings.Contains(err.Error(), redacttest.User) || !strings.Contains(err.Error(), tokenRefusal) {
				t.Fatalf("%s: the client's own error does not name the address's user beside the caller's words, so the row cannot see a leak: %q", c.kind, err)
			}
		}
	})
	for _, c := range cases {
		t.Run(string(c.kind), func(t *testing.T) {
			t.Parallel()
			endpoint, calls := unreadEndpoint(t, c.kind, c.user)
			var fault Fault
			var detail string
			if c.kind == SystemOne {
				result, err := Decide(context.Background(), decision(t, endpoint))
				if err != nil {
					t.Fatal(err)
				}
				fault, detail = result.Fault, result.Detail
			} else {
				result, err := Run(context.Background(), request(t, endpoint))
				if err != nil {
					t.Fatal(err)
				}
				fault, detail = result.Fault, result.Detail
			}
			if c.user {
				redacttest.RequireNoPiece(t, "agentloop.token-failure/"+string(c.kind), detail, redacttest.User)
			}
			if want := "reading the " + string(c.kind) + " token: " + tokenRefusal; fault != FaultProviderError || detail != want {
				t.Errorf("fault %q, detail %q; want %q, %q: the caller's words for its token alone", fault, detail, FaultProviderError, want)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("the endpoint got %d calls; want none without a token", n)
			}
		})
	}
}
