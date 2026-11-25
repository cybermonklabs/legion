package api_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
)

// TestAProofOfARouteWithAnEscapedElementVerifies: a session proof signs the URL of its request as
// the client sends it (proof.TargetURI keeps an escaped path element as written: %41 where the
// client escaped a letter, %5F an underscore). The broker checks the proof's htu against its own
// URL followed by the path as the request wrote it (r.URL.EscapedPath), so the proof verifies and
// the route answers for itself; where the broker joined the decoded path (r.URL.Path), %41 became
// an "A" and %5F an "_", the two texts differed, and every proof of such a route was refused 401
// PROOF_INVALID before the route was reached. Each name decodes to a valid agent secret name, which
// the route checks before it authenticates.
func TestAProofOfARouteWithAnEscapedElementVerifies(t *testing.T) {
	ts := newTestServer(t)
	for _, c := range []struct{ name, element string }{
		{"escaped-letter", "%41BC"},
		{"escaped-underscore", "A%5FB"},
	} {
		t.Run(c.name, func(t *testing.T) {
			id, key := ts.newSessionEnrollment(t, "rereader-"+uuid.NewString(), ciphertest.RereaderOperator)
			status, body := ts.session(t, key, id, http.MethodPost, "/v1/secrets/"+c.element+"/reread", nil)
			t.Logf("POST /v1/secrets/%s/reread answered %d %s", c.element, status, body)
			if status == http.StatusUnauthorized {
				t.Fatalf("POST /v1/secrets/%s/reread answered %d %s; want the route's own answer, not a proof refused for its escaped path", c.element, status, body)
			}
		})
	}
}
