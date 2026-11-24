package sandbox

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// TestAnAdoptionTheShimRefusesNamesTheAppsGitNameOnlyInItsShape: the role's App identity names its
// git name as the bot login an App read of GitHub answered, and something in GitHub's place
// echoed an installation token there, which its noreply email holds too. The shim refuses the
// adoption, and the refusal names the identity only as a login GitHub allows, here its fixed
// phrase, and holds no run of the token. Captures sandbox.answer-words/adopt-identity.
func TestAnAdoptionTheShimRefusesNamesTheAppsGitNameOnlyInItsShape(t *testing.T) {
	g := newRig(t, nil)
	conn := fake.NewConn()
	g.conns.Register(workerToken, conn)
	loc := g.spawn(workerSpec(t))
	bot := githubtest.InstallationToken(t.Name()) + "-RDX-"
	conn.FailAdoptWorkingCopy(errors.New("jj metaedit exited 1"))

	err := g.r.AdoptWorkingCopy(g.ctx, loc, runtime.GitIdentity{Name: bot, Email: "7+" + bot + "@users.noreply.github.com"})
	if err == nil {
		t.Fatal("the adoption answered no error, though the shim refused it")
	}
	oauth2calltest.RequireNoPiece(t, "sandbox.answer-words/adopt-identity", err.Error(), bot)
	if want := "working copy for (a login GitHub does not allow): jj metaedit exited 1"; !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal is %q, want it to say %q", err, want)
	}
}
