package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// While its owner does a step, the architect's slot holds the App of that step: the rig's
// credential names its App in its token (staticCredentialToken). With the runtime's ActsAs
// answering the implementer for the architect of LEGION-208, a refresh tick writes the architect's
// slot with the implement App's token; with ActsAs answering acceptance, the owner's accepting
// step, the next tick writes the review App's, the App of the role acceptance, over the implement
// App's; once ActsAs answers none, the step having ended, a tick writes the review App's again
// after the implement App's; and a launch of the architect into the kept pod while ActsAs answers
// the implementer writes the implement App's token, its start frame naming the digest of that
// token.
func TestTheOwnersSlotHoldsTheAppOfItsStep(t *testing.T) {
	var acting atomic.Value
	g := newRig(t, nil, actingOwner(&acting))
	acting.Store(claim.RoleImplementer)
	g.spawn(rootSpec(t))
	issue := SandboxName(rootToken)
	before := g.pod(issue).UID
	storeOwner(g)
	held := func() string {
		t.Helper()
		return slotToken(t, g, issue, claim.RoleArchitect)
	}
	implement, review := staticCredentialToken(claim.RoleImplementer), staticCredentialToken(claim.RoleArchitect)

	for _, tick := range []struct {
		acting claim.Role
		want   string
		while  string
	}{
		{claim.RoleImplementer, implement, "while its owner does the implementer step"},
		{claim.RoleAcceptance, review, "while its owner does the accepting step"},
		{claim.RoleImplementer, implement, "while its owner does the implementer step again"},
		{"", review, "once its owner's step ended"},
	} {
		acting.Store(tick.acting)
		g.r.refreshGitHubCredentials(g.ctx)
		if got := held(); got != tick.want {
			t.Fatalf("the architect slot holds %q after a tick %s, want %q", got, tick.while, tick.want)
		}
	}

	acting.Store(claim.RoleImplementer)
	again := rootSpec(t)
	again.Generation++
	g.spawn(again)
	if after := g.pod(issue).UID; after != before {
		t.Fatalf("the architect's launch replaced the pod (%s, then %s), want it kept", before, after)
	}
	if got := held(); got != implement {
		t.Fatalf("the architect slot holds %q after its launch while its owner does the implementer step, want %q", got, implement)
	}
	sum := sha256.Sum256([]byte(implement))
	if start := lastStart(t, g, rootToken); start.GitHubCredential == nil || start.GitHubCredential.TokenDigest != hex.EncodeToString(sum[:]) {
		t.Fatalf("the architect's start frame names the credential %+v, want the digest of %q", start.GitHubCredential, implement)
	}
}
