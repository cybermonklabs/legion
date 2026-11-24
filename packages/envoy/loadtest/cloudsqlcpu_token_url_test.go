package main

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestATokenURLThatDoesNotParsePrintsNoneOfIt: a -token-url url.Parse refuses (an invalid escape in
// its path), with redacttest's credentials in its user information and its query, fails
// cloudsql-cpu in fixed words, with none of the URL (unparsed); http.NewRequest's error, which the
// metadata read returned, quotes it whole (twin). Captures loadtest.cloudsql-token-url/unparsed.
func TestATokenURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	unparsed := "http://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/computeMetadata%zz?access" + "_token=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodGet, unparsed, nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		_, _, err := runCloudSQLCPUAgainst(t, "http://127.0.0.1:1", "-token-url", unparsed)
		if err == nil {
			t.Fatal("cloudsql-cpu read a token at a -token-url that does not parse")
		}
		redacttest.RequireNoPiece(t, "loadtest.cloudsql-token-url/unparsed", err.Error())
		if want := "the -token-url does not parse (it is not printed)"; err.Error() != want {
			t.Errorf("cloudsql-cpu = %q; want %q", err, want)
		}
	})
}
