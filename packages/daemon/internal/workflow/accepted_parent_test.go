package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedBeneath records a child of parent in tree, standing in at with the Dispatch status its
// phase carries.
func seedBeneath(t *testing.T, pool *pgxpool.Pool, key, tree, parent string, at phase.Phase, status string) {
	t.Helper()
	above := parent
	seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: tree, Project: "LEGION", Title: key, Parent: &above, Phase: at, Generation: 1, Status: status, Rank: key})
}

// signOffNow applies the architect's sign_off of key and answers what the daemon made of it.
func signOffNow(t *testing.T, pool *pgxpool.Pool, id, key string) intake.Result {
	t.Helper()
	engine := testEngine(config.DesignGateRootIssues, nil)
	result, err := intake.ApplyFact(context.Background(), pool, "api", id, rootsSignOff(t, pool, engine, key), engine, admissionStub{})
	if err != nil {
		t.Fatalf("apply %s: %v", id, err)
	}
	return result
}

// A6. A parent is accepted after its children: sign_off is refused while any issue beneath the
// one being accepted is still in the workflow, naming each of them, and is taken once the last
// of them leaves, since a parent is accepted only once no issue beneath it is still in the
// workflow. A parent is accepted once its checks pass on a base commit that carries its
// children's merges, so accepting it first would send a head to the merge queue that no check ran
// with that work.
func TestSignOffIsRefusedWhileAnIssueBeneathIsInTheWorkflow(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedAccepting(t, pool)
	seedBeneath(t, pool, "LEGION-209", "LEGION-208", "LEGION-208", phase.Implementing, "in_progress")
	seedBeneath(t, pool, "LEGION-210", "LEGION-208", "LEGION-208", phase.Testing, "testing")
	seedBeneath(t, pool, "LEGION-211", "LEGION-208", "LEGION-208", phase.Done, "done")

	result := signOffNow(t, pool, "signoff-held", "LEGION-208")
	if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != "SIGNOFF_CHILD_IN_WORKFLOW" {
		t.Fatalf("sign-off with two issues beneath = %+v, want 409 SIGNOFF_CHILD_IN_WORKFLOW", result.Refusal)
	}
	for _, want := range []string{"LEGION-209", "LEGION-210"} {
		if !strings.Contains(result.Refusal.Message, want) {
			t.Fatalf("refusal %q does not name %s", result.Refusal.Message, want)
		}
	}
	if strings.Contains(result.Refusal.Message, "LEGION-211") {
		t.Fatalf("refusal %q names LEGION-211, which is done and holds nothing", result.Refusal.Message)
	}
	assertPhase(t, pool, phase.Accepting)

	// One leaves by its own close, the other by a person's park: either empties the set.
	leave(t, pool, "LEGION-209", phase.Done, "done")
	if result := signOffNow(t, pool, "signoff-one-left", "LEGION-208"); result.Refusal == nil || result.Refusal.Code != "SIGNOFF_CHILD_IN_WORKFLOW" {
		t.Fatalf("sign-off with one issue still beneath = %+v, want it refused again", result.Refusal)
	}
	leave(t, pool, "LEGION-210", phase.Admitted, "backlog")
	if result := signOffNow(t, pool, "signoff-accepted", "LEGION-208"); result.Refusal != nil {
		t.Fatalf("sign-off once the last issue beneath left = %+v, want it accepted", result.Refusal)
	}
	assertPhase(t, pool, phase.Retro)
}

// The sign_off and close_root routes judge an act in a read-only transaction before they write its
// status in Dispatch: SignOffRefusal and CloseRootRefusal read every hold their facts read, write
// nothing, and answer the refusal the fact itself would, nil where it would take the act and for
// an issue the daemon does not record.
func TestTheSignOffAndCloseJudgementsReadTheirFactsRefusalsReadOnly(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedAccepting(t, pool)
	seedBeneath(t, pool, "LEGION-209", "LEGION-208", "LEGION-208", phase.Implementing, "in_progress")
	engine := testEngine(config.DesignGateRootIssues, nil)
	signOffJudged := func(fact intake.SignOff) (*intake.Refusal, bool) {
		t.Helper()
		var refusal *intake.Refusal
		var accepts bool
		if err := pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
			var err error
			refusal, accepts, err = engine.SignOffRefusal(context.Background(), tx, fact)
			return err
		}); err != nil {
			t.Fatalf("judge the sign-off of %s read-only: %v", fact.Issue, err)
		}
		return refusal, accepts
	}
	closeJudged := func(fact intake.CloseRoot) *intake.Refusal {
		t.Helper()
		var refusal *intake.Refusal
		if err := pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
			var err error
			refusal, err = engine.CloseRootRefusal(context.Background(), tx, fact)
			return err
		}); err != nil {
			t.Fatalf("judge the close of %s read-only: %v", fact.Issue, err)
		}
		return refusal
	}
	rootsClose := func(key string) intake.CloseRoot {
		t.Helper()
		by, generation, _ := treeArchitectsRead(t, pool, engine, key)
		return intake.CloseRoot{Issue: key, Reason: "moot", By: by, Generation: generation}
	}

	if refusal, _ := signOffJudged(rootsSignOff(t, pool, engine, "LEGION-208")); refusal == nil || refusal.Code != "SIGNOFF_CHILD_IN_WORKFLOW" || !strings.Contains(refusal.Message, "LEGION-209") {
		t.Fatalf("the sign-off judgement with LEGION-209 beneath = %+v, want SIGNOFF_CHILD_IN_WORKFLOW naming it", refusal)
	}
	if refusal := closeJudged(rootsClose("LEGION-208")); refusal == nil || refusal.Code != "CLOSE_AFTER_PHASE_STARTED" {
		t.Fatalf("the close judgement of a root in accepting = %+v, want CLOSE_AFTER_PHASE_STARTED", refusal)
	}
	if refusal, _ := signOffJudged(rootsSignOff(t, pool, engine, "LEGION-209")); refusal == nil || refusal.Code != "SIGNOFF_OUTSIDE_ACCEPTING" {
		t.Fatalf("the sign-off judgement of an issue in implementing = %+v, want SIGNOFF_OUTSIDE_ACCEPTING", refusal)
	}
	if refusal, accepts := signOffJudged(intake.SignOff{Issue: "LEGION-999"}); refusal != nil || !accepts {
		t.Fatalf("the sign-off judgement of an issue the daemon does not record = %+v, accepts %v; want none, and the write left to the fact", refusal, accepts)
	}
	assertPhase(t, pool, phase.Accepting)
	leave(t, pool, "LEGION-209", phase.Done, "done")
	if refusal, accepts := signOffJudged(rootsSignOff(t, pool, engine, "LEGION-208")); refusal != nil || !accepts {
		t.Fatalf("the sign-off judgement once the issue beneath left = %+v, accepts %v; want none, and the sign-off accepting the issue", refusal, accepts)
	}
	if result := signOffNow(t, pool, "signoff-judged", "LEGION-208"); result.Refusal != nil {
		t.Fatalf("the sign-off the judgement takes = %+v, want it taken", result.Refusal)
	}
	assertPhase(t, pool, phase.Retro)
}

// The two judgements meet the fences every architect's request meets (requestRefusal) before
// anything of the act is written, as the fact itself does: a sign-off or a close by an architect of
// no tree of the issue, on a generation the issue left, or on a run of accepting that no longer
// stands, is refused as the fact would refuse it. Before this the routes judged the key alone, and
// such an act wrote its status in Dispatch first.
func TestTheAcceptanceJudgementsMeetTheRequestFences(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedAccepting(t, pool)
	engine := testEngine(config.DesignGateRootIssues, nil)
	signOff := rootsSignOff(t, pool, engine, "LEGION-208")
	judged := func(fact intake.Fact) *intake.Refusal {
		t.Helper()
		var refusal *intake.Refusal
		if err := pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
			var err error
			switch fact := fact.(type) {
			case intake.SignOff:
				refusal, _, err = engine.SignOffRefusal(context.Background(), tx, fact)
			case intake.CloseRoot:
				refusal, err = engine.CloseRootRefusal(context.Background(), tx, fact)
			}
			return err
		}); err != nil {
			t.Fatalf("judge %T read-only: %v", fact, err)
		}
		return refusal
	}
	stranger := claim.Token(treeRoleToken(t, "LEGION-300", claim.RoleArchitect))
	for _, tc := range []struct {
		name string
		fact intake.Fact
		code string
	}{
		{name: "a sign-off by an architect of another tree", fact: intake.SignOff{Issue: "LEGION-208", By: stranger, Generation: signOff.Generation, Entry: signOff.Entry}, code: "ISSUE_OUTSIDE_TREE"},
		{name: "a sign-off on a generation the issue left", fact: intake.SignOff{Issue: "LEGION-208", By: signOff.By, Generation: signOff.Generation + 1, Entry: signOff.Entry}, code: "SIGNOFF_STALE_GENERATION"},
		{name: "a sign-off on a run that no longer stands", fact: intake.SignOff{Issue: "LEGION-208", By: signOff.By, Generation: signOff.Generation,
			Entry: record.StepEntry{Place: signOff.Entry.Place, Starts: signOff.Entry.Starts + 1}}, code: "SIGNOFF_STALE_ENTRY"},
		{name: "a close by an architect of another tree", fact: intake.CloseRoot{Issue: "LEGION-208", Reason: "moot", By: stranger, Generation: signOff.Generation}, code: "ISSUE_OUTSIDE_TREE"},
		{name: "a close on a generation the root left", fact: intake.CloseRoot{Issue: "LEGION-208", Reason: "moot", By: signOff.By, Generation: signOff.Generation + 1}, code: "CLOSE_STALE_GENERATION"},
	} {
		if refusal := judged(tc.fact); refusal == nil || refusal.Code != tc.code {
			t.Errorf("%s: the judgement = %+v, want %s", tc.name, refusal, tc.code)
		}
	}
	if refusal := judged(signOff); refusal != nil {
		t.Fatalf("the control, the sign-off as its route builds it = %+v, want it taken", refusal)
	}
}

// A plan can put the owner's acceptance before another step (`implementer acceptance:owner
// reviewer`, `implementer acceptance:owner tester`), where the sign-off moves the issue to that
// step and the daemon records no acceptance. The sign-off judgement says so (accepts false), so
// the route writes no retro in Dispatch ahead of it; the sign-off whose acceptance is its task's
// last step accepts, the control.
func TestASignOffBeforeAnotherStepAcceptsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		steps   []string
		accepts bool
		next    phase.Phase
	}{
		{steps: []string{"implementer", "acceptance:owner", "reviewer"}, next: phase.Reviewing},
		{steps: []string{"implementer", "acceptance:owner", "tester"}, next: phase.Testing},
		{steps: []string{"implementer", "reviewer", "acceptance:owner"}, accepts: true, next: phase.Retro},
	} {
		t.Run(strings.Join(tc.steps, " "), func(t *testing.T) {
			t.Parallel()
			pool := plannedConflictIssue(t, phase.Accepting, settledGreen("lint"), tc.steps...)
			engine := readsAfter(2 * time.Minute)
			fact := rootsSignOff(t, pool, engine, "LEGION-208")
			var refusal *intake.Refusal
			var accepts bool
			if err := pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
				var err error
				refusal, accepts, err = engine.SignOffRefusal(context.Background(), tx, fact)
				return err
			}); err != nil {
				t.Fatalf("judge the sign-off read-only: %v", err)
			}
			if refusal != nil || accepts != tc.accepts {
				t.Fatalf("the sign-off judgement = %+v, accepts %v; want none, accepts %v", refusal, accepts, tc.accepts)
			}
			// A session wrote retro while the issue was in accepting, and the daemon queued its
			// set-back. A sign-off that accepts the issue drops it, since the acceptance now stands
			// for that retro; one that moves the issue to a later step keeps it, so Dispatch does not
			// stay at a retro the daemon does not record.
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				row, err := record.NewOutboxRow("LEGION-208", record.ReassertStatusWrite("needs_review", "retro", 5), time.Now().UTC())
				if err != nil {
					return err
				}
				return record.NewStore().Enqueue(context.Background(), tx, row)
			}); err != nil {
				t.Fatalf("queue a session's set-back of retro: %v", err)
			}
			applyRefusingNothing(t, pool, engine, fact)
			if got := issuePhase(t, pool); got != tc.next {
				t.Fatalf("after the sign-off the issue is in %s, want %s", got, tc.next)
			}
			var setBacks int
			if err := pool.QueryRow(context.Background(), "select count(*) from outbox where issue = 'LEGION-208' and payload->>'kind' = 'reassert' and payload->>'observedStatus' = 'retro'").Scan(&setBacks); err != nil {
				t.Fatalf("count the set-backs of retro: %v", err)
			}
			if want := map[bool]int{true: 0, false: 1}[tc.accepts]; setBacks != want {
				t.Fatalf("set-backs of retro after the sign-off = %d, want %d: a sign-off that accepts drops them, one that moves to a later step keeps them", setBacks, want)
			}
		})
	}
}

// leave moves key to at with the Dispatch status it leaves on, as a close or a person's park
// leaves it.
func leave(t *testing.T, pool *pgxpool.Pool, key string, at phase.Phase, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "update issues set phase = $2, status = $3 where key = $1", key, string(at), status); err != nil {
		t.Fatalf("take %s out of the workflow: %v", key, err)
	}
}

// A6, the recorded departure. A child the daemon itself holds at admitted beneath an ancestor it
// has already accepted does not refuse that ancestor's sign-off. Without the exclusion the rules
// deadlock: the child is pinned at admitted while the ancestor is accepted, and the ancestor cannot
// be accepted or closed while the child is in the workflow. Decided by this tree's architect on
// dispatch://INFRA-980/comment/d642164a-31d2-4d6c-ae5e-4da5dd0b9104.
func TestAChildHeldUnderAnAcceptedAncestorDoesNotRefuseThatAncestorsSignOff(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	// The root's own pull request has merged, and the root stands in merging, waiting for the
	// rest of its tree. LEGION-209 is in accepting beneath it, and LEGION-300, a person's todo
	// under LEGION-209, stands at admitted held by the root above it.
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "needs_review", Rank: "U"})
	seedBeneath(t, pool, "LEGION-209", "LEGION-208", "LEGION-208", phase.Accepting, "needs_review")
	seedBeneath(t, pool, "LEGION-300", "LEGION-208", "LEGION-209", phase.Admitted, "todo")
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43,
		Branch: "legion/LEGION-209", HeadSHA: "head", CheckedHead: "head", Failing: []string{}})

	if result := signOffNow(t, pool, "signoff-209", "LEGION-209"); result.Refusal != nil {
		t.Fatalf("sign-off of %s = %+v, want it accepted: the only issue beneath it is one the daemon holds at admitted under an accepted ancestor", "LEGION-209", result.Refusal)
	}
	var at phase.Phase
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-209'").Scan(&at); err != nil || at != phase.Retro {
		t.Fatalf("LEGION-209 = %s, %v; want retro", at, err)
	}
	// The hold stands, and is now the nearer ancestor's: the sign-off put LEGION-209 in retro,
	// so LEGION-300 waits for it as well as for the merged root above it. It is still in the
	// workflow for every other reading.
	if got := record.AcceptedAncestor(*issueRecord(t, pool, "LEGION-300"), treeByKey(t, pool, "LEGION-208")); got != "LEGION-209" {
		t.Fatalf("the accepted ancestor of LEGION-300 = %q, want the issue just accepted above it, LEGION-209", got)
	}
	if !record.InWorkflow(*issueRecord(t, pool, "LEGION-300")) {
		t.Fatalf("LEGION-300 left the workflow, want it held at admitted inside it")
	}
}

// treeByKey is every recorded member of tree, by key.
func treeByKey(t *testing.T, pool *pgxpool.Pool, tree string) map[string]record.Issue {
	t.Helper()
	byKey := map[string]record.Issue{}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		members, err := record.NewStore().TreeIssues(context.Background(), tx, tree)
		if err != nil {
			return err
		}
		for _, member := range members {
			byKey[member.Key] = member
		}
		return nil
	})
	return byKey
}

// A6. close_root is refused the same way and with the same code: the close lingers the whole
// tree, which would stop every issue beneath mid-turn.
func TestCloseRootIsRefusedWhileAnIssueBeneathIsInTheWorkflow(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U"})
	seedBeneath(t, pool, "LEGION-209", "LEGION-208", "LEGION-208", phase.Planning, "in_progress")

	closeNow := func(id string) intake.Result {
		t.Helper()
		result, err := intake.ApplyFact(context.Background(), pool, "api", id, intake.CloseRoot{Issue: "LEGION-208", Reason: "moot"},
			testEngine(config.DesignGateRootIssues, nil), admissionStub{})
		if err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
		return result
	}
	result := closeNow("close-held")
	if result.Refusal == nil || result.Refusal.Code != "SIGNOFF_CHILD_IN_WORKFLOW" || !strings.Contains(result.Refusal.Message, "LEGION-209") {
		t.Fatalf("close_root with an issue beneath = %+v, want 409 SIGNOFF_CHILD_IN_WORKFLOW naming LEGION-209", result.Refusal)
	}
	assertPhase(t, pool, phase.Admitted)

	leave(t, pool, "LEGION-209", phase.Admitted, "backlog")
	if result := closeNow("close-accepted"); result.Refusal != nil {
		t.Fatalf("close_root once the issue beneath left = %+v, want it accepted", result.Refusal)
	}
	assertPhase(t, pool, phase.Done)
}

// A6. A person's move of an issue to todo beneath an ancestor the daemon has already accepted is
// recorded and held at admitted, and the architect is told why. The chain is walked past a
// parent already done, so a grandchild under a done parent is held by whichever issue above it
// is still merging.
func TestAPersonsTodoUnderAnAcceptedAncestorIsHeldAtAdmitted(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "needs_review", Rank: "U"})
	seedTreeLifecycle(t, pool, "LEGION-208")
	seedBeneath(t, pool, "LEGION-209", "LEGION-208", "LEGION-208", phase.Done, "done")

	applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil),
		intake.DispatchIssue{Key: "LEGION-300", ResolvedRepo: "sjawhar/legion", Seq: 3, Type: "issue.updated",
			Status: "todo", Title: "grandchild", Parent: "LEGION-209", Rank: "Z"})

	held := *issueRecord(t, pool, "LEGION-300")
	if held.Tree != "LEGION-208" || held.Phase != phase.Admitted {
		t.Fatalf("the person's todo = %#v, want it recorded in LEGION-208's tree at admitted", held)
	}
	if got := record.AcceptedAncestor(held, treeByKey(t, pool, "LEGION-208")); got != "LEGION-208" {
		t.Fatalf("the accepted ancestor of the grandchild = %q, want LEGION-208, found by walking past the done parent", got)
	}
	var reason string
	if err := pool.QueryRow(context.Background(),
		"select payload->>'reason' from outbox where kind = 'notice' and issue = 'LEGION-300' and payload->>'kind' = 'child-status'").Scan(&reason); err != nil {
		t.Fatalf("read the child-status notice: %v", err)
	}
	for _, want := range []string{"LEGION-300", "LEGION-208", "admitted"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("the child-status notice %q does not say %q", reason, want)
		}
	}
}

// The notice of a person's todo beneath an accepted ancestor names the head the daemon accepted
// as the head the last step of the ancestor's task passed, which is true of each plan: a plan with
// no reviewer step has no reviewed head, and an issue stands in merging once its task's last step
// passed whatever that step is (record.Accepted). The ancestor walks today's line in one case and
// the plan `implementer tester`, which has no reviewer step, in the other; the text is one.
func TestTheNoticeOfATodoUnderAnAcceptedAncestorNamesTheHeadItsLastStepPassed(t *testing.T) {
	t.Parallel()
	const want = "LEGION-300 is beneath LEGION-208, whose head the last step of its task passed and the daemon has already accepted, " +
		"so LEGION-300 is recorded at admitted and nothing starts it while LEGION-208 stands in retro or merging."
	for _, tc := range []struct {
		name  string
		steps []string
	}{
		{"an ancestor with no plan rows, which walks today's steps", nil},
		{"an ancestor whose plan has no reviewer step", []string{"implementer", "tester"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
			seedTreeLifecycle(t, pool, "LEGION-208")
			if tc.steps != nil {
				seedStepsPlan(t, pool, "LEGION-208", 1, tc.steps...)
				seedRecord(t, pool, func(tx pgx.Tx) error {
					return record.NewStore().MoveTask(t.Context(), tx, "LEGION-208", 1, "a", record.TaskPlace{Kind: record.PlaceMerge}, record.TaskRunning)
				})
			}

			applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil),
				intake.DispatchIssue{Key: "LEGION-300", ResolvedRepo: "sjawhar/legion", Seq: 3, Type: "issue.updated",
					Status: "todo", Title: "child", Parent: "LEGION-208", Rank: "Z"})

			var reason string
			if err := pool.QueryRow(context.Background(),
				"select payload->>'reason' from outbox where kind = 'notice' and issue = 'LEGION-300' and payload->>'kind' = 'child-status'").Scan(&reason); err != nil {
				t.Fatalf("read the child-status notice: %v", err)
			}
			if reason != want {
				t.Fatalf("the child-status notice = %q, want %q", reason, want)
			}
		})
	}
}

// The texts the daemon sends an architect about children still in the workflow tell it how a child
// leaves: by the architect's own status write in Dispatch (record.EndChildRule), and never by an
// operation the daemon no longer serves or by the old word for the move (dispatch://INFRA-3178 R6).
// The sign_off and close_root refusals, the pr-merged notice and the retro-held notice of a parent
// held before its retro for the issues beneath it also say that a status the architect wrote counts
// only once the daemon has taken it, because the Dispatch event that carries it can reach the
// daemon after the architect calls again. The children-first notice ends a child, released or not,
// by the same write. The why the daemon posts beside a person's todo on a held parent is for that
// person, and carries none of this (heldParentWait's reason, held in restart_task_test.go). Each
// row holds only that much of the wording.
func TestTheRefusalsOverChildrenInTheWorkflowNameTheStatusWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		lag  bool
		text func(t *testing.T) string
	}{
		{"sign_off", true, func(t *testing.T) string {
			pool := migratedPool(t)
			seedAccepting(t, pool)
			seedBeneath(t, pool, "LEGION-209", "LEGION-208", "LEGION-208", phase.Implementing, "in_progress")
			result := signOffNow(t, pool, "signoff-held", "LEGION-208")
			if result.Refusal == nil || result.Refusal.Code != "SIGNOFF_CHILD_IN_WORKFLOW" {
				t.Fatalf("sign-off with an issue beneath = %+v, want SIGNOFF_CHILD_IN_WORKFLOW", result.Refusal)
			}
			return result.Refusal.Message
		}},
		{"close_root", true, func(t *testing.T) string {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U"})
			seedBeneath(t, pool, "LEGION-209", "LEGION-208", "LEGION-208", phase.Planning, "in_progress")
			result, err := intake.ApplyFact(context.Background(), pool, "api", "close-held", intake.CloseRoot{Issue: "LEGION-208", Reason: "moot"},
				testEngine(config.DesignGateRootIssues, nil), admissionStub{})
			if err != nil || result.Refusal == nil || result.Refusal.Code != "SIGNOFF_CHILD_IN_WORKFLOW" {
				t.Fatalf("close_root with an issue beneath = %+v, %v; want SIGNOFF_CHILD_IN_WORKFLOW", result.Refusal, err)
			}
			return result.Refusal.Message
		}},
		{"pr-merged", true, func(t *testing.T) string {
			rig := seedMergedRoot(t, record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"})
			for _, notice := range architectNotices(t, rig.pool) {
				if notice.Kind == "pr-merged" {
					return notice.Reason
				}
			}
			t.Fatalf("no pr-merged notice after a merge that waits for LEGION-209")
			return ""
		}},
		{"children-first", false, func(*testing.T) string {
			return ChildrenFirstNotice(record.ChildWait{Parent: "LEGION-208", Open: []record.OpenChild{{Key: "LEGION-209", Status: "triage"}}}).Reason
		}},
		{"retro-held", true, func(*testing.T) string {
			return retroHeldNotice("LEGION-208", acceptanceHold{code: signOffChildInWorkflow, names: "LEGION-209"}, false).Reason
		}},
		{"retro-held over an open Dispatch child", true, func(*testing.T) string {
			return retroHeldNotice("LEGION-208", acceptanceHold{code: signOffChildOpen, names: "LEGION-209 (backlog)"}, true).Reason
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.text(t)
			if !strings.Contains(text, "dispatch issue-update") || !strings.Contains(text, "--status done") {
				t.Errorf("the %s text %q does not name the architect's status write (dispatch issue-update … --status done)", tc.name, text)
			}
			if strings.Contains(strings.ToLower(text), "park") {
				t.Errorf("the %s text %q names a removed operation or the old word for the move", tc.name, text)
			}
			if tc.lag && (!strings.Contains(text, "has not taken yet") || !strings.Contains(text, "read_record")) {
				t.Errorf("the %s text %q does not say that a status the daemon has not taken yet counts only once read_record shows it", tc.name, text)
			}
		})
	}
}
