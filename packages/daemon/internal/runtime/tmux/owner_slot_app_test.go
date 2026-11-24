package tmux

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
)

// While its owner does a step, the architect's pane holds the App of that step: the test's
// credential names its App in its token (staticCredentialToken). A spawn of the architect while
// the runtime's ActsAs answers the implementer for it writes the implement App's token into its gh
// files; once ActsAs answers none, the step having ended, a refresh tick writes the review App's,
// the architect's own; with ActsAs answering the implementer again the next tick writes the
// implement App's, and with ActsAs answering acceptance, the owner's accepting step, the review
// App's, the App of the role acceptance, over the implement App's.
func TestTheOwnersPaneHoldsTheAppOfItsStep(t *testing.T) {
	ctx := context.Background()
	g := newOwnerPaneRig(t)
	g.acting.Store(claim.RoleImplementer)
	implement, review := staticCredentialToken(claim.RoleImplementer), staticCredentialToken(claim.RoleArchitect)

	if _, err := g.Spawn(ctx, g.spec); err != nil {
		t.Fatalf("Spawn of the architect: %v", err)
	}
	if got := g.held(t); got != implement {
		t.Fatalf("the architect's pane holds %q after its spawn while its owner does the implementer step, want %q", got, implement)
	}

	for _, tick := range []struct {
		acting claim.Role
		want   string
		while  string
	}{
		{"", review, "once its owner's step ended"},
		{claim.RoleImplementer, implement, "while its owner does the implementer step again"},
		{claim.RoleAcceptance, review, "while its owner does the accepting step"},
	} {
		g.acting.Store(tick.acting)
		g.refreshGitHubCredentials(ctx)
		if got := g.held(t); got != tick.want {
			t.Fatalf("the architect's pane holds %q after a tick %s, want %q", got, tick.while, tick.want)
		}
	}
}

// The line a launch logs of the credential it wrote names the role whose App the pane holds, not
// only the claim's: a spawn of the architect while its owner does the implementer step logs the
// implementer with the implement App, and the claim's own role, the architect, beside it. A line
// naming the architect with the implement App would read as the architect's own role acting as
// that App. The pane holds that App only while its owner does an act whose role acts as it: a step,
// the retro of its own implementation step, or a merge forward asked of it.
func TestTheOwnersLaunchLogsTheRoleItsPaneActsAs(t *testing.T) {
	var logs strings.Builder
	f := newFakeServerRuntimeWith(t, staticCredential, slog.New(slog.NewTextHandler(&logs, nil)), func(o *Options) {
		o.ActsAs = func(context.Context, claim.Token) (claim.Role, bool, error) { return claim.RoleImplementer, true, nil }
	})
	spec := f.spec(t, claim.RoleArchitect)
	if _, err := f.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn of the architect: %v", err)
	}
	// The launch's line is the one that names the claim's role beside the role its pane acts as.
	written := logLines(logs.String(), "claimRole=")
	if len(written) != 1 {
		t.Fatalf("the launch logged %d written credentials %q, want one", len(written), written)
	}
	for _, want := range []string{"role=" + string(claim.RoleImplementer), "claimRole=" + string(claim.RoleArchitect), "app=" + string(appauth.Implement)} {
		if !strings.Contains(written[0], want) {
			t.Errorf("the launch logged %q, want %q in it", written[0], want)
		}
	}
}
