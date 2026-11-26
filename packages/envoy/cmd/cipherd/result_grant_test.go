// result_grant_test.go drives the REAL compiled broker's result grants: a CI run's token traded
// at the token endpoint for an airlock placeholder, on a fresh schema, with the results profile
// DISPATCH_RESULTS_TOKEN, a proxy block of kind dpop, in BROKER_PROFILES_FILE. The helpers are
// token_exchange_test.go's and main_test.go's.
package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/dpopproof"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// resultsBoundHost is the host the proxy block of the profile DISPATCH_RESULTS_TOKEN binds in
// these tests, standing in for Dispatch's public host.
const resultsBoundHost = "dispatch.ci-results.test"

// resultsProfiles is the profiles file of a broker whose result entry's grants are held to
// DISPATCH_RESULTS_TOKEN: a profile of a name and a proxy block alone, of kind dpop, bound to
// resultsBoundHost, carried in Authorization alone, allowing POST under the results routes' two
// prefixes.
const resultsProfiles = `{"profiles": [{"name": "DISPATCH_RESULTS_TOKEN", "proxy": {"hosts": ["` + resultsBoundHost + `"], "headers": ["Authorization"],
	"allow": [{"method": "POST", "pathPrefix": "/api/v1/projects/"}, {"method": "POST", "pathPrefix": "/api/v1/issues/"}], "kind": "dpop"}}]}`

// resultsUpstream is the private destination resultsBoundHost's requests go to, as
// BROKER_PROXY_PRIVATE_UPSTREAM names it: the broker starts with the entry DISPATCH_RESULTS_TOKEN
// only beside the destination of its one host (config.ProxyMintedRules). The URL names the bound
// host itself, so a test that writes sends the destination's requests where
// BROKER_AIRLOCK_TEST_RESOLVE puts that host (resultsDispatch.env), over TLS checked against its
// stand-in's certificate; a test that only trades a token dials nothing.
const resultsUpstream = "BROKER_PROXY_PRIVATE_UPSTREAM=" + resultsBoundHost + "=https://" + resultsBoundHost

// resultsExchangeEnv is exchangeEnv with the result entry of resultsWorkflow and the airlock its
// grants need: the profiles file above, its private destination, the proxy, its certificate
// authority and a port of its own.
func resultsExchangeEnv(t *testing.T, databaseURL string, cluster, github *oidctest.Issuer, accessSeconds int) []string {
	t.Helper()
	env := append(exchangeEnv(t, databaseURL, cluster, github, accessSeconds), airlockEnv(t)...)
	return append(env,
		"BROKER_EXCHANGE_RESULT_WORKFLOWS="+exchangeRepository+"="+resultsWorkflow+"=refs/heads/*="+project+"="+person,
		"BROKER_PROFILES_FILE="+profilesFile(t, resultsProfiles), resultsUpstream)
}

// askedForThePlaceholder is form asking for the airlock placeholder.
func askedForThePlaceholder(form url.Values) url.Values {
	return with(form, "requested_token_type", oauthwire.TokenTypeProxyPlaceholder)
}

// postTokenAt posts form to the broker at addr's token endpoint with a DPoP proof of a fresh key
// for publicURL's token endpoint, the URL the broker checks a proof's htu against, and answers the
// status and the JSON body.
func postTokenAt(t *testing.T, addr, publicURL string, form url.Values) (int, map[string]any) {
	t.Helper()
	key := proofKey(t)
	compact, err := dpopproof.Sign(key, http.MethodPost, publicURL+"/v1/oauth/token", "", "")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", formContentType)
	request.Header.Set("DPoP", compact)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode the token answer: %v", err)
	}
	return response.StatusCode, body
}

// TestAPlaceholderExchangeNeedsNoRouteToTheBrokersOwnPublicURL: a broker whose BROKER_PUBLIC_URL
// names a host no resolver answers, and which reports its one result entry at boot, trades a
// result entry's run for a placeholder through its API port: the trade never calls the broker's
// own public URL.
func TestAPlaceholderExchangeNeedsNoRouteToTheBrokersOwnPublicURL(t *testing.T) {
	const publicURL = "https://secrets.unreachable.test"
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	githubKey := github.PublishKey(t, "github-key")
	broker := startBroker(t, brokerOptions{Env: append(resultsExchangeEnv(t, databaseURL, cluster, github, longAccessSeconds), "BROKER_PUBLIC_URL="+publicURL)})
	broker.AwaitRecord(t, "token exchange", map[string]any{"state": "on", "github": true, "result_workflows": 1})
	status, body := postTokenAt(t, broker.Addr, publicURL, askedForThePlaceholder(runForm(mintRunToken(t, github, githubKey, resultsWorkflow, resultsBranch, "7"), "")))
	held, _ := body["access_token"].(string)
	if status != http.StatusOK || !strings.HasPrefix(held, placeholder.MintedPrefix(oauthwire.AirlockResultsWord)) || body["issued_token_type"] != oauthwire.TokenTypeProxyPlaceholder {
		t.Fatalf("the placeholder exchange with BROKER_PUBLIC_URL=%s = %d %v, want 200 with a result placeholder", publicURL, status, body)
	}
}
