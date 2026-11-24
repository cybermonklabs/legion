// packages/envoy/internal/cipher/proxy/anthropic_kind_test.go
//
// The anthropic kind behind the proxy, end to end in one process: a real requests.Machine whose
// profiles file declares the kind as the spec writes it, two stored anthropic-federation secrets
// behind it by their proxy tag, the production minter (secrets.AnthropicFederation) reading each
// secret's Google identity token at a stand-in of IAM and trading it at a stand-in of Anthropic's
// token exchange, and the rig's stand-in service answering as api.anthropic.com. Secret N is
// narrowed by its one profile, which holds ANTHROPIC_ADMIN_HDO's three allow rows; secret K has no
// profile, so only the kind's allow bounds it. What the rows read is what each layer did: the
// policy's rules, the identity tokens IAM was asked for, the exchanges Anthropic was sent, what
// reached the API and what the broker logged.
package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// anthropicHost is the one host the anthropic kind binds, and anthropicTokenPrefix what every
// token Anthropic's exchange answers opens with: a search for it finds any such token.
const (
	anthropicHost        = "api.anthropic.com"
	anthropicTokenPrefix = "sk-ant-oat01-"
)

// anthropicKindJSON is the anthropic kind as the spec declares it: api.anthropic.com alone, the
// header Authorization, the way mint, the variable ANTHROPIC_ADMIN_TOKEN, a max_grant of an hour,
// and GET, POST and DELETE under /v1/organizations/ - the Admin API, and not the token exchange or
// the Messages API on the same host.
const anthropicKindJSON = `[{"name": "anthropic", "hosts": ["` + anthropicHost + `"], "headers": ["Authorization"], ` +
	`"variables": ["ANTHROPIC_ADMIN_TOKEN"], "ways": ["mint"], "max_grant": "1h", "allow": ` + adminRowsJSON + `}]`

// adminRowsJSON is ANTHROPIC_ADMIN_HDO's allow in the deployment repository, the three rows each
// Anthropic admin profile keeps as its own and the kind's allow repeats.
const adminRowsJSON = `[{"method": "GET", "pathPrefix": "/v1/organizations/"}, {"method": "POST", "pathPrefix": "/v1/organizations/"}, {"method": "DELETE", "pathPrefix": "/v1/organizations/"}]`

// adminRows is adminRowsJSON as the policy reads it.
var adminRows = []proxyrules.Allow{
	{Method: http.MethodGet, PathPrefix: "/v1/organizations/"},
	{Method: http.MethodPost, PathPrefix: "/v1/organizations/"},
	{Method: http.MethodDelete, PathPrefix: "/v1/organizations/"},
}

// federationSecret is one stored anthropic-federation secret of the rows: its name, the Google
// account BROKER_SERVICE_ACCOUNT_SECRETS binds to it, and the Anthropic ids of its federation rule,
// none of them a real organization's. narrowed is whether it carries proxy-narrowed=true and has
// its one profile.
type federationSecret struct {
	name, account, organization, rule, target, workspace string
	narrowed                                             bool
}

var (
	secretN = federationSecret{
		name: "ANTHROPIC_ADMIN_NARROWED", account: "anthropic-narrowed@example-project.iam.gserviceaccount.com",
		organization: "11111111-1111-4111-8111-111111111111", rule: "fdrl_01narrowed", target: "svac_01narrowed", workspace: "wrkspc_01narrowed",
		narrowed: true,
	}
	secretK = federationSecret{
		name: "ANTHROPIC_ADMIN_KIND", account: "anthropic-kind@example-project.iam.gserviceaccount.com",
		organization: "22222222-2222-4222-8222-222222222222", rule: "fdrl_01kind", target: "svac_01kind", workspace: "wrkspc_01kind",
	}
)

// stored is the secret as the store holds it: type anthropic-federation, of profileOperator at
// tier human, admin, a max-grant of an hour and its owner as its one requester
// (policytest.TypeSecret), the proxy tag anthropic, and proxy-narrowed=true for N.
func (f federationSecret) stored() awstest.LocalSecret {
	value, _ := json.Marshal(map[string]string{
		"organization_id": f.organization, "federation_rule_id": f.rule, "service_account_id": f.target,
		"service_account": f.account, "workspace_id": f.workspace,
	})
	s := policytest.Proxied(policytest.TypeSecret(f.name, profileOperator, policy.TierHuman, policy.TypeAnthropicFederation, string(value)), "anthropic")
	s.Tags[policy.TagAdmin] = policy.AdminTrue
	if f.narrowed {
		s = policytest.Narrowed(s)
	}
	return s
}

// exchangeStand is Anthropic's POST /v1/oauth/token: it keeps each exchange's JSON body, answers
// a token of its own for each, sk-ant-oat01- and the first block of the exchange's organization
// and its count, so which token reached a call says which exchange made it, and answers the
// lifetime a row sets, 3600 seconds until it sets another.
type exchangeStand struct {
	server   *httptest.Server
	mu       sync.Mutex
	asked    []map[string]any
	issued   []string
	lifetime int
}

func newExchangeStand(t *testing.T) *exchangeStand {
	t.Helper()
	s := &exchangeStand{lifetime: 3600}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/oauth/token" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		organization, _ := body["organization_id"].(string)
		s.mu.Lock()
		s.asked = append(s.asked, body)
		token := fmt.Sprintf("%s%.8s-%d", anthropicTokenPrefix, organization, len(s.asked))
		s.issued = append(s.issued, token)
		lifetime := s.lifetime
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": lifetime, "scope": "org:admin"})
	}))
	t.Cleanup(s.server.Close)
	return s
}

// exchanges is every exchange's body and the token answered to it, in order, as copies.
func (s *exchangeStand) exchanges() ([]map[string]any, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.asked...), append([]string(nil), s.issued...)
}

// answerLifetime makes every later exchange answer seconds as its token's expires_in.
func (s *exchangeStand) answerLifetime(seconds int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifetime = seconds
}

// anthropicRig is the rig with a broker behind its proxy that serves the two secrets under the
// anthropic kind, its stand-ins, and one live session of profileOperator, who asks for both and
// approves each grant.
type anthropicRig struct {
	*rig
	st           *store.Store
	machine      *requests.Machine
	iam          *ciphertokentest.IAMStand
	exchange     *exchangeStand
	enrollmentID string
	sign         func(name string) string
}

func newAnthropicRig(t *testing.T) *anthropicRig {
	t.Helper()
	// The pod's own access token, which generateIdToken is called with (ciphertoken's reader
	// reads it from the metadata server); IAM, which mints each bound account's identity token;
	// and the exchange.
	ciphertokentest.NewMetadataStand(t)
	k := &anthropicRig{st: storetest.Open(t), iam: ciphertokentest.NewIAMStand(t), exchange: newExchangeStand(t)}
	file := policytest.KindsFileOf(anthropicKindJSON, `{"name": "`+secretN.name+`", "allow": `+adminRowsJSON+`}`)
	local := awstest.NewLocal(secretN.stored(), secretK.stored())
	k.machine = &requests.Machine{
		Store: k.st, Policy: policytest.ProfiledCurrent(t, local, file), Secrets: policytest.Store(local),
		PendingTTL: 12 * time.Hour, Audience: profileAudience, Skew: time.Minute,
		Replay: (&enroll.Service{Store: k.st}).Replay,
		Minters: map[string]requests.Minter{policy.TypeAnthropicFederation: (&secrets.AnthropicFederation{
			Accounts:      map[string]string{secretN.name: secretN.account, secretK.name: secretK.account},
			IdentityToken: (&ciphertoken.GoogleAccounts{Audience: secrets.AnthropicAudience, IAMCredentialsURL: k.iam.Server.URL}).Read,
			TokenURL:      k.exchange.server.URL + "/v1/oauth/token",
		}).Mint},
		// Two strings the release carries to the session unread: the session here is the rig's
		// own client, which is pointed at the rig's proxy and trusts its authority.
		AirlockProxyURL: "http://127.0.0.1:13381", ProxyRoots: func() string { return "-----BEGIN CERTIFICATE-----\nanthropic-rig\n-----END CERTIFICATE-----\n" },
	}
	k.machine.Chain = requests.NewChainVerifier(k.st, profileAudience, time.Minute, k.machine.Policy)
	grants := &rolledGrants{}
	grants.machine.Store(k.machine)
	k.rig = newServedRig(t, []string{anthropicHost}, proxyrules.Rules{}, Options{Grants: grants})
	// The release's mint line is the request machine's own (slog's default logger), the proxy's
	// lines are the proxy's logger's: both go to the one capture, so one search reads every line
	// the broker wrote.
	logtest.SetDefault(t, k.logs)
	enrollmentID, key := ciphertest.InsertSession(t, k.st.Pool, profileOperator)
	k.enrollmentID = enrollmentID
	k.sign = func(name string) string {
		lifetime := 3600
		signed, err := record.Sign(key, profileAudience, []record.AuthorizationDetail{{
			Type: record.KindAgentSecret, Identifier: name, Actions: []string{"inject"},
		}}, "k47: read and change the organization's workspaces", "", &lifetime, time.Now())
		if err != nil {
			t.Fatalf("sign the request object for %s: %v", name, err)
		}
		return signed
	}
	return k
}

// grant asks for name, approves the request as the session's person, and releases the grant at
// once, as `agent-secrets run` does: it answers the grant's id, what the release names behind the
// proxy, and the placeholder of the name's one value slot.
func (k *anthropicRig) grant(t *testing.T, name string) (grantID string, released *requests.AirlockRelease, placeholder string) {
	t.Helper()
	request, err := k.machine.Create(t.Context(), k.enrollmentID, k.sign(name), "")
	if err != nil || request.State != "pending" || request.RecordID == nil {
		t.Fatalf("Create(%s) = %+v, %v; want it pending on a person, as an admin's federation secret is", name, request, err)
	}
	// A person decides only under an approval-page session (requests.Machine.ApplyPageDecision), so
	// the operator signs in to the page first, as the page's sign-in callback writes a session.
	secret := make([]byte, 32)
	_, _ = rand.Read(secret) // crypto/rand.Read never fails.
	page := store.BrowserSessionRef{Hash: sha256.Sum256(secret), Login: profileOperator}
	if err := k.st.CreateBrowserSession(t.Context(), store.BrowserSignIn{
		Hash: page.Hash, Login: profileOperator, Expires: time.Now().Add(time.Hour),
		Account: store.IssuerSubject{Issuer: "https://accounts.google.com", Subject: "sub-" + profileOperator},
	}); err != nil {
		t.Fatalf("sign %s in to the approval page: %v", profileOperator, err)
	}
	decision, err := k.machine.ApplyPageDecision(t.Context(), *request.RecordID, true, page)
	if err != nil || decision.GrantID == "" {
		t.Fatalf("ApplyPageDecision(%s) = %+v, %v; want a grant", name, decision, err)
	}
	release, err := k.machine.Values(t.Context(), decision.GrantID, k.enrollmentID, requests.ReleaseCall{})
	if err != nil || release.Airlock == nil {
		t.Fatalf("Values(%s) = %+v, %v; want the name behind the proxy", name, release, err)
	}
	if _, inline := release.Values[name]; inline {
		t.Fatalf("the release hands over %s's value itself; want it behind the proxy alone", name)
	}
	if holdsText(t, release, anthropicTokenPrefix) {
		t.Fatalf("the release of %s holds a string that opens %s", name, anthropicTokenPrefix)
	}
	slots := release.Airlock.Names[name].Slots
	if len(slots) != 1 || slots[requests.SlotValue] == "" {
		t.Fatalf("the release names %s's slots %v; want the one value slot", name, slots)
	}
	return decision.GrantID, release.Airlock, slots[requests.SlotValue]
}

// The four sets of requests of the check: (a) the Admin API's reads and writes, which both lists
// allow, and the three the kind's allow refuses - (b) the token exchange on the same host, (c) the
// Messages API, (d) a method of the Admin API the kind does not name.
var (
	adminCalls   = []struct{ method, path string }{{http.MethodGet, "/v1/organizations/workspaces"}, {http.MethodPost, "/v1/organizations/workspaces"}, {http.MethodDelete, "/v1/organizations/workspaces/w1"}}
	refusedCalls = []struct{ method, path string }{{http.MethodPost, "/v1/oauth/token"}, {http.MethodPost, "/v1/messages"}, {http.MethodPut, "/v1/organizations/workspaces/w1"}}
)

// TestTheAnthropicKindCarriesTheAdminAPIAlone is check k47 at the proxy: the kind as the broker
// read it; for each secret, the three Admin API calls reach the API with the exchange's token in
// Authorization and no placeholder, and the token exchange, the Messages API and a PUT of the
// Admin API are refused 403 naming the rule, with nothing sent - for K, with no profile, by the
// kind's allow alone; IAM was asked for each secret's bound account's identity token for the
// audience https://api.anthropic.com, and the exchange was sent that token with that secret's
// own ids, once for the three calls; each call line names the way mint, each mint line holds the
// token's SHA-256, and no line holds the token. Then a fresh grant whose exchange answers a token
// of 3601 seconds is refused at its first call, and nothing reaches the API.
//
// The named reverts: decide on the profile's allow and not the kind's (drop proxyrules.check's
// kind allowOK), and K's refused calls reach the API with the token; let the minter take any
// lifetime (drop federationLifetimeMax's check), and the call after the 3601 answer reaches it.
func TestTheAnthropicKindCarriesTheAdminAPIAlone(t *testing.T) {
	k := newAnthropicRig(t)

	rules := k.machine.AirlockRules()
	want := proxyrules.Kind{
		Name: "anthropic", Hosts: []string{anthropicHost}, Headers: []string{"Authorization"}, Variables: []string{"ANTHROPIC_ADMIN_TOKEN"},
		Ways: []string{proxyrules.WayMint}, Allow: adminRows, MaxGrant: "1h",
	}
	if kind := rules.Kind("anthropic"); kind == nil || !reflect.DeepEqual(*kind, want) {
		t.Fatalf("the broker reads the anthropic kind as %+v; want %+v", kind, want)
	}
	for _, c := range []struct {
		secret federationSecret
		allow  []proxyrules.Allow
	}{{secretN, adminRows}, {secretK, nil}} {
		binding := rules.Lookup(c.secret.name)
		if binding == nil || binding.Kind != "anthropic" || binding.Way != proxyrules.WayMint || !reflect.DeepEqual(binding.Allow, c.allow) {
			t.Fatalf("the broker binds %s as %+v; want the anthropic kind, way mint, and its profile's allow %v", c.secret.name, binding, c.allow)
		}
	}

	var narrowedGrant string
	for _, s := range []federationSecret{secretN, secretK} {
		t.Run(s.name, func(t *testing.T) {
			askedBefore := len(k.iam.Requests())
			exchangedBefore, _ := k.exchange.exchanges()
			grantID, released, placeholder := k.grant(t, s.name)
			if s.narrowed {
				narrowedGrant = grantID
			}
			client := k.client(released.ProxyPassword)

			// (a): one exchange of the proxy's own, whose token every call carries.
			sent := len(k.requests())
			_, releasedTokens := k.exchange.exchanges()
			for _, call := range adminCalls {
				if answer, body := k.do(client, call.method, "https://"+anthropicHost+call.path, "", "Authorization", "Bearer "+placeholder); answer.StatusCode != http.StatusOK {
					t.Fatalf("%s %s = %d %q; want 200 from the API", call.method, call.path, answer.StatusCode, body)
				}
			}
			asked, tokens := k.exchange.exchanges()
			if len(tokens) != len(releasedTokens)+1 {
				t.Fatalf("the three Admin API calls made %d exchanges; want one, whose token the proxy holds", len(tokens)-len(releasedTokens))
			}
			token := tokens[len(tokens)-1]
			carried := k.requests()[sent:]
			if len(carried) != len(adminCalls) {
				t.Fatalf("the API saw %d calls; want the %d Admin API calls", len(carried), len(adminCalls))
			}
			for i, seen := range carried {
				if seen.Method != adminCalls[i].method || seen.Path != adminCalls[i].path || seen.Header.Get("Authorization") != "Bearer "+token {
					t.Errorf("the API saw %s %s with Authorization %q; want %s %s with the exchange's token", seen.Method, seen.Path, seen.Header.Get("Authorization"), adminCalls[i].method, adminCalls[i].path)
				}
				if holdsText(t, seen, placeholder) {
					t.Errorf("the API's record of %s %s holds the placeholder", seen.Method, seen.Path)
				}
			}
			for _, call := range adminCalls {
				line := callLineOf(t, k.rig, map[string]any{"method": call.method, "path": call.path, "secret": s.name})
				if line.fields["way"] != proxyrules.WayMint || line.fields["allowed"] != true || line.fields["grant"] != grantID {
					t.Errorf("the call line of %s %s = %v; want an allowed line of grant %s naming the way mint", call.method, call.path, line.fields, grantID)
				}
			}

			// (b), (c), (d): refused by the kind's allow, nothing sent.
			for _, call := range refusedCalls {
				sent, refusals := len(k.requests()), len(k.logs.refusals())
				answer, body := k.do(client, call.method, "https://"+anthropicHost+call.path, "", "Authorization", "Bearer "+placeholder)
				if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "not allowed by rule") {
					t.Errorf("%s %s = %d %q; want 403 naming the kind's rule", call.method, call.path, answer.StatusCode, body)
				}
				if lines := k.logs.refusals()[refusals:]; len(lines) != 1 || lines[0].fields["code"] != codeNotAllowed || lines[0].fields["reason"] != "not allowed by rule" {
					t.Errorf("%s %s logged %+v; want one refusal, %s, by the kind's rule", call.method, call.path, lines, codeNotAllowed)
				}
				if got := k.requests()[sent:]; len(got) != 0 {
					t.Errorf("%s %s reached the API: %+v; want nothing sent", call.method, call.path, got)
				}
			}

			// IAM and the exchange: the release's mint and the proxy's, each an identity token of
			// the secret's own bound account for Anthropic, traded with the secret's own ids.
			identities := k.iam.Requests()[askedBefore:]
			exchanged := asked[len(exchangedBefore):]
			if len(exchanged) != 2 || len(identities) != len(exchanged) {
				t.Fatalf("%s's release and calls made %d exchanges and %d identity tokens; want two of each, the release's and the proxy's", s.name, len(exchanged), len(identities))
			}
			for _, identity := range identities {
				if identity.Path != "/v1/projects/-/serviceAccounts/"+s.account+":generateIdToken" || identity.Body["audience"] != "https://api.anthropic.com" {
					t.Errorf("IAM was asked %s for %v; want %s's identity token for https://api.anthropic.com", identity.Path, identity.Body, s.account)
				}
			}
			for _, body := range exchanged {
				wantBody := map[string]any{
					"grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer", "assertion": ciphertokentest.IAMIdentityToken,
					"organization_id": s.organization, "federation_rule_id": s.rule, "service_account_id": s.target, "workspace_id": s.workspace,
				}
				if !reflect.DeepEqual(body, wantBody) {
					t.Errorf("the exchange was sent %v; want %v", body, wantBody)
				}
			}

			// The mint lines: one for each token minted, the proxy's among them with its SHA-256.
			var proxyMint bool
			for _, line := range k.logs.lines(requests.MintMessage) {
				if line.fields["secret"] != s.name {
					continue
				}
				if line.fields["way"] != proxyrules.WayMint || line.fields["type"] != policy.TypeAnthropicFederation || line.fields["slot"] != requests.SlotValue || line.fields["grant"] != grantID {
					t.Errorf("a mint line of %s = %v; want way mint, type %s, the value slot and grant %s", s.name, line.fields, policy.TypeAnthropicFederation, grantID)
				}
				proxyMint = proxyMint || line.fields["sha256"] == sha256Hex(token)
			}
			if !proxyMint {
				t.Errorf("no mint line of %s holds the SHA-256 of the token the proxy swapped in", s.name)
			}
		})
	}

	// No line the broker wrote holds a token; the same search finds one in the API's record.
	if !holdsText(t, k.requests(), anthropicTokenPrefix) {
		t.Fatal("the control failed: the search finds no token in the API's record of the calls it took")
	}
	if holdsText(t, k.logs.writtenLines(t), anthropicTokenPrefix) {
		t.Error("a line the broker wrote holds an Anthropic token")
	}

	t.Run("a fresh grant whose exchange answers a token of 3601 seconds", func(t *testing.T) {
		// The first grant ends, so the request after it is a grant of its own, which the proxy
		// has read nothing of.
		if err := k.machine.RevokeGrant(t.Context(), narrowedGrant, k.enrollmentID); err != nil {
			t.Fatalf("RevokeGrant(%s): %v", narrowedGrant, err)
		}
		_, released, placeholder := k.grant(t, secretN.name)
		k.exchange.answerLifetime(3601)
		sent, refusals := len(k.requests()), len(k.logs.refusals())
		call := adminCalls[0]
		answer, body := k.do(k.client(released.ProxyPassword), call.method, "https://"+anthropicHost+call.path, "", "Authorization", "Bearer "+placeholder)
		if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "3601 seconds") {
			t.Errorf("%s %s after a 3601-second answer = %d %q; want 403 naming the token's lifetime", call.method, call.path, answer.StatusCode, body)
		}
		if lines := k.logs.refusals()[refusals:]; len(lines) != 1 || lines[0].fields["code"] != codeMintFailed {
			t.Errorf("the call logged %+v; want one refusal, %s", lines, codeMintFailed)
		}
		if got := k.requests()[sent:]; len(got) != 0 {
			t.Errorf("the call reached the API: %+v; want nothing sent", got)
		}
	})
}
