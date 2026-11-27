// named_issuers_audiences_test.go drives the REAL compiled broker trusting two named Kubernetes
// issuers with BROKER_K8S_OIDC_AUDIENCE listing two audiences, on a real Postgres and over HTTP
// alone, as named_issuers_test.go drives it: the one audience list serves every issuer
// BROKER_K8S_OIDC_ISSUER names, so the second issuer's token of either listed audience is taken
// and its token of an audience the setting does not list is refused.
package main

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestTheSecondNamedIssuersTokenOfAListedAudienceEnrolsAPod: with two named issuers, A and B, and
// BROKER_K8S_OIDC_AUDIENCE listing enrolmentAudience and podAudienceOld, B's daemon logs in with
// B's token of the first listed audience, and a pod whose token B issued for the second listed
// audience enrolls, 201 with an enrollment, while a pod whose token B issued for an audience the
// setting does not list is refused 403 POD_IDENTITY_MISMATCH, as on a broker of one issuer and
// one audience. Its control is the second row with the pod verifier's audience check turned off
// for B's tokens, where the refused token enrolls and the row fails.
func TestTheSecondNamedIssuersTokenOfAListedAudienceEnrolsAPod(t *testing.T) {
	a, b := oidctest.New(t), oidctest.New(t)
	a.PublishKey(t, "cluster-key")
	bKey := b.PublishKey(t, "cluster-key")
	broker := startBroker(t, brokerOptions{Env: namedIssuersEnv(t, storetest.URL(t), bothIssuers(a, b),
		// After namedIssuersEnv's own, one audience: os/exec keeps the last copy of a variable.
		"BROKER_K8S_OIDC_AUDIENCE="+enrolmentAudience+" "+podAudienceOld,
		"BROKER_SERVICE_LOGINS="+namedService+"="+namedDaemonB+"=b",
	)})
	client := namedClient{t: t, base: "http://" + broker.Addr}

	status, code, login := client.tokenLogin(clusterToken(t, b, bKey, namedDaemonB, ""), proofKey(t))
	if status != http.StatusOK || login.credentialID == "" {
		t.Fatalf("B's daemon's token login with B's token of the first listed audience answered %d %q, want 200 with a credential", status, code)
	}
	expect(t, "B's pod token ", []namedRoute{
		{"of the second listed audience enrolls", func() (int, string) {
			status, code, _ := client.enroll(login, "pod-of-b-listed", clusterTokenFor(t, b, bKey, namedWorker, "pod-of-b-listed", podAudienceOld))
			return status, code
		}, http.StatusCreated, ""},
		{"of an audience the setting does not list is refused", func() (int, string) {
			status, code, _ := client.enroll(login, "pod-of-b-unlisted", clusterTokenFor(t, b, bKey, namedWorker, "pod-of-b-unlisted", unlistedPodAudience))
			return status, code
		}, http.StatusForbidden, "POD_IDENTITY_MISMATCH"},
	})
}
