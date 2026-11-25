package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A phase worker holds a member's phase, and only a phase worker does (phaseHolder): admitted,
// which phase.RoleFor answers the tree's architect for since #3711, has no phase holder, since a
// member there has not started its work and its first phase's start carries its news; accepting,
// which the owning architect holds, is named through newsClaims.holderOf and not here; a phase no
// role works names none. A member in reviewing names its reviewer's claim.
func TestOnlyAPhaseWorkerHoldsAMembersPhase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		phase phase.Phase
		role  claim.Role
	}{
		{phase.Admitted, ""},
		{phase.Accepting, ""},
		{phase.AwaitingPerson, ""},
		{phase.Reviewing, claim.RoleReviewer},
		{phase.Merging, claim.RoleMerger},
	} {
		holder, held := phaseHolder("legion", record.Issue{Key: "LEGION-801", Phase: tc.phase})
		if held != (tc.role != "") || held && holder.role != tc.role {
			t.Fatalf("the holder of a member in %s = %+v (held %t), want role %q", tc.phase, holder, held, tc.role)
		}
		if held {
			if want, _ := claim.NewToken("legion", "LEGION-801", tc.role); holder.token != want {
				t.Fatalf("the holder of a member in %s is %s, want %s", tc.phase, holder.token, want)
			}
		}
	}
}

// The holder of a member's phase is named from the member's record (newsClaims.member). A caller
// that read none of the tree's issues hands none over (newNewsClaims), and a member is then read
// from the record with the rest of the tree, so the tester's claim is named for a member in
// testing, and that claim is told by a task now. Without that read the member would be the zero
// issue, in no phase, which names no holder and reads as told at its next start. A key the record
// holds no issue of in the tree is an error, never a member told at its next start.
func TestAMemberIsNamedByItsRealPhaseWhenItsCallerHandedNoIssueOver(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	root := r.member(reconcileRoot, "", phase.Implementing)
	r.member(reconcileChild, reconcileRoot, phase.Testing)
	tester := r.holder(reconcileChild, claim.RoleTester, supervise.StateIdle, "", 1)
	child := toldKey{reconcileChild, record.AudienceHolder}

	var holders map[toldKey]owedHolder
	r.inTx(func(tx pgx.Tx) error {
		var err error
		holders, err = readOwedHolders(r.ctx, tx, r.records, newNewsClaims(root, nil), []toldKey{child})
		return err
	})
	holder, held := holders[child]
	if !held || holder.token != tester || holder.issue.Phase != phase.Testing || holder.entry != 1 {
		t.Fatalf("the holder of %s, which no caller handed over = %+v (held %t), want its tester's claim %s at entry 1",
			reconcileChild, holder, held, tester)
	}
	if told := holderTold(holder.run, holder.entry); told != workflow.NewsSentNow {
		t.Fatalf("the tester of %s is told %q, want %q: its claim runs at its phase's entry and holds no task", reconcileChild, told, workflow.NewsSentNow)
	}

	const unknown = "LEGION-699"
	err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := readOwedHolders(ctx, tx, r.records, newNewsClaims(root, nil),
			[]toldKey{{unknown, record.AudienceHolder}})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "the record holds no issue "+unknown) {
		t.Fatalf("the holder of %s, which the record does not hold = %v, want an error naming it", unknown, err)
	}
}

// The owner walk of a member in accepting reads every issue of the tree (newsClaims.owningIssue),
// since a caller hands over the facts it read of the tree, whole, or nothing (newNewsClaims). So a
// member in accepting whose parent's own architect holds its role is held by that architect, never
// by the root's, whether the walk reads the tree itself, asked first with nothing handed over, or
// reads the facts its caller handed over.
func TestAMemberInAcceptingIsHeldByItsParentsArchitectWhateverItsCallerHandedOver(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	root := r.member(reconcileRoot, "", phase.Implementing)
	r.member(reconcileChild, reconcileRoot, phase.Implementing)
	accepting := r.member(reconcileGrandchild, reconcileChild, phase.Accepting)
	r.holder(reconcileRoot, claim.RoleArchitect, supervise.StateWorking, "", 0)
	own := r.holder(reconcileChild, claim.RoleArchitect, supervise.StateWorking, "", 0)

	for _, handed := range []bool{false, true} {
		var holder newsHolder
		var held bool
		r.inTx(func(tx pgx.Tx) error {
			var facts *record.NewsFacts
			if handed {
				read, err := record.TreeNewsFacts(r.ctx, r.records, tx, config.DesignGateRootIssues.Steps(), reconcileRoot)
				if err != nil {
					return err
				}
				facts = &read
			}
			var err error
			holder, held, err = newNewsClaims(root, facts).holderOf(r.ctx, tx, r.records, accepting)
			return err
		})
		if !held || holder.token != own || holder.issue.Key != reconcileChild || holder.role != claim.RoleArchitect {
			t.Fatalf("the holder of %s in accepting, the facts handed over %t = %+v (held %t), want its parent's architect %s",
				reconcileGrandchild, handed, holder, held, own)
		}
	}
}

// A member in accepting that no sub-architect owns is held by the tree's root architect, which
// answers its news itself: the root architect's summary says so (workflow.NewsYouAnswer), and never
// that a task tells it or that its next start carries it.
func TestTheRootArchitectsSummarySaysItAnswersAMemberInAcceptingItOwns(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Implementing)
	r.member(reconcileChild, reconcileRoot, phase.Accepting)
	r.told(reconcileRoot, record.AudienceHolder, 5)
	r.told(reconcileChild, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleArchitect, supervise.StateWorking, "", 0)

	var drafts []memberDraft
	r.inTx(func(tx pgx.Tx) error {
		root, err := r.records.Issue(r.ctx, tx, reconcileRoot)
		if err != nil {
			return err
		}
		drafts, err = readTreeMembers(r.ctx, tx, r.records, config.DesignGateRootIssues.Steps(), newNewsClaims(*root, nil))
		return err
	})
	for _, draft := range drafts {
		if draft.issue.Key == reconcileChild {
			if draft.state != workflow.NewsYouAnswer {
				t.Fatalf("the summary tells %s %q, want %q: the root architect holds it in accepting", reconcileChild, draft.state, workflow.NewsYouAnswer)
			}
			return
		}
	}
	t.Fatalf("the summary's members = %+v, want a line for %s", drafts, reconcileChild)
}
