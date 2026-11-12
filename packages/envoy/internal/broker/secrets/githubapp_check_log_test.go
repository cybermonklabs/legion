package secrets_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestAGitHubAppCheckOfAReadOnlyNarrowingLogsNoReleasedToken: the broker's narrowing check
// (MintRequest.Check) mints and revokes the token a read-only release would mint, asking again
// without actions where the installation does not hold them, as the release does. It releases
// nothing, so it does not log the WARN a release logs for the token released without actions,
// whose words say a token was released for the installation: one check mints once for each
// repository it probes, and each would write that line for a release that never happened.
func TestAGitHubAppCheckOfAReadOnlyNarrowingLogsNoReleasedToken(t *testing.T) {
	var logged bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	github.setAnswer(func(installation string, body []byte) (int, any) {
		if strings.Contains(string(body), `"actions"`) {
			return http.StatusUnprocessableEntity, map[string]string{"message": "The permissions requested are not granted to this installation."}
		}
		return asGitHubAnswers(installation, body)
	})
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}

	checked, err := app.Mint(t.Context(), secrets.MintRequest{Value: githubAppValue(t, "123456", key), GitHubOwners: []string{"acme-org", "ada"},
		GitHubNarrowing: &secrets.GitHubNarrowing{Owner: "ada", ReadOnly: true}, Check: true})
	if err != nil || checked.Value != "" {
		t.Fatalf("a check of a read-only narrowing of an installation holding no actions = %q, %v; want it passed with nothing released", checked.Value, err)
	}
	if _, tokens := github.take(); len(tokens) != 2 {
		t.Fatalf("%d token requests, want the read set with actions and the one again without", len(tokens))
	}
	if revoked := github.takeRevoked(); len(revoked) != 1 {
		t.Fatalf("revokes = %q, want the one token the check minted given back", revoked)
	}
	if strings.Contains(logged.String(), "does not hold actions") {
		t.Fatalf("log = %q; want no line about a token released without actions, since a check releases none", logged.String())
	}
}
