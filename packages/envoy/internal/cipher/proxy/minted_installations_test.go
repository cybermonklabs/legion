// packages/envoy/internal/cipher/proxy/minted_installations_test.go
// The proxy in front of a profile served from a github-installations secret: installation tokens
// a minter outside the broker minted ahead of any request. The test plays that minter: it mints
// the tokens at the GitHub stand-in with the App key, as the deployment's minter does at GitHub,
// and stores them. The broker behind the proxy holds no minter but the github-installations one,
// so a token it handed over that the stand-in knows is a token it read from the store.
package proxy

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// mintedSource is the github-installations secret GITHUB_READ is served from in these rows, of
// profileOperator at agent tier, whose github-owners tag names acme and ada, behind profileKind by
// its proxy tag and narrowed by the profile's own allow.
const mintedSource = "PROFILE_TOKENS"

// mintAhead mints, at the stand-in minting serves, a token of each of acme and ada at the
// permissions of readOnly, as the deployment's minter mints at GitHub, and answers the
// github-installations value that minter writes of them and the token it minted for ada.
func mintAhead(t *testing.T, minting *proxyrulestest.Service) (value, adaToken string) {
	t.Helper()
	permissions := map[string]string{"contents": "read", "metadata": "read"}
	minted, err := secrets.GitHubApp{Base: minting.URL()}.Mint(t.Context(), secrets.MintRequest{
		Name: "MINTER_APP_KEY", Source: "MINTER_APP_KEY", Type: policy.TypeGitHubApp, Value: policytest.TypeValue(policy.TypeGitHubApp),
		GitHub: &secrets.GitHubScope{Accounts: []string{"acme", "ada"}, Permissions: permissions, Value: githubname.ValueInstallations},
	})
	if err != nil {
		t.Fatalf("mint the tokens ahead, as the minter does: %v", err)
	}
	var release struct {
		Installations map[string]struct {
			InstallationID int64  `json:"installation_id"`
			Token          string `json:"token"`
			ExpiresAt      string `json:"expires_at"`
		} `json:"installations"`
	}
	if err := json.Unmarshal([]byte(minted.Value), &release); err != nil {
		t.Fatalf("read the tokens minted ahead: %v", err)
	}
	stored := map[string]any{}
	for account, one := range release.Installations {
		stored[account] = map[string]any{"installation_id": one.InstallationID, "token": one.Token, "expires_at": one.ExpiresAt,
			"permissions": permissions, "repository_selection": "all"}
	}
	encoded, err := json.Marshal(map[string]any{"shapeVersion": 1, "installations": stored})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded), release.Installations["ada"].Token
}

// TestTheProxySwapsInTheStoredTokenOfAMintedInstallationsProfile: a profile of a
// github-installations secret behind the proxy is released as placeholders, and the proxy, when it
// carries the grant's first call, swaps in the token the store holds for the target's account:
// the call reaches the stand-in with that token, which the stand-in knows, and the broker sends
// the stand-in no token request of its own. The proxy holds that token for later calls.
//
// The named revert: wire github-installations to the github-app minter (or hand the proxy no
// value of the type), and the proxy's read of the grant fails, so the call is refused.
func TestTheProxySwapsInTheStoredTokenOfAMintedInstallationsProfile(t *testing.T) {
	stand := proxyrulestest.NewGitHubStand(proxyrulestest.GitHubOptions{Installations: profileInstallations, DistinctTokens: true})
	minting, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatalf("start the GitHub stand-in: %v", err)
	}
	t.Cleanup(minting.Close)
	minting.AnswerWith(stand.Answer)
	value, adaToken := mintAhead(t, minting)
	before := len(minting.Requests())

	file := policytest.KindsFileOf(profileKindsJSON, `{"name": "`+profileName+`", "source": "`+mintedSource+`", "github": {"accounts": ["acme", "ada"], "permissions": `+readOnly+`, "value": "token"}, `+
		`"allow": [{"method": "GET", "pathPrefix": "/repos/"}]}`)
	st := storetest.Open(t)
	local := memory.New(profiledSource(mintedSource, policy.TypeGitHubInstallations, value))
	m := &requests.Machine{
		Store: st, Policy: policytest.ProfiledCurrent(t, local, file), Secrets: local,
		PendingTTL: 12 * time.Hour, Audience: profileAudience, Skew: time.Minute,
		Replay:          (&enroll.Service{Store: st}).Replay,
		Minters:         map[string]requests.Minter{policy.TypeGitHubInstallations: secrets.GitHubInstallations{}.Mint},
		AirlockProxyURL: "http://127.0.0.1:13381", ProxyRoots: func() string { return "-----BEGIN CERTIFICATE-----\nminted-rig\n-----END CERTIFICATE-----\n" },
	}
	m.Chain = requests.NewChainVerifier(st, profileAudience, time.Minute, m.Policy)
	grants := &rolledGrants{}
	grants.machine.Store(m)
	r := newRig(t, Options{Grants: grants})
	r.answerWith(stand.Answer)

	enrollmentID, key := ciphertest.InsertSession(t, st.Pool, profileOperator)
	signed, err := record.Sign(key, profileAudience, []record.AuthorizationDetail{{
		Type: record.KindAgentSecret, Identifier: profileName, Actions: []string{"inject"}, Locations: []string{"https://github.com/ada"},
	}}, "read the proofs repository", "", nil, time.Now())
	if err != nil {
		t.Fatalf("sign the request object: %v", err)
	}
	request, err := m.Create(t.Context(), enrollmentID, signed, "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want a grant at once", request, err)
	}
	release, err := m.Values(t.Context(), *request.GrantID, enrollmentID, requests.ReleaseCall{})
	if err != nil || release.Airlock == nil {
		t.Fatalf("Values = %+v, %v; want the profile behind the proxy", release, err)
	}
	if _, inline := release.Values[profileName]; inline {
		t.Fatalf("the release hands over %s's value itself; want it behind the proxy alone", profileName)
	}
	slots := release.Airlock.Names[profileName].Slots
	placeholder := slots[requests.SlotValue]
	if placeholder == "" || len(slots) != 1 {
		t.Fatalf("the release names %s's slots %v; want the one value slot", profileName, slots)
	}

	client := r.client(release.Airlock.ProxyPassword)
	from := len(r.requests())
	for range 2 {
		answer, body := r.get(client, "https://"+boundHost+profileRepository, "Authorization", "Bearer "+placeholder)
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("GET %s through the proxy = %d %s; want 200, the stand-in reading a token it minted", profileRepository, answer.StatusCode, body)
		}
	}
	carried := r.requests()[from:]
	if len(carried) != 2 || !slices.ContainsFunc(carried, func(seen seenRequest) bool { return seen.Header.Get("Authorization") == "Bearer "+adaToken }) {
		t.Fatalf("the service behind the bound host was sent %+v; want both calls carrying ada's stored token", carried)
	}
	for _, seen := range minting.Requests()[before:] {
		if seen.Method == http.MethodPost && strings.HasSuffix(seen.Path, "/access_tokens") {
			t.Fatalf("the broker asked the stand-in for a token (%s %s); want it to hand over the stored one alone", seen.Method, seen.Path)
		}
	}
}
