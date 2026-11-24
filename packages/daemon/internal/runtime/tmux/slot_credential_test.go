package tmux

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Under tmux a pane's gh files are its claim's own directory, which nothing stands between: while
// the runtime's ActsAs gives the architect the implementer, its owner doing that step, the write
// of the architect's pane leaves its hosts.yml holding the implement App's token, and the pane is
// said to hold it (SlotHoldsCredential answers nil); once ActsAs gives the architect no other role,
// as when its step ended and no other act of the owner's runs, the write puts the review App's
// back, and the pane is said to hold that.
func TestASlotSaysWhenItsFilesHoldTheCredentialUnderTmux(t *testing.T) {
	ctx := context.Background()
	g := newOwnerPaneRig(t)
	if _, err := g.Spawn(ctx, g.spec); err != nil {
		t.Fatalf("Spawn of the architect: %v", err)
	}
	for _, tc := range []struct {
		acting, role claim.Role
		want         string
	}{
		{claim.RoleImplementer, claim.RoleImplementer, staticCredentialToken(claim.RoleImplementer)},
		{"", claim.RoleArchitect, staticCredentialToken(claim.RoleArchitect)},
	} {
		g.acting.Store(tc.acting)
		if err := g.WriteSlotCredential(ctx, g.spec.Claim); err != nil {
			t.Fatalf("WriteSlotCredential(the architect) while it does %q = %v", tc.acting, err)
		}
		if got := g.held(t); got != tc.want {
			t.Fatalf("the architect's pane holds %q after the write while it does %q, want %q", got, tc.acting, tc.want)
		}
		if err := g.SlotHoldsCredential(ctx, g.spec.Claim, tc.role); err != nil {
			t.Fatalf("SlotHoldsCredential(the architect, %s) = %v, want nil", tc.role, err)
		}
	}
}

// The App a pane is written for on request is the App of the step's role on the pane's own issue:
// LEGION-43's plan defines coder of the type implementation and prober of the type testing, and
// WriteSlotCredential of LEGION-43's architect while it does coder writes the implement App's lease
// and while it does prober the review App's.
func TestASlotCredentialIsTheAppOfTheStepsRoleOnItsIssueUnderTmux(t *testing.T) {
	ctx := context.Background()
	widgets := ghrepo.MustParse("acme/widgets")
	architect, err := claim.NewToken("omp", "LEGION-43", claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	var acting atomic.Value
	apps := appauth.RoleApps{Plan: planOf43}
	r := &Runtime{
		stateDir:             t.TempDir(),
		gitHubCredential:     appauth.GitHubCredential(appLeaseTokens{}, servesOneOf(widgets), ghrepo.Renames{}, nil, nil, apps, slog.Default()),
		roleApps:             apps,
		controllerCredential: staticControllerCredential,
		claimRepository:      acmeWidgets,
		actsAs: func(_ context.Context, token claim.Token) (claim.Role, bool, error) {
			return acting.Load().(claim.Role), token == architect, nil
		},
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		tracked:     map[string]*trackedProcess{},
		writeBundle: ghconfig.WriteBundle,
	}
	r.track(runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: architect, Incarnation: "4242:1",
		Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}, "LEGION-43", widgets, "acme", claim.RoleArchitect)
	dir := ghconfig.Dir(runtime.GHConfigDir(r.stateDir, architect), "acme")
	for _, tc := range []struct {
		role claim.Role
		want string
	}{
		{"coder", "ghs_implement_acme"},
		{"prober", "ghs_review_acme"},
		{"coder", "ghs_implement_acme"},
	} {
		acting.Store(tc.role)
		if err := r.WriteSlotCredential(ctx, architect); err != nil {
			t.Fatalf("WriteSlotCredential(the architect of LEGION-43) while it does %s = %v", tc.role, err)
		}
		if got := readHostsToken(t, dir); got != tc.want {
			t.Fatalf("the architect's pane holds %q after the write while it does %s, want %q", got, tc.role, tc.want)
		}
	}
}
