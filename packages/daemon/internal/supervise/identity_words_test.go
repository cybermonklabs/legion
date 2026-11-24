package supervise

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// notALogin is githubrest.NotALogin, written out so the row pins the text.
const notALogin = "(a login GitHub does not allow)"

// TestAnAdoptionLogsTheAppsGitNameOnlyInItsShape: the role's App identity names its git name as
// the bot login an App read of GitHub answered, and something in GitHub's place echoed an
// installation token there. The adoption's line names that identity only as a login GitHub
// allows, here its fixed phrase, and holds no run of the token. Captures
// supervise.answer-words/adopted-identity.
func TestAnAdoptionLogsTheAppsGitNameOnlyInItsShape(t *testing.T) {
	bot := runtime.GitIdentity{Name: githubtest.InstallationToken(t.Name()) + "-RDX-", Email: "7+bot@users.noreply.github.com"}
	h := newHarness(t)
	h.deps.Identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) { return bot, nil }
	h.restart()
	h.reach(StateReady)

	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 5})
	h.wantPrompts(1)

	adopted := h.records.of(adoptedLine)
	if len(adopted) != 1 {
		t.Fatalf("%q was written %d times, want once", adoptedLine, len(adopted))
	}
	got, ok := adopted[0].attr("identity")
	if !ok {
		t.Fatalf("%q carries no identity", adoptedLine)
	}
	oauth2calltest.RequireNoPiece(t, "supervise.answer-words/adopted-identity", got.String(), bot.Name)
	if got.String() != notALogin {
		t.Errorf("%q carries identity %q, want %q", adoptedLine, got.String(), notALogin)
	}
}
