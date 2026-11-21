// result_grant_test.go drives the REAL compiled broker's result grants: a CI run's token traded
// at the token endpoint for an airlock placeholder, on a fresh schema, with the airlock's rules
// entry DISPATCH_RESULTS_TOKEN of kind dpop. The helpers are token_exchange_test.go's and
// main_test.go's.
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

// resultsBoundHost is the host the rules entry DISPATCH_RESULTS_TOKEN binds in these tests,
// standing in for Dispatch's public host.
const resultsBoundHost = "dispatch.ci-results.test"

// resultsRules is the airlock rules file of a broker whose result entry's grants are held to
// DISPATCH_RESULTS_TOKEN: kind dpop, bound to resultsBoundHost, carried in Authorization alone,
// allowing POST under the results routes' two prefixes.
const resultsRules = `{"secrets": [{"name": "DISPATCH_RESULTS_TOKEN", "hosts": ["` + resultsBoundHost + `"], "headers": ["Authorization"],
	"allow": [{"method": "POST", "pathPrefix": "/api/v1/projects/"}, {"method": "POST", "pathPrefix": "/api/v1/issues/"}], "kind": "dpop"}]}`

// resultsExchangeEnv is exchangeEnv with the result entry of resultsWorkflow and the airlock its
// grants need: the rules file above, the proxy, its certificate authority and a port of its own.
func resultsExchangeEnv(t *testing.T, databaseURL string, cluster, github *oidctest.Issuer, accessSeconds int) []string {
	t.Helper()
	env := append(exchangeEnv(t, databaseURL, cluster, github, accessSeconds), airlockEnv(t)...)
	return append(env,
		"BROKER_EXCHANGE_RESULT_WORKFLOWS="+exchangeRepository+"="+resultsWorkflow+"=refs/heads/*="+project+"="+person,
		"BROKER_AIRLOCK_RULES_FILE="+airlockRulesFile(t, resultsRules))
}

// askedForThePlaceholder is form asking for the airlock placeholder.
func askedForThePlaceholder(form url.Values) url.Values {
	return with(form, "requested_token_type", oauthwire.TokenTypeAirlockPlaceholder)
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
	if status != http.StatusOK || !strings.HasPrefix(held, placeholder.ResultPrefix) || body["issued_token_type"] != oauthwire.TokenTypeAirlockPlaceholder {
		t.Fatalf("the placeholder exchange with BROKER_PUBLIC_URL=%s = %d %v, want 200 with a result placeholder", publicURL, status, body)
	}
}
