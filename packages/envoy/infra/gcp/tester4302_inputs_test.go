package gcp_test

// A red test from the independent test of the route check at 7c7131eece: it fails at that head.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
)

// routeAuthorization.secretKey refuses a key no Secret can hold: Kubernetes takes a Secret key of
// letters, digits, "-", "_" and ".", but not "." or "..", nor one that begins with "..", and the
// rolled pods could not start on such a key. Control: "route" and ".route" resolve.
func TestClusteredNatsHubRouteSecretKeyIsASecretsKey(t *testing.T) {
	for _, row := range []struct {
		key string
		ok  bool
	}{{"route", true}, {".route", true}, {".", false}, {"..", false}, {"..route", false}} {
		args := clusteredArgs()
		args.RouteAuthorization = &gcp.ClusteredHubRouteAuthorizationArgs{SecretName: "envoy-nats-route", SecretKey: row.key, Stage: "env"}
		_, err := gcp.ResolveClusteredNatsHub(args)
		if row.ok {
			require.NoError(t, err, "control: %q", row.key)
		} else {
			require.ErrorContains(t, err, "routeAuthorization.secretKey", "%q is no Secret key", row.key)
		}
	}
}
