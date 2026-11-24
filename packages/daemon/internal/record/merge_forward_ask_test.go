package record

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// In merging the merge is the merger's, so the daemon asks the merger to merge the base forward,
// and in every phase before it the implementer (cybermonklabs/legion#4979). The ask the record keeps
// names the role asked; an ask a daemon before that change wrote names none, and it asked the
// implementer in every phase, so such an ask reads as the implementer's, merging included.
func TestAMergeForwardNamesTheRoleItAsks(t *testing.T) {
	for _, tc := range []struct {
		at   phase.Phase
		want claim.Role
	}{
		{phase.Implementing, claim.RoleImplementer},
		{phase.Testing, claim.RoleImplementer},
		{phase.Reviewing, claim.RoleImplementer},
		{phase.Accepting, claim.RoleImplementer},
		{phase.Retro, claim.RoleImplementer},
		{phase.Merging, claim.RoleMerger},
	} {
		if got := MergeForwardAsker(tc.at); got != tc.want {
			t.Errorf("MergeForwardAsker(%s) = %s, want %s", tc.at, got, tc.want)
		}
	}

	var before MergeForward
	if err := json.Unmarshal([]byte(`{"head":"c0de","base":"main","phase":"merging","why":"merge-next","askedAt":"2026-11-19T01:00:00Z"}`), &before); err != nil {
		t.Fatalf("decode an ask a daemon before the merger's asks wrote: %v", err)
	}
	if got := before.Asked(); got != claim.RoleImplementer {
		t.Fatalf("an ask that names no role reads as the %s's, want the implementer's: the daemon that wrote it asked the implementer in merging", got)
	}
	after := MergeForward{Head: "c0de", Base: "main", Role: claim.RoleMerger, Phase: phase.Merging, Why: ConflictMergeNext}
	encoded, err := json.Marshal(after)
	if err != nil {
		t.Fatalf("encode the merger's ask: %v", err)
	}
	var read MergeForward
	if err := json.Unmarshal(encoded, &read); err != nil || read.Asked() != claim.RoleMerger {
		t.Fatalf("the merger's ask %s reads back as the %s's (%v), want the merger's", encoded, read.Asked(), err)
	}
}

// The ask of a merge forward names its issue, its pull request, the head that conflicts and the
// base, whichever role is asked, and the role asked decides the rest of it: the merger's ask and
// the implementer's ask of the same merge forward are not one message.
func TestAMergeForwardAskNamesItsMergeAndDependsOnTheRoleAsked(t *testing.T) {
	merger := MergeForwardMessage{Role: claim.RoleMerger, Step: MergeForwardAsk, Head: "c0de", Base: "main", Phase: phase.Merging,
		PullRequest: 42, Why: ConflictMergeNext}
	implementer := merger
	implementer.Role = claim.RoleImplementer
	asks := map[claim.Role]string{}
	for _, ask := range []MergeForwardMessage{merger, implementer} {
		said := ask.Message("LEGION-208", PullRequest{Repo: "sjawhar/legion"})
		for _, value := range []string{"LEGION-208", "sjawhar/legion#42", "c0de", "main"} {
			if !strings.Contains(said, value) {
				t.Errorf("the %s's ask reads %q, want it to name %s", ask.Role, said, value)
			}
		}
		asks[ask.Role] = said
	}
	if asks[claim.RoleMerger] == asks[claim.RoleImplementer] {
		t.Fatalf("the merger's ask and the implementer's are both %q, want the role asked to decide the ask", asks[claim.RoleMerger])
	}
}
