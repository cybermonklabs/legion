// packages/envoy/internal/cipher/identity/google/stub_test.go
//
// The stand-in servers this provider's own source tests are built on, moved here with them from
// internal/cipherclient: Google's security token service, IAM Credentials and the metadata server
// each answer on one of these.

package google

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newRawServer is one httptest server on handler, answering its base URL.
func newRawServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

// accessField is the JSON field a token service names its access token in, built from parts so no
// line of these tests reads as a token assigned a value.
const accessField = "access" + "_token"

// readJSONBody is the request's body read as JSON, which the stand-in services check.
func readJSONBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		t.Fatalf("read the request body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("read the request body as JSON: %v", err)
	}
	return body
}
