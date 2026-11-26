package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// The proxy URL and certificate a test server behind the airlock hands every release
// (BROKER_AIRLOCK_PROXY_URL, BROKER_AIRLOCK_CA_CERT_FILE): strings the release carries unread.
const (
	testProxyURL      = "http://127.0.0.1:13381"
	testCACertificate = "-----BEGIN CERTIFICATE-----\ntest-airlock-ca\n-----END CERTIFICATE-----\n"
)

// newProxiedTestServer is newProfiledTestServer whose profiles file declares policytest.Kinds and
// whose stored secrets of names each carry a proxy tag naming example (api.example.com), as each
// owner's `cipher secret retag --proxy example` writes it, so a release of each takes the airlock's
// path; adjust runs after the tags.
func newProxiedTestServer(t *testing.T, names []string, adjust func(*api.Deps)) *testServer {
	t.Helper()
	return newProfiledTestServer(t, policytest.KindsFile(), func(d *api.Deps) {
		for _, name := range names {
			proxyTag(t, d, name, "example")
		}
		d.Machine.AirlockProxyURL, d.Machine.AirlockCACertificate = testProxyURL, testCACertificate
		adjust(d)
	})
}

// proxyTag puts a proxy tag (policy.TagProxy) naming kind on the stored secret name of the server
// d builds, and has its policy read the namespace again, so the policy serves name behind that kind
// of its profiles file from here on.
func proxyTag(t *testing.T, d *api.Deps, name, kind string) {
	t.Helper()
	local := d.Machine.Secrets.(secrets.AWS).Client.(*secrets.Local)
	if _, err := local.TagResource(t.Context(), &secretsmanager.TagResourceInput{
		SecretId: aws.String(policytest.ID(name)), Tags: []smtypes.Tag{{Key: aws.String(policy.TagProxy), Value: aws.String(kind)}},
	}); err != nil {
		t.Fatalf("tag %s with %s=%s: %v", name, policy.TagProxy, kind, err)
	}
	if err := d.Policy.Refresh(t.Context()); err != nil {
		t.Fatalf("policy refresh after tagging %s: %v", name, err)
	}
	if served := d.Policy.Get().Secrets[name]; served.Proxy != kind {
		t.Fatalf("after its %s tag names %s, %s is served behind %q; refused %+v", policy.TagProxy, kind, name, served.Proxy, d.Policy.Get().Refused)
	}
}

// wireAirlockValues is POST /v1/grants/{id}/values's answer as a session reads it when the grant
// holds a name behind the airlock: every field by its wire name, so a change to one of them
// fails here.
type wireAirlockValues struct {
	Values  map[string]string `json:"values"`
	Airlock *struct {
		ProxyURL      string `json:"proxy_url"`
		CACertificate string `json:"ca_certificate"`
		ProxyPassword string `json:"proxy_password"`
		Names         map[string]struct {
			Value string            `json:"value"`
			Slots map[string]string `json:"slots"`
			Hosts []string          `json:"hosts"`
		} `json:"names"`
	} `json:"airlock"`
}

// TestTheValuesAnswerCarriesTheAirlock: a release of a grant whose one name its proxy tag puts
// behind a kind hands the session no value at all and the airlock instead — the proxy, its
// certificate, the password its connections open with, and the name's placeholder, slots and
// hosts, the hosts of its kind. A session that gets this and no "values" entry for the name is
// what makes an old CLI refuse to run rather than run with a placeholder it does not understand.
func TestTheValuesAnswerCarriesTheAirlock(t *testing.T) {
	t.Parallel()
	const proxyURL, caCertificate = testProxyURL, testCACertificate
	ts := newProxiedTestServer(t, []string{"WORKER_TOKEN"}, func(*api.Deps) {})
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	_, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, sessionKey, ts.URL, "read the worker token", "WORKER_TOKEN"), "session_id": nil})
	created := decode[wireCreateRequestResponse](t, body)
	if created.GrantID == nil {
		t.Fatalf("POST /v1/requests = %s, want an automatic grant", body)
	}

	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+*created.GrantID+"/values", nil)
	if status != http.StatusOK {
		t.Fatalf("POST values = %d: %s", status, body)
	}
	answer := decode[wireAirlockValues](t, body)
	if len(answer.Values) != 0 {
		t.Fatalf("values = %v; want none: the one granted name is behind the airlock", answer.Values)
	}
	if answer.Airlock == nil {
		t.Fatalf("the answer carries no airlock: %s", body)
	}
	if answer.Airlock.ProxyURL != proxyURL || answer.Airlock.CACertificate != caCertificate {
		t.Errorf("airlock = %+v; want the proxy %q and its certificate", answer.Airlock, proxyURL)
	}
	held, named := answer.Airlock.Names["WORKER_TOKEN"]
	if !named || len(answer.Airlock.Names) != 1 {
		t.Fatalf("airlock names = %v; want WORKER_TOKEN alone", answer.Airlock.Names)
	}
	if !strings.HasPrefix(held.Value, "agent-secrets-placeholder-") {
		t.Errorf("WORKER_TOKEN = %q; want a placeholder", held.Value)
	}
	if held.Slots["value"] != held.Value || len(held.Slots) != 1 {
		t.Errorf("WORKER_TOKEN's slots = %v; want the value slot holding %q", held.Slots, held.Value)
	}
	if len(held.Hosts) != 1 || held.Hosts[0] != "api.example.com" {
		t.Errorf("WORKER_TOKEN's hosts = %v; want its kind's", held.Hosts)
	}
	if answer.Airlock.ProxyPassword != held.Value {
		t.Errorf("proxy password = %q; want the grant's one placeholder %q", answer.Airlock.ProxyPassword, held.Value)
	}
	if strings.Contains(string(body), "worker-v1") {
		t.Errorf("the answer carries WORKER_TOKEN's real value: %s", body)
	}
	// The field order and the whole set of keys, which is the contract a client reads.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"expires_at", "values", "airlock"} {
		if _, ok := keys[want]; !ok {
			t.Errorf("the answer has no %q: %s", want, body)
		}
	}
	if len(keys) != 3 {
		t.Errorf("the answer carries %d fields: %s; want expires_at, values and airlock", len(keys), body)
	}
}

// TestAStoredSecretUnderADPoPBlockIsRefusedItsRelease: a stored secret whose name a dpop block of
// the profiles file holds - the inline binding of a capability the broker mints and holds, never a
// stored secret's release - is released to no session. The policy serves no secret by that name
// (policy.ReasonProfileMinted), so a grant of the stored secret made before the block was there is
// answered 403 GRANT_NOT_LIVE at its next release, and a new request for the name 400
// UNKNOWN_SECRET, each a refusal with nothing of the value in it and no placeholder written,
// rather than a broker fault. The release's own refusal of a dpop binding (requests.ErrMintedOnly,
// which this route answers 403 MINTED_ONLY) sits behind that policy rule, and the requests
// package tests it with the set the loader builds before the profiles' apply, the one set where a
// dpop block binds a name the policy serves.
func TestAStoredSecretUnderADPoPBlockIsRefusedItsRelease(t *testing.T) {
	t.Parallel()
	ts := newProxiedTestServer(t, nil, func(*api.Deps) {})
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	_, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, sessionKey, ts.URL, "read the worker token", "WORKER_TOKEN"), "session_id": nil})
	created := decode[wireCreateRequestResponse](t, body)
	if created.GrantID == nil {
		t.Fatalf("POST /v1/requests = %s, want an automatic grant", body)
	}

	// The pod a profiles change rolls in, whose file holds a dpop block of WORKER_TOKEN's name.
	ts.machine.Policy = policytest.ProfiledCurrent(t, ts.Secrets, policytest.KindsFile(
		`{"name": "WORKER_TOKEN", "proxy": {"hosts": ["dispatch.example.com"], "headers": ["Authorization"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}]}}`))
	if binding := ts.machine.AirlockRules().Lookup("WORKER_TOKEN"); binding == nil || binding.Inline == nil || binding.Way != proxyrules.WayDPoP {
		t.Fatalf("WORKER_TOKEN's binding = %+v; want the dpop block's inline rule", binding)
	}
	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+*created.GrantID+"/values", nil)
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "GRANT_NOT_LIVE" {
		t.Fatalf("POST values of a stored secret under a dpop block = %d %s; want 403 GRANT_NOT_LIVE", status, body)
	}
	if strings.Contains(string(body), "worker-v1") {
		t.Errorf("the refusal carries WORKER_TOKEN's real value: %s", body)
	}
	var rows int
	if err := ts.Store.Pool.QueryRow(t.Context(), `select count(*) from airlock_placeholders where grant_id=$1`, *created.GrantID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("the refused release left %d placeholder rows (%v); want none", rows, err)
	}
	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, sessionKey, ts.URL, "read the worker token again", "WORKER_TOKEN"), "session_id": nil})
	if status != http.StatusBadRequest || decode[wireError](t, body).Code != "UNKNOWN_SECRET" || strings.Contains(string(body), "worker-v1") {
		t.Fatalf("POST /v1/requests for a name a dpop block holds = %d %s; want 400 UNKNOWN_SECRET with nothing of the value", status, body)
	}
}
