// packages/envoy/internal/cipher/ciphertest/rig_test.go

package ciphertest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

// TestNewRigConnectsAndServesHealthz is the rig's own smoke test: NewRig migrates a real schema
// and mounts a real broker that answers its public healthz route.
func TestNewRigConnectsAndServesHealthz(t *testing.T) {
	rig := NewRig(t)
	status, body := rig.Req(t, http.MethodGet, "/healthz", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200: %s", status, body)
	}
}

// TestTheRigIssuesTokensOfTheLifeItIsGiven: a Rig built WithAccessLifetime issues access tokens
// of that life, which is what a trade's expires_in says, and one built without the option issues
// them for fifteen minutes, BROKER_ACCESS_TOKEN_SECONDS' default, as it always has.
//
// The OAuth library counts expires_in down from the moment it set the token's expiry to the
// moment it writes the answer, in whole seconds rounded down, so a trade answers the life it was
// given less the part of a second, or more, that the trade itself took: 419 for 420. The bound
// below is that: no more than the life, and no less than the life less the trade's own length,
// rounded up.
func TestTheRigIssuesTokensOfTheLifeItIsGiven(t *testing.T) {
	for _, c := range []struct {
		name    string
		options []Option
		want    int64
	}{
		{"WithAccessLifetime(420s)", []Option{WithAccessLifetime(420 * time.Second)}, 420},
		{"no option", nil, 900},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig := NewRig(t, c.options...)
			before := time.Now()
			answer, _ := tradeAs(t, rig, ExchangeAccount)
			took := int64((time.Since(before) + time.Second - 1) / time.Second)
			if answer.ExpiresIn > c.want || answer.ExpiresIn < c.want-took {
				t.Fatalf("a trade at a Rig built with %s answered expires_in %d, want %d less at most the %ds the trade took", c.name, answer.ExpiresIn, c.want, took)
			}
		})
	}
}

// TestTheRigGrantsTheDaemonItsFixedSession: a pod of DaemonExchangeAccount trades its subject
// token, naming no session, for a token the broker reads back as the session
// legion-daemon:EXAMPLE of project EXAMPLE, acting for the Rig's Operator, which is the token a
// daemon writes Dispatch with; a pod of OtherProjectAccount trades for other-project-worker of
// project OTHER, for the same person.
func TestTheRigGrantsTheDaemonItsFixedSession(t *testing.T) {
	rig := NewRig(t)
	for _, c := range []struct {
		account, session, project string
	}{
		{DaemonExchangeAccount, "legion-daemon:EXAMPLE", "EXAMPLE"},
		{OtherProjectAccount, "other-project-worker", "OTHER"},
	} {
		_, read := tradeAs(t, rig, c.account)
		if !read.Active || read.Session != c.session || read.Person != rig.Operator ||
			read.ProjectScope != oauthwire.ProjectScopeNamed || len(read.Projects) != 1 || read.Projects[0] != c.project {
			t.Errorf("%s's token reads back as %+v, want a live token of session %s, person %s and the one project %s",
				c.account, read, c.session, rig.Operator, c.project)
		}
	}
}

// exchangeAnswer is what this file reads of a trade's answer.
type exchangeAnswer struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// tradeAs is one trade at the Rig's token endpoint as a pod of account makes it: its subject
// token, naming no session, proved with a fresh key through cipherclient's DPoP signer. It
// answers the trade's answer and what the broker's introspection reads the minted token back as
// (Rig.Introspect), and fails t on any refusal.
func tradeAs(t *testing.T, rig *Rig, account string) (exchangeAnswer, oauthwire.Introspection) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"grant_type":         {cipherclient.GrantTypeTokenExchange},
		"client_id":          {oauthwire.ExchangeClientID},
		"subject_token":      {rig.MintSubjectToken(t, account, uuid.NewString())},
		"subject_token_type": {oauthwire.SubjectTokenTypeKubernetes},
	}
	proved := &http.Client{Transport: cipherclient.NewDPoPTransport(key, http.DefaultTransport)}
	status, body := postForm(t, proved, rig.URL+cipherclient.TokenPath, form)
	var answer exchangeAnswer
	if err := json.Unmarshal(body, &answer); err != nil || status != http.StatusOK || answer.AccessToken == "" {
		t.Fatalf("%s's trade = %d %s, want 200 and an access token", account, status, body)
	}
	return answer, rig.Introspect(t, answer.AccessToken)
}

// postForm posts form to target through client, answering the status and the body.
func postForm(t *testing.T, client *http.Client, target string, form url.Values) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer of POST %s: %v", target, err)
	}
	return response.StatusCode, body
}

// TestTheRigsRegistryServesThePolicysMetrics: Rig.Registry holds the policy's metrics as the
// broker's metrics port serves them. The rig's first load is one read="list" and refuses nothing;
// a reread of a secret the store holds without a value is one read="describe" and one
// broker_unservable_secrets series for it, a reread of a name the store does not hold adds none,
// and the reread after its value is put takes the series away.
func TestTheRigsRegistryServesThePolicysMetrics(t *testing.T) {
	rig := NewRig(t)
	scrape := func(metric string) string {
		t.Helper()
		recorder := httptest.NewRecorder()
		rig.Registry.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		var samples strings.Builder
		for line := range strings.Lines(recorder.Body.String()) {
			if strings.HasPrefix(line, metric+"{") {
				samples.WriteString(line)
			}
		}
		return samples.String()
	}
	reread := func(name string, served bool) {
		t.Helper()
		status, body := rig.Reread(t, name)
		var answer struct {
			Served bool `json:"served"`
		}
		if err := json.Unmarshal(body, &answer); err != nil || status != http.StatusOK || answer.Served != served {
			t.Fatalf("reread %s = %d %s, want 200 served=%t", name, status, body, served)
		}
	}
	unservable := `broker_unservable_secrets{secret="` + policytest.ID("LATER_KEY") + `",reason="` + policy.ReasonNoCurrentValue + `",owner="` + rig.Operator + `"} 1` + "\n"

	for _, step := range []struct {
		name               string
		act                func()
		reads, unservables string
	}{
		{"the first load", func() {}, `broker_policy_store_reads_total{read="list"} 1` + "\n", ""},
		{"a reread of a secret with no value", func() {
			rig.Secrets.Put(policytest.Secret("LATER_KEY", rig.Operator, policy.TierAgent, ""))
			reread("LATER_KEY", false)
		}, `broker_policy_store_reads_total{read="describe"} 1` + "\n" + `broker_policy_store_reads_total{read="list"} 1` + "\n", unservable},
		{"a reread of a name the store does not hold", func() { reread("NO_SUCH_KEY", false) },
			`broker_policy_store_reads_total{read="describe"} 2` + "\n" + `broker_policy_store_reads_total{read="list"} 1` + "\n", unservable},
		{"a reread once its value is put", func() {
			rig.Secrets.Put(policytest.Secret("LATER_KEY", rig.Operator, policy.TierAgent, "later-v1"))
			reread("LATER_KEY", true)
		}, `broker_policy_store_reads_total{read="describe"} 3` + "\n" + `broker_policy_store_reads_total{read="list"} 1` + "\n", ""},
	} {
		step.act()
		if got := scrape("broker_policy_store_reads_total"); got != step.reads {
			t.Fatalf("after %s the store reads are\n%s\nwant\n%s", step.name, got, step.reads)
		}
		if got := scrape("broker_unservable_secrets"); got != step.unservables {
			t.Fatalf("after %s broker_unservable_secrets reads\n%s\nwant\n%s", step.name, got, step.unservables)
		}
	}
}

// TestDecideApprovesAndDeniesOnTheBrokersPage: Rig.Decide signs Operator in to the rig's approval
// page and decides a pending TEST_SECRET request there, so the session's request is denied after a
// denial and granted after an approval (in that order, since a live grant would answer the next
// request at once).
func TestDecideApprovesAndDeniesOnTheBrokersPage(t *testing.T) {
	rig := NewRig(t)
	enrollmentID, key := rig.NewSession(t, rig.Operator)
	for _, approve := range []bool{false, true} {
		requestID, recordID := pendingRequest(t, rig, enrollmentID, key)
		rig.Decide(t, recordID, approve)
		want := "denied"
		if approve {
			want = "granted"
		}
		status, body := signed(t, rig, key, enrollmentID, http.MethodGet, "/v1/requests/"+requestID, nil)
		var read struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(body, &read); err != nil || status != http.StatusOK || read.State != want {
			t.Fatalf("request after Decide(approve=%t) = %d %s, want %s", approve, status, body, want)
		}
	}
}

// pendingRequest asks for TEST_SECRET as the session and answers the pending request and its
// record.
func pendingRequest(t *testing.T, rig *Rig, enrollmentID string, key *ecdsa.PrivateKey) (requestID, recordID string) {
	t.Helper()
	compact, err := record.Sign(key, rig.URL, []record.AuthorizationDetail{{Type: record.KindAgentSecret, Identifier: "TEST_SECRET", Actions: []string{"inject"}}}, "checking Rig.Decide decides on the broker's page", "", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, body := signed(t, rig, key, enrollmentID, http.MethodPost, "/v1/requests", map[string]any{"request": compact, "session_id": nil})
	var created struct {
		RequestID string  `json:"request_id"`
		State     string  `json:"state"`
		RecordID  *string `json:"record_id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || status != http.StatusOK || created.State != "pending" || created.RecordID == nil {
		t.Fatalf("POST /v1/requests TEST_SECRET = %d %s, want a pending request with a record", status, body)
	}
	return created.RequestID, *created.RecordID
}

// signed sends a request as the session, with its proof.
func signed(t *testing.T, rig *Rig, key *ecdsa.PrivateKey, enrollmentID, method, path string, body any) (int, []byte) {
	t.Helper()
	p, err := proof.Sign(key, enrollmentID, method, rig.URL+path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rig.Req(t, method, path, map[string]string{"Proof": p}, body)
}
