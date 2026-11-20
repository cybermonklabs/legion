// packages/envoy/internal/cipher/ciphertest/rig_test.go

package ciphertest

import (
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
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
