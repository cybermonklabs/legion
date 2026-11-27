package secrets

import (
	"net/http"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
)

// TraceTransportForTest has mintAttempt read the connection trace of transport as it reads its
// own transport's (tracesConnections). A test passes only a transport it made itself and
// registers no protocol on, so its trace is net/http's own. It exists in tests alone.
func TraceTransportForTest(transport *http.Transport) { tracedTransports.Store(transport, struct{}{}) }

// OwnClientTracesConnectionsForTest reports whether the client a minter sends through when it
// names none is one whose connection trace mintAttempt reads, and whether a caller's own
// *http.Transport is not. It exists in tests alone.
func OwnClientTracesConnectionsForTest() (own, callers bool) {
	return tracesConnections(ownClient), tracesConnections(&http.Client{Transport: &http.Transport{}})
}

// FakeGCPSecretForTest is one secret StartFakeGCPForTest serves: Tags are its annotations, the
// tags the GCP store reads, and Value its latest version's value.
type FakeGCPSecretForTest struct {
	Tags  map[string]string
	Value string
}

// StartFakeGCPForTest serves secrets, keyed by their full resource names, from gcp_test.go's fake
// Secret Manager (startFake), each with an ENABLED latest version and both its replication policy
// and that version on the Cloud KMS key key, and answers the broker's own client dialled to it. It
// exists in tests alone, for the tests outside this package that read the GCP store through the
// policy, which this package's own tests cannot import.
func StartFakeGCPForTest(t *testing.T, key string, secrets map[string]FakeGCPSecretForTest) *secretmanager.Client {
	t.Helper()
	f := &fakeSecretManager{secrets: map[string]*fakeSecret{}}
	for name, s := range secrets {
		f.secrets[name] = &fakeSecret{annotations: s.Tags, replicationKey: key, versionKey: key,
			state: secretmanagerpb.SecretVersion_ENABLED, value: []byte(s.Value)}
	}
	return startFake(t, f)
}
