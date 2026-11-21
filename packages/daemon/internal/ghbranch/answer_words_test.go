package ghbranch

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/testreason"
)

// echoed is a member of GitHub's answer, or of whatever answers in GitHub's place, that echoes the
// installation token the read carried and quotes a URL carrying made-up credentials.
var echoed = "token " + installationToken + " refused; " + oauth2calltest.Words

// requireNoEcho is testreason.RequireNone of text under capture, and fails t where text holds a run
// of eight characters of the installation token.
func requireNoEcho(t *testing.T, capture, text string) {
	t.Helper()
	testreason.RequireNone(t, capture, text)
	for at := 0; at+8 <= len(installationToken); at++ {
		if strings.Contains(text, installationToken[at:at+8]) {
			t.Errorf("%s: the output prints the installation token: %q", capture, text)
			return
		}
	}
}

// TestACreateNamesTheBasesCommitOnlyInItsShape: GitHub answers the read of the base's ref with a
// commit that is no sha, echoing the token and the probe, and refuses the create. The create's
// error names the commit as "(a sha that is not a commit's)" (echo); the control's sha prints as
// it came. Captures ghbranch.answer-words/create-<case>.
func TestACreateNamesTheBasesCommitOnlyInItsShape(t *testing.T) {
	for _, c := range []struct {
		name, commit, want string
	}{
		{"echo", echoed, "create refs/heads/legion/WIDGETS-12 on acme/widgets at (a sha that is not a commit's): GitHub answered POST /git/refs with 403"},
		{"control", baseCommit, "create refs/heads/legion/WIDGETS-12 on acme/widgets at " + baseCommit + ": GitHub answered POST /git/refs with 403"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := &github{refs: map[string]string{"main": c.commit}, create: func() (int, string) {
				return http.StatusForbidden, `{"message":"Resource not accessible by integration","status":"403"}`
			}}
			err := Create(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", "main")
			if err == nil {
				t.Fatal("Create answered no error for a create GitHub refused")
			}
			requireNoEcho(t, "ghbranch.answer-words/create-"+c.name, err.Error())
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("Create's error is %q, want %q in it", err, c.want)
			}
		})
	}
}

// TestAReadRefusesAFullNameNamingNoneOfIt: GitHub answers the repository's read with a full_name
// that is no owner/name, echoing the token and the probe. Read refuses it, and its error names the
// field and the repository asked for, and nothing the answer wrote there. Captures
// ghbranch.answer-words/full-name.
func TestAReadRefusesAFullNameNamingNoneOfIt(t *testing.T) {
	g := &github{defaultBranch: "main", fullName: echoed}
	_, err := Read(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets"))
	if err == nil {
		t.Fatal("Read answered no error for a full_name that is no owner/name")
	}
	requireNoEcho(t, "ghbranch.answer-words/full-name", err.Error())
	if !strings.Contains(err.Error(), "full_name") || !strings.Contains(err.Error(), "acme/widgets") {
		t.Errorf("Read's error is %q, want it to name full_name and acme/widgets", err)
	}
}
