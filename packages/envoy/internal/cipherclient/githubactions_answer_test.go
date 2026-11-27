package cipherclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
)

// TestGitHubActionsIdentityReadsItsAnswerThroughTheAnswerReader: GitHub's answer, which carries
// the run's OIDC token, is read through redact.ReadAnswer, at most a MiB of it. An answer past the
// MiB is refused (redact.ErrAnswerTooLarge) and answers no token, never cut at the MiB and read as
// if whole (past-limit), though its first MiB is a whole answer of a token followed by spaces; the
// same answer within the MiB answers its token (within-limit).
func TestGitHubActionsIdentityReadsItsAnswerThroughTheAnswerReader(t *testing.T) {
	const runToken = "eyJhbGciOiJSUzI1NiJ9.a-run-token.sig"
	for _, x := range []struct {
		name    string
		padding int
		refused bool
	}{
		{"past-limit", 1 << 20, true},
		{"within-limit", 1<<20 - 64, false},
	} {
		t.Run(x.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"value":"`+runToken+`"}`)
				_, _ = io.WriteString(w, strings.Repeat(" ", x.padding))
			}))
			t.Cleanup(server.Close)
			identity := &GitHubActionsIdentity{RequestURL: server.URL + "/token?api-version=2.0", RequestToken: actionsRequestToken, Audience: "the-broker", HTTP: server.Client()}
			token, _, err := identity.Token(context.Background())
			switch {
			case x.refused && (!errors.Is(err, redact.ErrAnswerTooLarge) || token != ""):
				t.Errorf("Token of an answer past a MiB answered %q, %v; want no token and redact.ErrAnswerTooLarge", token, err)
			case !x.refused && (err != nil || token != runToken):
				t.Errorf("Token of an answer within a MiB answered %q, %v; want its token", token, err)
			}
		})
	}
}
