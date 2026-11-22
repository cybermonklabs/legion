package api

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A boot token of a launch the claim has since replaced resolves stale, with the generation it was
// minted for, after a daemon restart as well as before it (R3): the store keeps the previous
// launches' hashes, so the worker stream tells that older shim to stop rather than reading its
// token as one no launch minted.
func TestAReplacedLaunchsTokenResolvesStaleAfterARestart(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	token := claim.Token("legion-LEGION-209-implementer")
	c := supervise.Claim{
		Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("first launch"),
		Locator: &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "1:1",
			Tmux: &runtime.TmuxLocator{Window: "w", Pane: "%1"}},
	}
	written := NewBootTokens(st).Recording(st)
	if err := written.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the first launch: %v", err)
	}
	c.Generation, c.BootTokenHash = 2, supervise.HashBootToken("second launch")
	if err := written.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the second launch: %v", err)
	}

	restarted, err := store.Open(ctx, st.Pool().Config().ConnString())
	if err != nil {
		t.Fatalf("open a second store over the same database: %v", err)
	}
	t.Cleanup(restarted.Close)
	tokens := NewBootTokens(restarted)

	old, found, err := tokens.Resolve(ctx, "first launch")
	if err != nil || !found {
		t.Fatalf("the first launch's token after a restart: found %t, %v, want it known", found, err)
	}
	if old != (BootToken{Claim: token, Generation: 1, Stale: true}) {
		t.Fatalf("the first launch's token resolves to %+v, want generation 1 of %s, stale", old, token)
	}
	current, found, err := tokens.Resolve(ctx, "second launch")
	if err != nil || !found || current != (BootToken{Claim: token, Generation: 2}) {
		t.Fatalf("the current launch's token resolves to %+v (found %t, %v), want generation 2, not stale", current, found, err)
	}
	if _, found, err := tokens.Resolve(ctx, "a token no launch minted"); err != nil || found {
		t.Fatalf("a token no launch minted resolved: found %t, %v", found, err)
	}
}
