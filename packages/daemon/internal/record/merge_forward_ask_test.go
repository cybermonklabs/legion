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

// The merger's ask tells it that the merge and its conflict are its own and that the reviewer is
// asked to approve the head it pushes (cybermonklabs/legion#4979); the landed message, not the
// ask, holds its merge to that approval. The implementer's ask says none of that: an ask of the
// implementer that a daemon before that change queued in merging still reads as it did.
func TestTheMergersAskSaysTheMergeIsItsOwn(t *testing.T) {
	merger := MergeForwardMessage{Role: claim.RoleMerger, Step: MergeForwardAsk, Head: "c0de", Base: "main", Phase: phase.Merging,
		PullRequest: 42, Why: ConflictMergeNext}
	said := merger.Message("LEGION-208", "sjawhar/legion")
	for _, want := range []string{
		"LEGION-208: pull request sjawhar/legion#42 at head c0de conflicts with its base branch main",
		"The issue stays in merging.",
		"Merge main forward in the issue workspace",
		"The merge and its conflict are yours to resolve, as your role says.",
		"the daemon asks the reviewer to read it and approve the new head.",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the merger's ask reads %q, want it to say %q", said, want)
		}
	}
	implementer := merger
	implementer.Role = claim.RoleImplementer
	if said := implementer.Message("LEGION-208", "sjawhar/legion"); strings.Contains(said, "yours to resolve") || strings.Contains(said, "approve the new head") {
		t.Fatalf("the implementer's ask in merging reads %q, want none of the merger's sentences", said)
	}
}
