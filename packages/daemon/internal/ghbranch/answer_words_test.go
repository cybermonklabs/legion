package ghbranch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// notABranch is githubrest.NotABranch, written out so the row pins the text.
const notABranch = "(a branch name the daemon does not print)"

// echoed is a member of GitHub's answer, or of whatever answers in GitHub's place, that echoes the
// installation token the read carried and quotes a URL carrying made-up credentials.
var echoed = "token " + installationToken + " refused; " + oauth2calltest.Words

// TestABaseThatIsNoBranchNameIsNamedByAFixedPhrase: the daemon records a repository's default
// branch as GitHub's answer named it (Read), and something in GitHub's place echoed the read's
// installation token there. Create is then asked to start an issue's branch at that base, and the
// read of the base is answered 404 (refused), answered with no commit (no-commit), or gets no
// answer at all (unanswered). Each refusal names the base by its fixed phrase (notABranch), in its
// words and in the read's own name, and holds no run of the token. The control's base, a branch's
// name, prints as it came. Captures ghbranch.answer-words/base-<case>.
func TestABaseThatIsNoBranchNameIsNamedByAFixedPhrase(t *testing.T) {
	refused := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found","status":"404"}`)
	}
	for _, tc := range []struct {
		name, base, named string
		answer            http.HandlerFunc
	}{
		{"refused", installationToken + "-RDX-", notABranch, refused},
		{"no-commit", installationToken + "-RDX-", notABranch, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"type":"commit"}}`)
		}},
		{"unanswered", installationToken + "-RDX-", notABranch, func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack the base's read: %v", err)
				return
			}
			_ = conn.Close()
		}},
		{"control", "release/v7.1", "release/v7.1", refused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.answer)
			t.Cleanup(server.Close)
			err := Create(context.Background(), widgets(server.URL), ghrepo.MustParse("acme/widgets"), "legion/LEGION-12", tc.base, nil)
			if err == nil {
				t.Fatal("Create answered no error, though the base's read failed")
			}
			oauth2calltest.RequireNoPiece(t, "ghbranch.answer-words/base-"+tc.name, err.Error(), installationToken)
			if want := "read refs/heads/" + tc.named + " of acme/widgets"; !strings.Contains(err.Error(), want) {
				t.Errorf("Create error = %q, want it to say %q", err, want)
			}
		})
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
		{"echo", echoed, "create refs/heads/legion/LEGION-12 on acme/widgets at (a sha that is not a commit's): GitHub answered POST /git/refs with 403"},
		{"control", baseCommit, "create refs/heads/legion/LEGION-12 on acme/widgets at " + baseCommit + ": GitHub answered POST /git/refs with 403"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := &github{refs: map[string]string{"main": c.commit}, create: func() (int, string) {
				return http.StatusForbidden, `{"message":"Resource not accessible by integration","status":"403"}`
			}}
			err := Create(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets"), "legion/LEGION-12", "main", nil)
			if err == nil {
				t.Fatal("Create answered no error for a create GitHub refused")
			}
			oauth2calltest.RequireNoPiece(t, "ghbranch.answer-words/create-"+c.name, err.Error(), installationToken)
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
	oauth2calltest.RequireNoPiece(t, "ghbranch.answer-words/full-name", err.Error(), installationToken)
	if !strings.Contains(err.Error(), "full_name") || !strings.Contains(err.Error(), "acme/widgets") {
		t.Errorf("Read's error is %q, want it to name full_name and acme/widgets", err)
	}
}
