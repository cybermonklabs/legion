package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAMetadataReadPrintsNoCredentialOfItsURL: cloudsql-cpu's -token-url may carry user
// information, which the access token's read sends as Basic credentials, and whatever answers there
// answers with a redirect whose Location does not parse and quotes the password (userinfoEcho). The
// command's error holds neither the user name, which net/http's error keeps in the URL it names,
// nor a run of the password, and names the failure by its class. The twin shows net/http's own
// error holds the password. Captures loadtest.metadata-words/unparsed.
func TestAMetadataReadPrintsNoCredentialOfItsURL(t *testing.T) {
	tokenURL := userinfoEcho(t) + "/computeMetadata/v1/instance/service-accounts/default/token"
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, tokenURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		redirectstest.RequireEcho(t, request, redacttest.Password)
	})
	t.Run("unparsed", func(t *testing.T) {
		_, _, err := runCloudSQLCPUAgainst(t, "http://127.0.0.1:1", "-token-url", tokenURL)
		if err == nil {
			t.Fatal("cloudsql-cpu read an access token from a server that answers a redirect")
		}
		redacttest.RequireNoPiece(t, "loadtest.metadata-words/unparsed", err.Error(), redacttest.Password)
		if !strings.HasSuffix(err.Error(), "gave no access token: "+redact.ClassRequestFailed) {
			t.Errorf("cloudsql-cpu = %q, want the failure by its class", err)
		}
	})
}
