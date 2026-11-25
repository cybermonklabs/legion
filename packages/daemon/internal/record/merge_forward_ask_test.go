package record

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/texts"
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

// The merger's ask is the merge-forward ask's record, with the facts of the implementer's: the
// pull request, the head that conflicts with the base, the base, that the head conflicts with it,
// that two reads of GitHub found it so, the phase the issue stays in, why the daemon acted and the
// branch the merge is pushed to. What
// the merger's ask said beside them (cybermonklabs/legion#4979) - that the merge and its conflict
// are its own to resolve, and that, where its task has a reviewer step, the reviewer is asked to
// read its merge when it lands and approve the new head - is a step its prompt gives
// (internal/prompts/go/merger.md), so the record holds neither sentence; the landed message, not
// the ask, holds its merge to that approval. An ask of the implementer that a daemon before that
// change queued in merging reads as the same record, on the implementer's own topic.
func TestTheMergersAskSaysTheMergeIsItsOwn(t *testing.T) {
	merger := MergeForwardMessage{Role: claim.RoleMerger, Step: MergeForwardAsk, Head: "c0de", Base: "main", Phase: phase.Merging,
		PullRequest: 42, Why: ConflictMergeNext}
	want := texts.MergeForwardAskRecord{RoleHead: texts.RoleHead{Kind: texts.MergeForwardAsk, Issue: "LEGION-208"}, PullRequest: "sjawhar/legion#42",
		Head: "c0de", ConflictsWith: "main", HeadConflictsWithBase: true, ConflictReads: texts.ConflictReadsConfirmed, Phase: phase.Merging,
		Why: string(ConflictMergeNext), PushTo: claim.IssueBranch("LEGION-208")}
	got := merger.Record("LEGION-208", PullRequest{Repo: "sjawhar/legion"})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the merger's ask is %+v, want %+v", got, want)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode the merger's ask: %v", err)
	}
	for _, step := range []string{"yours to resolve", "approve the new head"} {
		if strings.Contains(string(encoded), step) {
			t.Errorf("the merger's ask %s says %q, a step the merger's prompt gives", encoded, step)
		}
	}
	implementer := merger
	implementer.Role = claim.RoleImplementer
	if got := implementer.Record("LEGION-208", PullRequest{Repo: "sjawhar/legion"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("the implementer's ask in merging is %+v, want the same record as the merger's %+v", got, want)
	}
}
