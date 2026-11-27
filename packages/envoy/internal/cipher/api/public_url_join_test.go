// public_url_join_test.go drives the broker's own URLs — the RFC 8414 metadata's endpoints and the
// htu a proof is checked against — on a deployment whose BROKER_PUBLIC_URL carries a path and a
// query, which an ingress that signs the address it forwards sets.
package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
)

// The path and the query such a deployment's BROKER_PUBLIC_URL carries.
const (
	publicPath  = "/broker"
	publicQuery = "sig=s1g-7Qz4k"
)

// publicURLWithQuery is the rig with that URL as the broker's own: the one address its callers
// reach it at, and the base of every absolute URL it writes about itself.
func publicURLWithQuery(t *testing.T) *testServer {
	t.Helper()
	return newTestServerWith(t, func(deps *api.Deps) { deps.PublicURL += publicPath + "?" + publicQuery })
}

// TestTheMetadataDocumentJoinsItsEndpointsOntoThePublicURL: every endpoint the RFC 8414 document
// names is its path joined onto the public URL's path with the configured query kept after it,
// which is what RFC 6749 section 3.1 says of an endpoint URI that carries a query —
// /broker/v1/oauth/token?sig=... — where the path appended to the URL's text sat inside that query
// (/broker?sig=.../v1/oauth/token), naming the path /broker with the query's value changed. The
// issuer is the URL as the deployment set it: it is the identifier the document is published
// under, and RFC 8414 section 2 gives this handler no leave to publish another.
func TestTheMetadataDocumentJoinsItsEndpointsOntoThePublicURL(t *testing.T) {
	ts := publicURLWithQuery(t)
	document := ts.metadata(t)
	base := ts.URL + publicPath
	for _, row := range []struct{ name, got, want string }{
		{"issuer", document.Issuer, base + "?" + publicQuery},
		{"token_endpoint", document.TokenEndpoint, base + "/v1/oauth/token?" + publicQuery},
		{"device_authorization_endpoint", document.DeviceAuthorizationEndpoint, base + "/v1/oauth/device_authorization?" + publicQuery},
		{"introspection_endpoint", document.IntrospectionEndpoint, base + "/v1/oauth/introspect?" + publicQuery},
		{"revocation_endpoint", document.RevocationEndpoint, base + "/v1/oauth/revoke?" + publicQuery},
	} {
		if row.got != row.want {
			t.Errorf("%s = %q, want %q: the endpoint's path is joined onto the public URL's path, its query kept", row.name, row.got, row.want)
		}
	}
}

// TestAProofIsCheckedAgainstThePathJoinedOntoThePublicURL: the htu the broker checks a session's
// proof against is the request's path joined onto the public URL's path, the configured query
// kept (joined) — the same join agent-secrets makes when it signs the proof. A proof naming the
// path appended to that URL's text is refused (appended), so the comparison can be seen to fail.
func TestAProofIsCheckedAgainstThePathJoinedOntoThePublicURL(t *testing.T) {
	ts := publicURLWithQuery(t)
	const route = "/v1/secrets/WORKER_TOKEN/reread"
	for _, c := range []struct {
		name    string
		htu     string
		refused bool
	}{
		{"joined", ts.URL + publicPath + route + "?" + publicQuery, false},
		{"appended", ts.URL + publicPath + "?" + publicQuery + route, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			id, key := ts.newSessionEnrollment(t, "join-"+uuid.NewString(), ciphertest.RereaderOperator)
			signed, err := proof.Sign(key, id, http.MethodPost, c.htu, time.Now())
			if err != nil {
				t.Fatalf("proof.Sign: %v", err)
			}
			status, body := ts.req(t, http.MethodPost, route, map[string]string{"Proof": signed}, nil)
			code := ""
			if status >= 400 {
				code = decode[wireError](t, body).Code
			}
			switch {
			case c.refused && code != "PROOF_INVALID":
				t.Errorf("a proof naming %q = %d %s; want PROOF_INVALID: it names an htu the broker does not build", c.htu, status, body)
			case !c.refused && code == "PROOF_INVALID":
				t.Errorf("a proof naming %q = %d %s; want it taken: it names the htu the broker builds", c.htu, status, body)
			}
		})
	}
}
