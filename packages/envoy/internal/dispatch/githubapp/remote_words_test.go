package githubapp

import (
	"errors"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAStatusErrorPrintsGitHubsBodyAsTheRuleReadsIt: GitHub refuses a request with a body that
// quotes a URL carrying a credential. The StatusError CheckResponse answers prints the status and
// the body as the rule for remote text reads it, under every verb and in a value that holds it,
// and none of the credentials.
func TestAStatusErrorPrintsGitHubsBodyAsTheRuleReadsIt(t *testing.T) {
	for _, row := range []struct{ name, body, shown string }{
		{"user information and a query value", redacttest.Words, redacttest.Shown},
		{"a fragment's value", redacttest.FragmentWords, redacttest.FragmentShown},
		{"a JSON body the rule cannot read", `{"message":"` + redacttest.Words + `"}`, redact.WordsNotReadable},
	} {
		t.Run(row.name, func(t *testing.T) {
			var status *StatusError
			if err := CheckResponse(http.StatusUnprocessableEntity, http.Header{}, []byte(row.body)); !errors.As(err, &status) {
				t.Fatalf("CheckResponse = %v, want a StatusError", err)
			}
			redacttest.RequireNoneFormatted(t, "githubapp.StatusError", status)
			if want := "status 422: " + row.shown; status.Error() != want {
				t.Errorf("StatusError = %q, want %q", status, want)
			}
		})
	}
}
