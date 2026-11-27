package e2e

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
)

// r1GrantValues is POST /v1/grants/{id}/values's answer as the client of release R1, the last
// release before the kinds, decodes it: a copy of cmd/cipher's GrantValues, AirlockRelease and
// AirlockName as the commit this change is built on holds them. It is a copy, rather than the
// client's own types, so that a change to the client cannot make this check pass, and a field the
// answer drops or renames is one this decoder reads as empty.
type r1GrantValues struct {
	Values        map[string]string              `json:"values"`
	ExpiresAt     *time.Time                     `json:"expires_at"`
	Locations     []string                       `json:"locations"`
	GitHubLeftOut map[string]map[string][]string `json:"github_left_out"`
	Airlock       *struct {
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

// requireKeys fails t unless the JSON object raw, the part of the answer where names, writes each
// of keys: a field the R1 client reads that this answer always carries, so its absence is a field
// dropped or renamed, which the client would read as empty.
func requireKeys(t *testing.T, raw json.RawMessage, where string, keys ...string) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("%s is no JSON object: %v (%s)", where, err, raw)
	}
	for _, key := range keys {
		if _, held := object[key]; !held {
			t.Errorf("%s writes no %q, which the client of release R1 reads: %s", where, key, raw)
		}
	}
	return object
}

// TestTheReleaseOfANameBehindAKindDecodesWithTheR1Client: the release answer of a broker that
// reads kinds changes only by adding to it, so a client of release R1, which pods and developer
// boxes run until they take a later one, reads a release of a name the proxy holds back as it read
// one of a name a proxy block held back: the proxy, the certificate, the password, and the name's
// value, slots and hosts, now the hosts of its kind. Every field that client reads is in the
// answer under the key it reads, and no part of the answer holds the secret's value. The client
// of this change is that client: cmd/cipher's decoder is unchanged from the commit this change is
// built on, so a broker of R1, whose answer has the same shape, decodes with it too.
func TestTheReleaseOfANameBehindAKindDecodesWithTheR1Client(t *testing.T) {
	stored := append(slices.Clone(e2eSecrets), policytest.Proxied(policytest.Secret("PROXIED_TOKEN", testApprover, policy.TierAgent, "proxied-v1"), "example"))
	ts := newE2EServerWith(t, e2eOptions{secrets: stored, profiles: policytest.KindsFile()})
	enrollmentID, key := ts.loginAndEnroll(t, testApprover, "e2e-r1-client")

	status, body := ts.session(t, key, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, key, ts.URL, "read it through the proxy", "PROXIED_TOKEN", "AUTO_READ_TOKEN"), "session_id": nil})
	created := decode[wireCreateRequestResponse](t, body)
	if status != http.StatusOK || created.State != "granted" || created.GrantID == nil {
		t.Fatalf("POST /v1/requests = %d %s; want granted at once", status, body)
	}
	status, body = ts.session(t, key, enrollmentID, http.MethodPost, "/v1/grants/"+*created.GrantID+"/values", nil)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/grants/{id}/values = %d: %s", status, body)
	}

	answer := requireKeys(t, body, "the answer", "expires_at", "values", "airlock")
	airlock := requireKeys(t, answer["airlock"], "the answer's airlock", "proxy_url", "ca_certificate", "proxy_password", "names")
	names := requireKeys(t, airlock["names"], "the airlock's names", "PROXIED_TOKEN")
	requireKeys(t, names["PROXIED_TOKEN"], "PROXIED_TOKEN's entry", "value", "slots", "hosts")

	var r1 r1GrantValues
	if err := json.Unmarshal(body, &r1); err != nil {
		t.Fatalf("the client of release R1 cannot decode the answer: %v (%s)", err, body)
	}
	if r1.Values["AUTO_READ_TOKEN"] != "read-v1" || len(r1.Values) != 1 {
		t.Errorf("values = %v; want AUTO_READ_TOKEN's alone, PROXIED_TOKEN held back", r1.Values)
	}
	if r1.Airlock == nil || r1.Airlock.ProxyURL != e2eProxyURL || r1.Airlock.CACertificate != e2eCACertificate {
		t.Fatalf("airlock = %+v; want the proxy %s and its certificate", r1.Airlock, e2eProxyURL)
	}
	held, named := r1.Airlock.Names["PROXIED_TOKEN"]
	if !named || len(r1.Airlock.Names) != 1 {
		t.Fatalf("airlock names = %+v; want PROXIED_TOKEN alone", r1.Airlock.Names)
	}
	if held.Value == "" || held.Slots["value"] != held.Value || len(held.Slots) != 1 || r1.Airlock.ProxyPassword != held.Value {
		t.Errorf("PROXIED_TOKEN = %+v with password %q; want one value slot holding its placeholder, which is the password", held, r1.Airlock.ProxyPassword)
	}
	if !slices.Equal(held.Hosts, []string{"api.example.com"}) {
		t.Errorf("PROXIED_TOKEN's hosts = %v; want the example kind's, which the client keeps off NO_PROXY", held.Hosts)
	}
	if strings.Contains(string(body), "proxied-v1") {
		t.Errorf("the answer holds PROXIED_TOKEN's value: %s", body)
	}
}

// TestTheReleaseOfANameBehindAProxyBlockOfAFileOfNoKinds: under a profiles file that declares no
// kinds, as every deployment's file did before them, a name behind a proxy block is released as
// it was then: the command gets a placeholder for it, never its value, and the hosts of its block,
// which the client keeps off NO_PROXY; a name no block names is released as its value beside it.
// The client of release R1 decodes the answer.
func TestTheReleaseOfANameBehindAProxyBlockOfAFileOfNoKinds(t *testing.T) {
	stored := append(slices.Clone(e2eSecrets), policytest.Secret("PROXIED_TOKEN", testApprover, policy.TierAgent, "proxied-v1"))
	file := policytest.ProfilesFile(`{"name": "PROXIED_TOKEN", "proxy": {"hosts": ["api.example.com", "uploads.example.com"], "headers": ["Authorization"]}}`)
	ts := newE2EServerWith(t, e2eOptions{secrets: stored, profiles: file})
	enrollmentID, key := ts.loginAndEnroll(t, testApprover, "e2e-proxy-block")

	status, body := ts.session(t, key, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, key, ts.URL, "read it through the proxy", "PROXIED_TOKEN", "AUTO_READ_TOKEN"), "session_id": nil})
	created := decode[wireCreateRequestResponse](t, body)
	if status != http.StatusOK || created.State != "granted" || created.GrantID == nil {
		t.Fatalf("POST /v1/requests = %d %s; want granted at once", status, body)
	}
	status, body = ts.session(t, key, enrollmentID, http.MethodPost, "/v1/grants/"+*created.GrantID+"/values", nil)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/grants/{id}/values = %d: %s", status, body)
	}
	var r1 r1GrantValues
	if err := json.Unmarshal(body, &r1); err != nil {
		t.Fatalf("the client of release R1 cannot decode the answer: %v (%s)", err, body)
	}
	if r1.Values["AUTO_READ_TOKEN"] != "read-v1" || len(r1.Values) != 1 {
		t.Errorf("values = %v; want AUTO_READ_TOKEN's alone, PROXIED_TOKEN held back", r1.Values)
	}
	if r1.Airlock == nil || r1.Airlock.ProxyURL != e2eProxyURL {
		t.Fatalf("airlock = %+v; want the proxy %s", r1.Airlock, e2eProxyURL)
	}
	held, named := r1.Airlock.Names["PROXIED_TOKEN"]
	if !named || len(r1.Airlock.Names) != 1 || held.Value == "" || held.Slots["value"] != held.Value {
		t.Fatalf("airlock names = %+v; want PROXIED_TOKEN alone, one value slot holding its placeholder", r1.Airlock.Names)
	}
	if !slices.Equal(held.Hosts, []string{"api.example.com", "uploads.example.com"}) {
		t.Errorf("PROXIED_TOKEN's hosts = %v; want its block's", held.Hosts)
	}
	if strings.Contains(string(body), "proxied-v1") {
		t.Errorf("the answer holds PROXIED_TOKEN's value: %s", body)
	}
}
