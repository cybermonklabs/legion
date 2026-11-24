package githubapp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// A branch no read of GitHub named carries no answer's record (NamedBranch): the base branch an
// issue's pull request merged into, a branch read from the store, a name a caller holds. It is
// another party's words all the same — GitHub answered the pull request that named the base —
// so the comparison against it names it as the rule reads it, with its own token redacted.

// TestABranchACallerNamedPrintsAsTheRuleReadsIt: a caller names a branch that quotes a URL
// carrying a credential, or one that echoes the installation token this read carries, and the
// comparison against it is answered 500. Neither the error CommitOnBranch answers nor the note
// it leaves on the attempt holds the credential or a piece of the token. Captures
// githubapp.named-branch/<row>.
func TestABranchACallerNamedPrintsAsTheRuleReadsIt(t *testing.T) {
	const token = redacttest.Token
	for _, row := range []struct {
		name, branch string
		// piece is the credential the row's branch echoes, which the output may hold no run
		// of; empty where the branch carries no value of this request's.
		piece string
	}{
		{name: "credential", branch: redacttest.Words},
		{name: "echoes-token", branch: "release-" + token, piece: token},
	} {
		t.Run(row.name, func(t *testing.T) {
			client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/repos/acme/web/compare/") {
					githubtest.Unexpected(t, w, r)
					return
				}
				serverError(w, r)
			})
			ctx, attempt := githubreads.Begin(context.Background())
			_, err := client.CommitOnBranch(ctx, token, "acme", "web", NamedBranch(row.branch), mainCommit)
			if err == nil {
				t.Fatal("a comparison GitHub failed answered no error")
			}
			githubreads.NoteFailure(ctx, err)
			_, _, cause := attempt.Outcome()
			read := err.Error() + " | " + cause
			if row.piece == "" {
				redacttest.RequireNone(t, "githubapp.named-branch/"+row.name, read)
			} else {
				redacttest.RequireNoPiece(t, "githubapp.named-branch/"+row.name, read, row.piece)
			}
			if !strings.Contains(err.Error(), "release-") && !strings.Contains(err.Error(), redacttest.Shown) {
				t.Errorf("CommitOnBranch answered %q, want the branch named as the rule reads it", err)
			}
		})
	}
}

// TestABranchGitHubNamedPrintsFromItsOwnAnswer is the control of the two readings: a branch a
// read of GitHub named prints from that answer's record, and one a caller named prints as the
// rule reads it, so neither reading stands in for the other. Both readings leave an ordinary
// branch readable.
func TestABranchGitHubNamedPrintsFromItsOwnAnswer(t *testing.T) {
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/web" {
			githubtest.Unexpected(t, w, r)
			return
		}
		fmt.Fprint(w, `{"default_branch":"release/2026"}`)
	})
	read, err := client.DefaultBranch(context.Background(), "ghs_test", "acme", "web")
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if read.Name != "release/2026" || read.Shown() != "release/2026" {
		t.Errorf("a branch GitHub named = %q, printed as %q; want release/2026 both", read.Name, read.Shown())
	}
	named := NamedBranch("release/2026")
	if named.Name != "release/2026" || named.Shown() != "release/2026" {
		t.Errorf("a branch a caller named = %q, printed as %q; want release/2026 both", named.Name, named.Shown())
	}
}
