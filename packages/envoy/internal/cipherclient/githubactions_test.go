// packages/envoy/internal/cipherclient/githubactions_test.go
//
// A GitHub Actions run's own identity, against a stub GitHub that answers what a real run was
// answered (testdata/github-actions-token.json): the job asks GitHub for its OIDC token for the
// broker's audience with the request token GitHub gave it, asks again for every trade, and trades
// the token GitHub answered. The request token travels only to GitHub, over https or to this
// machine's loopback, and never to a host a redirect names; no refusal prints it, nor anything
// GitHub answered.

package cipherclient

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/testport"
)

// actionsRequestToken is the request token GitHub gave the job in these rows.
const actionsRequestToken = "example-actions-request-token"

// actionsFixture is testdata/github-actions-token.json, which packages/cipher-client's suite reads
// too.
type actionsFixture struct {
	RequestURL struct {
		QueryKeys []string `json:"query_keys"`
	} `json:"request_url"`
	Audience string `json:"audience"`
	Answer   struct {
		Status      int               `json:"status"`
		ContentType string            `json:"content_type"`
		Body        map[string]string `json:"body"`
	} `json:"answer"`
	JWT struct {
		Header map[string]string `json:"header"`
		Claims map[string]any    `json:"claims"`
	} `json:"jwt"`
	Refused struct {
		RequestToken string          `json:"request_token"`
		Status       int             `json:"status"`
		ContentType  string          `json:"content_type"`
		Body         json.RawMessage `json:"body"`
	} `json:"refused"`
}

func readActionsFixture(t *testing.T) actionsFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/github-actions-token.json")
	if err != nil {
		t.Fatalf("read the GitHub fixture: %v", err)
	}
	var fixture actionsFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("read the GitHub fixture: %v", err)
	}
	if fixture.Answer.Body["value"] != "${jwt}" || len(fixture.RequestURL.QueryKeys) == 0 {
		t.Fatalf("the GitHub fixture's answer is %v with the query keys %v; want the ${jwt} placeholder and the URL's own keys", fixture.Answer.Body, fixture.RequestURL.QueryKeys)
	}
	return fixture
}

// runTokenKey signs every run token of this binary: one RSA key, as GitHub signs with RS256.
var runTokenKey = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })

// signRunToken is a token GitHub could have answered: the fixture's header and claims, with a jti
// of its own, signed RS256. It answers an error rather than failing a test, since the stub calls
// it from its handler's goroutine.
func signRunToken(fixture actionsFixture) (string, error) {
	key, err := runTokenKey()
	if err != nil {
		return "", err
	}
	claims := maps.Clone(fixture.JWT.Claims)
	claims["jti"] = rand.Text()
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.SignatureAlgorithm(fixture.JWT.Header["alg"]), Key: jose.JSONWebKey{Key: key, KeyID: fixture.JWT.Header["kid"]}},
		(&jose.SignerOptions{}).WithType(jose.ContentType(fixture.JWT.Header["typ"])).WithHeader("x5t", fixture.JWT.Header["x5t"]),
	)
	if err != nil {
		return "", err
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return signed.CompactSerialize()
}

// actionsRequest is one request the stub GitHub received, as it received it.
type actionsRequest struct {
	Method, Path, Authorization string
	Query                       url.Values
}

// stubGitHub is GitHub's OIDC token endpoint for one job: to the job's request token it answers a
// token newly signed from the fixture, as GitHub answers every request with a new one, and to any
// other the fixture's refusal. It keeps every request and every token it answered.
type stubGitHub struct {
	requestURL string
	mu         sync.Mutex
	requests   []actionsRequest
	answered   []string
}

func newStubGitHub(t *testing.T, fixture actionsFixture) *stubGitHub {
	t.Helper()
	stub := &stubGitHub{}
	base := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		stub.requests = append(stub.requests, actionsRequest{
			Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Query: r.URL.Query(),
		})
		if r.Header.Get("Authorization") != "bearer "+actionsRequestToken {
			w.Header().Set("Content-Type", fixture.Refused.ContentType)
			w.WriteHeader(fixture.Refused.Status)
			_, _ = w.Write(fixture.Refused.Body)
			return
		}
		token, err := signRunToken(fixture)
		if err != nil {
			t.Errorf("sign a run token: %v", err)
			http.Error(w, "no token signed", http.StatusInternalServerError)
			return
		}
		stub.answered = append(stub.answered, token)
		// The fixture's own answer, its placeholder replaced by the token.
		body := maps.Clone(fixture.Answer.Body)
		for member, value := range body {
			if value == "${jwt}" {
				body[member] = token
			}
		}
		w.Header().Set("Content-Type", fixture.Answer.ContentType)
		w.WriteHeader(fixture.Answer.Status)
		_ = json.NewEncoder(w).Encode(body)
	})
	stub.requestURL = base + "/idtoken?" + actionsQuery(fixture).Encode()
	return stub
}

// actionsQuery is the query GitHub writes into ACTIONS_ID_TOKEN_REQUEST_URL: the fixture's keys,
// each with a value of this test's own.
func actionsQuery(fixture actionsFixture) url.Values {
	query := url.Values{}
	for _, key := range fixture.RequestURL.QueryKeys {
		query.Set(key, "example-"+key)
	}
	return query
}

func (s *stubGitHub) seen() ([]actionsRequest, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests), slices.Clone(s.answered)
}

// actionsIdentity is the identity of a job GitHub gave requestToken, whose request URL is
// requestURL, for the fixture's audience.
func actionsIdentity(fixture actionsFixture, requestURL, requestToken string) *GitHubActionsIdentity {
	return &GitHubActionsIdentity{RequestURL: requestURL, RequestToken: requestToken, Audience: fixture.Audience, HTTP: HTTPClient()}
}

// TestGitHubActionsIdentityAsksForItsAudienceWithTheRequestToken: the job asks GitHub once per
// trade, at the URL GitHub gave it with that URL's own query kept and the audience added to it,
// with the request token as its bearer, and the broker is shown the token GitHub answered as a
// workflow run's.
func TestGitHubActionsIdentityAsksForItsAudienceWithTheRequestToken(t *testing.T) {
	fixture := readActionsFixture(t)
	github := newStubGitHub(t, fixture)
	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
		return 200, map[string]any{
			"acc" + "ess_token": "dispatch-token-for-the-run", "token_type": "DPoP",
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "expires_in": 900,
		}
	})
	client, err := New(Config{
		BrokerURL: broker.Address(), Key: testKey(t),
		Subject: actionsIdentity(fixture, github.requestURL, actionsRequestToken),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	token, err := client.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token.AccessToken != "dispatch-token-for-the-run" {
		t.Errorf("the client held %q, want the token the broker answered", token.AccessToken)
	}

	requests, answered := github.seen()
	if len(requests) != 1 || len(answered) != 1 {
		t.Fatalf("GitHub was asked %d times and answered %d tokens, want one of each", len(requests), len(answered))
	}
	asked := requests[0]
	if asked.Method != http.MethodGet || asked.Path != "/idtoken" {
		t.Errorf("GitHub was asked %s %s, want GET at the request URL's own path", asked.Method, asked.Path)
	}
	want := actionsQuery(fixture)
	want.Set("audience", fixture.Audience)
	if asked.Query.Encode() != want.Encode() {
		t.Errorf("GitHub was asked with the query %q, want the request URL's own with the audience added: %q", asked.Query.Encode(), want.Encode())
	}
	if asked.Authorization != "bearer "+actionsRequestToken {
		t.Errorf("GitHub was asked with Authorization %q, want the job's request token as its bearer", asked.Authorization)
	}

	trades := broker.Requests()
	if len(trades) != 1 {
		t.Fatalf("the broker saw %d trades, want one", len(trades))
	}
	if got := trades[0].Form.Get("subject_token"); got != answered[0] {
		t.Errorf("the trade carried the subject token %q, want the one GitHub answered", got)
	}
	if got := trades[0].Form.Get("subject_token_type"); got != oauthwire.SubjectTokenTypeGitHub {
		t.Errorf("the trade presented the run's token as %q, want %q", got, oauthwire.SubjectTokenTypeGitHub)
	}
}

// TestGitHubActionsIdentityAsksAgainForEveryToken: the broker takes a GitHub token once, so every
// token the identity answers is one GitHub has just answered, and none is kept for the next.
func TestGitHubActionsIdentityAsksAgainForEveryToken(t *testing.T) {
	fixture := readActionsFixture(t)
	github := newStubGitHub(t, fixture)
	identity := actionsIdentity(fixture, github.requestURL, actionsRequestToken)
	var got []string
	for range 2 {
		token, tokenType, err := identity.Token(context.Background())
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if tokenType != oauthwire.SubjectTokenTypeGitHub {
			t.Errorf("the token type is %q, want %q", tokenType, oauthwire.SubjectTokenTypeGitHub)
		}
		got = append(got, token)
	}
	requests, answered := github.seen()
	if len(requests) != 2 {
		t.Fatalf("GitHub was asked %d times for two tokens, want twice", len(requests))
	}
	if !slices.Equal(got, answered) || got[0] == got[1] {
		t.Errorf("the identity answered %q, want each token GitHub answered, in turn: %q", got, answered)
	}
}

// TestGitHubActionsIdentityRefusalNamesItsStatusAndNothingItWasSent: GitHub refuses a request
// token with the answer a real run was given. The error names the request URL by its variable and
// GitHub's status, and holds neither the request token nor any text of GitHub's answer; and the
// broker is asked nothing. A request that got no answer names its class, and not the URL.
func TestGitHubActionsIdentityRefusalNamesItsStatusAndNothingItWasSent(t *testing.T) {
	fixture := readActionsFixture(t)
	github := newStubGitHub(t, fixture)
	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": "never", "token_type": "DPoP", "expires_in": 60}
	})
	wrong := fixture.Refused.RequestToken + "-" + printedSecret
	client, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t), Subject: actionsIdentity(fixture, github.requestURL, wrong)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Token(context.Background())
	if err == nil {
		t.Fatal("the trade went ahead with a request token GitHub refused")
	}
	namesAll(t, "the refusal", err.Error(), ActionsIDTokenRequestURLVar, "HTTP 401")
	holdsNoSecret(t, "the refusal", err.Error())
	var answer map[string]any
	if err := json.Unmarshal(fixture.Refused.Body, &answer); err != nil {
		t.Fatal(err)
	}
	for _, said := range []string{answer["source"].(string), answer["errorMessage"].(string), "malformed", "statusCode"} {
		if strings.Contains(err.Error(), said) {
			t.Errorf("the refusal %q holds %q, which GitHub's answer said", err, said)
		}
	}
	if requests, _ := github.seen(); len(requests) != 1 {
		t.Errorf("GitHub was asked %d times, want once", len(requests))
	}
	if n := len(broker.Requests()); n != 0 {
		t.Errorf("the broker saw %d trades with no run token to show it, want none", n)
	}

	lost := actionsIdentity(fixture, "http://"+testport.Refused(t)+"/idtoken?sig="+printedSecret, actionsRequestToken)
	_, _, err = lost.Token(context.Background())
	if err == nil {
		t.Fatal("Token answered with no GitHub to ask")
	}
	namesAll(t, "the failed request", err.Error(), ActionsIDTokenRequestURLVar, "connection refused")
	holdsNoSecret(t, "the failed request", err.Error())
}

// TestGitHubActionsIdentityFollowsNoRedirect: the request carries the job's request token, so an
// answer that names another place to ask is the refusal it is, and that place is asked nothing.
func TestGitHubActionsIdentityFollowsNoRedirect(t *testing.T) {
	fixture := readActionsFixture(t)
	for _, status := range []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			elsewhere := newStubGitHub(t, fixture)
			var asked atomic.Int64
			moving := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
				asked.Add(1)
				http.Redirect(w, r, elsewhere.requestURL, status)
			})
			_, _, err := actionsIdentity(fixture, moving+"/idtoken?"+actionsQuery(fixture).Encode(), actionsRequestToken).Token(context.Background())
			if err == nil {
				t.Fatal("Token took a redirect for a token")
			}
			namesAll(t, "the refusal", err.Error(), ActionsIDTokenRequestURLVar, "HTTP "+strconv.Itoa(status))
			if n := asked.Load(); n != 1 {
				t.Errorf("the request URL was asked %d times, want once", n)
			}
			if requests, _ := elsewhere.seen(); len(requests) != 0 {
				t.Errorf("the redirect's target was asked %d times, want none", len(requests))
			}
		})
	}
}

// TestGitHubActionsIdentityRefusesAPlainHTTPRequestURLOffLoopback: the request token would travel
// in the clear to a plain-http URL, so a request URL that is not https is refused before anything
// is sent, unless it is on this machine's loopback, as a stub GitHub is. The refusal names the
// variable, and none of its value.
func TestGitHubActionsIdentityRefusesAPlainHTTPRequestURLOffLoopback(t *testing.T) {
	fixture := readActionsFixture(t)
	var sent atomic.Int64
	recording := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent.Add(1)
		return nil, context.Canceled
	})}
	for _, requestURL := range []string{
		"http://token.actions.example/idtoken?api-version=" + printedSecret,
		"http://127.0.0.1.example/idtoken?api-version=2.0",
		"http://localhost.example/idtoken?api-version=2.0",
		"ftp://token.actions.example/idtoken?api-version=2.0",
		"token.actions.example/idtoken?api-version=" + printedSecret,
		"http://[::1/idtoken?api-version=" + printedSecret,
	} {
		t.Run(requestURL, func(t *testing.T) {
			identity := actionsIdentity(fixture, requestURL, actionsRequestToken)
			identity.HTTP = recording
			_, _, err := identity.Token(context.Background())
			if err == nil {
				t.Fatal("Token asked a request URL that is not https")
			}
			namesAll(t, "the refusal", err.Error(), ActionsIDTokenRequestURLVar, "https")
			holdsNoSecret(t, "the refusal", err.Error())
			if n := sent.Load(); n != 0 {
				t.Errorf("%d requests were sent, want none", n)
			}
		})
	}
	// Control: plain http to this machine's loopback is asked.
	github := newStubGitHub(t, fixture)
	if _, _, err := actionsIdentity(fixture, github.requestURL, actionsRequestToken).Token(context.Background()); err != nil {
		t.Fatalf("Token at %s: %v; want a loopback request URL asked", github.requestURL, err)
	}
}

// TestGitHubActionsIdentityRefusesAnAnswerWithNoValue: a 2xx that holds no token is a refusal
// before the broker is asked, and an answer that is not JSON is named without its text, which can
// be a token.
func TestGitHubActionsIdentityRefusesAnAnswerWithNoValue(t *testing.T) {
	fixture := readActionsFixture(t)
	for _, row := range []struct{ name, body, says string }{
		{"no value", `{}`, "no value"},
		{"an empty value", `{"value":""}`, "no value"},
		{"no JSON", printedSecret, "not the JSON"},
	} {
		t.Run(row.name, func(t *testing.T) {
			github := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", fixture.Answer.ContentType)
				_, _ = w.Write([]byte(row.body))
			})
			_, _, err := actionsIdentity(fixture, github+"/idtoken?"+actionsQuery(fixture).Encode(), actionsRequestToken).Token(context.Background())
			if err == nil {
				t.Fatalf("Token took %s for a token", row.body)
			}
			namesAll(t, "the refusal", err.Error(), ActionsIDTokenRequestURLVar, row.says)
			holdsNoSecret(t, "the refusal", err.Error())
		})
	}
}
