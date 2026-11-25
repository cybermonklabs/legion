package daemon

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// noPhaseRows is a record.PhaseReader of an issue that has no phases rows, whose every role's
// worker is that role's own claim (record.WorkerClaim).
type noPhaseRows struct{}

func (noPhaseRows) Phases(context.Context, pgx.Tx, string) ([]record.PhaseRow, error) {
	return nil, nil
}

// A phase worker holds a member's phase, and only a phase worker does (phaseHolder): admitted,
// which phase.RoleFor answers the tree's architect for since #3711, has no phase holder, since a
// member there has not started its work and its first phase's start carries its news; accepting,
// which the owning architect holds, is named through newsClaims.holderOf and not here; a phase no
// role works names none. A member in reviewing names its reviewer's claim, the worker of its role
// where the issue's phases rows name no step's agent.
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
		holder, held, err := phaseHolder(context.Background(), nil, noPhaseRows{}, "legion", record.Issue{Key: "LEGION-801", Phase: tc.phase})
		if err != nil {
			t.Fatalf("the holder of a member in %s: %v", tc.phase, err)
		}
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

// The claim that holds a member's phase is the claim of the agent that works the step of the
// phase's role (record.WorkerClaim), not that role's own claim. Here the agent of a key, started
// as the implementer, works the tester's step in its own claim, and the tester's own claim of an
// earlier version of the plan stands beside it, given the same entry. The member is owed the
// spec's news: the keyed agent's claim is named its holder, the reconcile queues the member's
// news-only start, and that start hands the news to the keyed agent's claim. The tester's own
// claim is told nothing.
func TestADesignNewsOfAStepAKeyedAgentWorksReachesThatAgentsClaim(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	root := r.member(reconcileRoot, "", phase.Testing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	own := r.holder(reconcileRoot, claim.RoleTester, supervise.StateIdle, "", 1)
	keyed := mustClaimToken(t, reconcileRoot, claim.RoleImplementer)
	r.inTx(func(tx pgx.Tx) error {
		if err := r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: reconcileRoot, Role: claim.RoleTester, Claim: keyed, StepTask: "t",
			StepPlace: 2, Entry: 1, EntryTask: "Work the phase.", EntryRaisedAt: time.Now().UTC()}); err != nil {
			return err
		}
		_, err := tx.Exec(r.ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak, given_entry, suspended_by)
			values ($1, 'legion', $2, $3, $4, 1, 'ses-keyed', '/sessions/keyed.jsonl', $5, 1, 0, 0, 0, 0, 1, '')`,
			string(keyed), reconcileRoot, reconcileRoot, string(claim.RoleTester), string(supervise.StateIdle))
		return err
	})

	member := toldKey{reconcileRoot, record.AudienceHolder}
	var holders map[toldKey]owedHolder
	r.inTx(func(tx pgx.Tx) error {
		var err error
		holders, err = readOwedHolders(r.ctx, tx, r.records, newNewsClaims(root, nil), []toldKey{member})
		return err
	})
	holder, held := holders[member]
	if !held || holder.token != keyed || holder.role != claim.RoleTester || holder.entry != 1 {
		t.Fatalf("the holder of %s in testing = %+v (held %t), want the keyed agent's claim %s on the tester's step at entry 1, not the tester's own claim %s",
			reconcileRoot, holder, held, keyed, own)
	}
	if told := holderTold(holder.run, holder.entry); told != workflow.NewsSentNow {
		t.Fatalf("the keyed agent's claim is told %q, want %q: it runs at its step's entry and holds no task", told, workflow.NewsSentNow)
	}

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}
	starts := r.newsStarts()
	want := record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleTester, Generation: 1, Phase: phase.Testing, News: true}
	if len(starts) != 1 || starts[0].row.Issue != reconcileRoot || !reflect.DeepEqual(starts[0].request, want) {
		t.Fatalf("the news-only starts = %+v, want one %+v on %s", starts, want, reconcileRoot)
	}

	ownMachine := r.machine(reconcileRoot, claim.RoleTester, supervise.StateIdle, "", 1)
	keyedMachine, _, err := r.sup.Create(r.ctx, supervise.Claim{Token: keyed, Project: "legion", Tree: reconcileRoot, Issue: reconcileRoot,
		Role: claim.RoleTester, State: supervise.StateIdle, Generation: 1, Session: "ses-keyed", SessionFile: "/tmp/keyed.jsonl",
		GivenEntry: 1}, "")
	if err != nil {
		t.Fatalf("create the keyed agent's claim: %v", err)
	}
	if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
		t.Fatalf("run the news-only start: %v", err)
	}
	if _, news := pendingNews(t, keyedMachine); !keyedMachine.Claim().Pending.NewsOnly ||
		!reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileRoot, 4)}) {
		t.Fatalf("the keyed agent's task carries %+v, want the news-only item of the spec from 4 to 5", news)
	}
	ownMachine.Wait()
	if pending := ownMachine.Claim().Pending; pending != nil {
		t.Fatalf("the tester's own claim holds %+v, want nothing: the keyed agent works the tester's step", pending)
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
