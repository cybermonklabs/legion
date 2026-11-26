// packages/envoy/cmd/dev-proxy-stands/anthropic_test.go
//
// The anthropic kind's stand-ins as the broker meets them: the real compiled binary, and the
// broker's own anthropic-federation minter (secrets.AnthropicFederation, with the identity token
// reader cmd/cipherd gives it) pointed at the metadata, iam and anthropic-token stands, as
// dev-airlock.sh points a broker at them. What each stand answers a request the minter does not
// make is the rows below it.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/googleidentity"
)

// The account and the stored value of the one anthropic-federation secret these rows mint, in the
// shape secrets.ParseValue takes. Nothing here is a credential.
const (
	standsAnthropicAccount = "anthropic-n@example-project.iam.gserviceaccount.com"
	standsAnthropicSecret  = "AIRLOCK_ANTHROPIC_N"
	standsAnthropicValue   = `{"organization_id":"00000000-0000-4000-8000-00000000000a","federation_rule_id":"fdrl_01standinrulen","service_account_id":"svac_01standintargetn","service_account":"` + standsAnthropicAccount + `","workspace_id":"wrkspc_01standinn"}`
)

// TestTheBrokersAnthropicMinterMintsAtTheStands: the broker's minter, given the iam and
// anthropic-token stands and a GCE_METADATA_HOST naming the metadata stand, reads the pod's access
// token at the metadata stand, spends it at the iam stand for the bound account's identity token
// for Anthropic's audience, and trades that token at the exchange for an sk-ant-oat01- token of an
// hour. The two lists the report names hold the identity token, with its account and audience, and
// the token, with its lifetime; the exchange's record holds the stored value's four ids and the
// identity token as its assertion. Once a check sets an expires_in of 3601 the next mint is refused
// by the minter, which is the refusal the anthropic kind's check drives, and the exchange did
// answer it, so the refusal is the broker's.
func TestTheBrokersAnthropicMinterMintsAtTheStands(t *testing.T) {
	out := startStands(t, "--dir", t.TempDir())
	if out.AnthropicAnswerURL != out.RecordsURL+"/answers/anthropic" || out.AnthropicTokensURL != out.RecordsURL+"/tokens/anthropic" || out.IAMTokensURL != out.RecordsURL+"/tokens/iam" {
		t.Fatalf("the report names %q, %q and %q; want the three anthropic routes on the recording server at %s",
			out.AnthropicAnswerURL, out.AnthropicTokensURL, out.IAMTokensURL, out.RecordsURL)
	}
	t.Setenv(googleidentity.MetadataHostVar, out.Stands[standMetadata].Address)
	minter := &secrets.AnthropicFederation{
		Accounts:      map[string]string{standsAnthropicSecret: standsAnthropicAccount},
		IdentityToken: (&ciphertoken.GoogleAccounts{Audience: secrets.AnthropicAudience, IAMCredentialsURL: out.Stands[standIAM].URL}).Read,
		TokenURL:      out.Stands[standAnthropicToken].URL + "/v1/oauth/token",
	}
	mint := func() (secrets.Minted, error) {
		return minter.Mint(t.Context(), secrets.MintRequest{
			Name: standsAnthropicSecret, Source: standsAnthropicSecret, Type: "anthropic-federation", HumanTier: true, Value: standsAnthropicValue,
		})
	}
	asked := time.Now()
	minted, err := mint()
	if err != nil || !strings.HasPrefix(minted.Value, anthropicTokenPrefix) {
		t.Fatalf("the minter answered a value of %d bytes, %v; want an %s token from the exchange stand", len(minted.Value), err, anthropicTokenPrefix)
	}
	if life := minted.Expires.Sub(asked); life < 59*time.Minute || life > 61*time.Minute {
		t.Errorf("the minted token lives %s; want the hour the exchange stand answers until a check sets another", life)
	}

	var identities []identityToken
	getJSON(t, out.IAMTokensURL, &identities)
	if len(identities) != 1 || identities[0].Account != standsAnthropicAccount || identities[0].Audience != secrets.AnthropicAudience {
		t.Fatalf("the iam stand minted %+v; want one identity token of %s for %s", identities, standsAnthropicAccount, secrets.AnthropicAudience)
	}
	var tokens []anthropicToken
	getJSON(t, out.AnthropicTokensURL, &tokens)
	if len(tokens) != 1 || tokens[0].AccessToken != minted.Value || tokens[0].ExpiresIn != 3600 || tokens[0].ServiceAccountID != "svac_01standintargetn" {
		t.Fatalf("the exchange stand lists %d tokens; want the one the minter got, of 3600 seconds, for svac_01standintargetn", len(tokens))
	}
	exchanges := recordsOf(t, out.Stands[standAnthropicToken].Records)
	if len(exchanges) != 1 {
		t.Fatalf("the exchange stand recorded %d requests; want the one exchange", len(exchanges))
	}
	var body anthropicExchangeBody
	if err := json.Unmarshal([]byte(exchanges[0].Body), &body); err != nil {
		t.Fatalf("the exchange's record holds no JSON body: %v", err)
	}
	if body.Assertion != identities[0].Token || body.OrganizationID != "00000000-0000-4000-8000-00000000000a" || body.FederationRuleID != "fdrl_01standinrulen" ||
		body.ServiceAccountID != "svac_01standintargetn" || body.WorkspaceID != "wrkspc_01standinn" {
		t.Errorf("the exchange recorded organization %q, rule %q, target %q, workspace %q and an assertion that is the iam stand's token: %t; want the stored value's four ids and that token",
			body.OrganizationID, body.FederationRuleID, body.ServiceAccountID, body.WorkspaceID, body.Assertion == identities[0].Token)
	}
	iamCalls := recordsOf(t, out.Stands[standIAM].Records)
	if len(iamCalls) != 1 || iamCalls[0].Path != iamAccountPrefix+standsAnthropicAccount+iamAccountSuffix {
		t.Fatalf("the iam stand recorded %+v; want one generateIdToken of %s", iamCalls, standsAnthropicAccount)
	}

	if status, reply := setTheAnswer(t, out.AnthropicAnswerURL, `{"expires_in": 3601}`); status != http.StatusOK || !strings.Contains(reply, `"3601"`) {
		t.Fatalf("setting an expires_in of 3601 answered %d %s; want 200 naming it", status, reply)
	}
	if _, err := mint(); !errors.Is(err, secrets.ErrMintRefused) || !strings.Contains(err.Error(), "3601") {
		t.Fatalf("the mint after an expires_in of 3601 = %v; want the minter's refusal naming the lifetime", err)
	}
	getJSON(t, out.AnthropicTokensURL, &tokens)
	if len(tokens) != 2 || tokens[1].ExpiresIn != 3601 {
		t.Fatalf("the exchange stand lists %d tokens; want a second, of 3601 seconds, which the minter refused", len(tokens))
	}
}

// TestTheAnthropicStandsRefuseWhatTheirServicesRefuse: the metadata stand answers its access token
// only to a request with Metadata-Flavor: Google; the iam stand answers only a call whose bearer is
// that token, and only one that names an audience; the exchange refuses another grant type, a body
// without its four fields and an assertion the iam stand did not mint; and the answer route
// refuses a lifetime of zero, leaving the hour in place. Each is what the service it stands in for
// refuses, so a broker that skipped a step reads a refusal rather than a token.
func TestTheAnthropicStandsRefuseWhatTheirServicesRefuse(t *testing.T) {
	out := startStands(t, "--dir", t.TempDir())
	metadata := out.Stands[standMetadata].URL + metadataTokenPath
	if status, _ := askStand(t, http.MethodGet, metadata, nil, ""); status != http.StatusForbidden {
		t.Errorf("the metadata access token without Metadata-Flavor answered %d; want 403", status)
	}
	status, answer := askStand(t, http.MethodGet, metadata, map[string]string{"Metadata-Flavor": "Google"}, "")
	var access struct {
		AccessToken string `json:"access_token"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(answer), &access) != nil || access.AccessToken == "" {
		t.Fatalf("the metadata access token answered %d %q; want 200 and a token", status, answer)
	}
	generate := out.Stands[standIAM].URL + iamAccountPrefix + standsAnthropicAccount + iamAccountSuffix
	audience := `{"audience": "https://api.anthropic.com", "includeEmail": true}`
	if status, _ := askStand(t, http.MethodPost, generate, nil, audience); status != http.StatusUnauthorized {
		t.Errorf("generateIdToken with no bearer answered %d; want 401", status)
	}
	if status, _ := askStand(t, http.MethodPost, generate, map[string]string{"Authorization": "Bearer another"}, audience); status != http.StatusUnauthorized {
		t.Errorf("generateIdToken with another bearer answered %d; want 401", status)
	}
	bearer := map[string]string{"Authorization": "Bearer " + access.AccessToken}
	if status, _ := askStand(t, http.MethodPost, generate, bearer, `{"includeEmail": true}`); status != http.StatusBadRequest {
		t.Errorf("generateIdToken with no audience answered %d; want 400", status)
	}
	status, answer = askStand(t, http.MethodPost, generate, bearer, audience)
	var identity struct {
		Token string `json:"token"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(answer), &identity) != nil || strings.Count(identity.Token, ".") != 2 {
		t.Fatalf("generateIdToken with this run's bearer answered %d; want 200 and a JWT", status)
	}

	exchange := out.Stands[standAnthropicToken].URL + anthropicExchange
	body := func(grant, assertion string) string {
		raw, _ := json.Marshal(anthropicExchangeBody{GrantType: grant, Assertion: assertion, FederationRuleID: "fdrl_x", OrganizationID: "org", ServiceAccountID: "svac_x"})
		return string(raw)
	}
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"another grant type", body("client_credentials", identity.Token), http.StatusBadRequest},
		{"no federation rule", `{"grant_type":"` + anthropicJWTBearer + `","assertion":"` + identity.Token + `"}`, http.StatusBadRequest},
		{"an assertion the iam stand did not mint", body(anthropicJWTBearer, identity.Token+"x"), http.StatusUnauthorized},
		{"a body that is no JSON object", "grant_type=" + anthropicJWTBearer, http.StatusBadRequest},
	} {
		if status, reply := askStand(t, http.MethodPost, exchange, nil, c.body); status != c.want || strings.Contains(reply, anthropicTokenPrefix) {
			t.Errorf("an exchange with %s answered %d %s; want %d and no token", c.name, status, reply, c.want)
		}
	}
	if status, reply := setTheAnswer(t, out.AnthropicAnswerURL, `{"expires_in": 0}`); status != http.StatusBadRequest {
		t.Errorf("an expires_in of 0 was answered %d %s; want 400", status, reply)
	}
	if status, reply := setTheAnswer(t, out.AnthropicAnswerURL, `{"lifetime": 3601}`); status != http.StatusBadRequest {
		t.Errorf("an answer naming no expires_in was answered %d %s; want 400", status, reply)
	}
	status, answer = askStand(t, http.MethodPost, exchange, nil, body(anthropicJWTBearer, identity.Token))
	var token anthropicToken
	if status != http.StatusOK || json.Unmarshal([]byte(answer), &token) != nil || !strings.HasPrefix(token.AccessToken, anthropicTokenPrefix) || token.ExpiresIn != 3600 {
		t.Fatalf("an exchange of the iam stand's token answered %d, a token opening %s: %t, expires_in %d; want 200 and an hour's token, the refused lifetimes left out",
			status, anthropicTokenPrefix, strings.HasPrefix(token.AccessToken, anthropicTokenPrefix), token.ExpiresIn)
	}
}

// TestTheAnthropicStandIsTheAdminAPIUnderItsName: --host anthropic=api.anthropic.com puts the
// Admin API's name in the anthropic stand's certificate, so a command behind the airlock verifies
// it under that name as it verifies the real API, and the stand records what it was sent.
func TestTheAnthropicStandIsTheAdminAPIUnderItsName(t *testing.T) {
	out := startStands(t, "--dir", t.TempDir(), "--host", "anthropic=api.anthropic.com")
	stand := out.Stands[standAnthropic]
	if stand.CertFile == "" {
		t.Fatal("the anthropic stand wrote no certificate; want an HTTPS stand")
	}
	pem, err := os.ReadFile(out.CACertFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "api.anthropic.com", MinVersion: tls.VersionTLS12},
	}}
	request, err := http.NewRequest(http.MethodGet, stand.URL+"/v1/organizations/workspaces", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+anthropicTokenPrefix+"recorded")
	answer, err := client.Do(request)
	if err != nil {
		t.Fatalf("call the anthropic stand as api.anthropic.com: %v", err)
	}
	answer.Body.Close()
	records := recordsOf(t, stand.Records)
	if answer.StatusCode != http.StatusOK || len(records) != 1 || records[0].Path != "/v1/organizations/workspaces" || records[0].Header.Get("Authorization") != "Bearer "+anthropicTokenPrefix+"recorded" {
		t.Fatalf("the anthropic stand answered %d and recorded %+v; want 200 and the call as it was sent", answer.StatusCode, records)
	}
}

// getJSON decodes the JSON one GET of url answers into into.
func getJSON(t *testing.T, url string, into any) {
	t.Helper()
	status, body := askStand(t, http.MethodGet, url, nil, "")
	if status != http.StatusOK {
		t.Fatalf("GET %s answered %d %s; want 200", url, status, body)
	}
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("GET %s answered no JSON of its list: %v\n%s", url, err, body)
	}
}

// askStand makes one request with the headers and the body given and answers its status and body.
func askStand(t *testing.T, method, url string, headers map[string]string, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	answer, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer answer.Body.Close()
	raw, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return answer.StatusCode, strings.TrimSpace(string(raw))
}
