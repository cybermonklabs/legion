package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestACheckRunsReadRefusalPrintsGitHubsMessageAsTheRuleReadsIt: GitHub refuses the read of a
// head's check runs with a body whose `message` quotes a URL carrying a credential, or with a body
// that is not JSON. The detail a check shows names the status and GitHub's message as the rule for
// remote text reads it, a body that names no message drops from the detail as it did before, and
// no credential prints.
func TestACheckRunsReadRefusalPrintsGitHubsMessageAsTheRuleReadsIt(t *testing.T) {
	const prefix = "GitHub could not be read for the check runs of acme/widgets at abc123: read check runs: status 422"
	rows := map[string]struct{ body, want string }{
		"a body that is not JSON": {redacttest.Words, prefix + "."},
	}
	for name, message := range map[string]string{
		"a message: user information and a query value": redacttest.Words,
		"a message: a fragment's value":                 redacttest.FragmentWords,
		"a message: words the rule cannot read":         redacttest.Unreadable,
	} {
		body, err := json.Marshal(map[string]string{"message": message, "documentation_url": "https://docs.github.com/rest"})
		if err != nil {
			t.Fatal(err)
		}
		rows[name] = struct{ body, want string }{string(body), prefix + ": " + redact.Field(message) + "."}
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			refused := githubapp.CheckResponse(http.StatusUnprocessableEntity, http.Header{}, []byte(row.body))
			var status *githubapp.StatusError
			if !errors.As(refused, &status) {
				t.Fatalf("CheckResponse = %v, want a StatusError", refused)
			}
			detail := ciUnreadDetail(delivery.NewestHead{Repo: "acme/widgets", HeadSHA: "abc123"}, fmt.Errorf("read check runs: %w", refused))
			redacttest.RequireNone(t, "api.ci-detail", detail)
			if detail != row.want {
				t.Errorf("the detail is %q, want %q", detail, row.want)
			}
		})
	}
}
