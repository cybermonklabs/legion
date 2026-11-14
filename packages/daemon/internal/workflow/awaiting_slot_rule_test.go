package workflow

import (
	"context"
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

// The spec's rule, restored: a root enters awaiting_person only while it holds an admission
// row, so only a root whose design gate has never opened, whose architect is writing the spec
// and holds the whole of the tree's work (dispatch://INFRA-980, "Which issue needs a slot",
// line 65).
//
// A root whose gate has opened waits for its children instead, and it keeps its row through
// that wait as through every other (record.KeepsSlot), so the row alone cannot tell the two
// apart: rule 0 reads the row to say the daemon has started this root at all, and AtWork is
// what says its architect still has work of its own to hand over. Sending such a root into
// awaiting_person takes every first-phase member of its tree out of the waiting line
// (record.Waiting drops the members of a root waiting on a person), so the children it is
// waiting for can never start; the controller is told of a wait the root has no work for; and
// the next pass answers the wait as an approval, because the gate is open, which clears what
// the architect handed to the person.
func TestARootWhoseGateHasOpenedEntersNoWaitOnAPerson(t *testing.T) {
	root := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Phase: phase.Admitted,
		Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", HandedOver: true, DeclaredSeq: 9}
	approved := 1
	opened := record.DesignGate{Issue: root.Key, Step: record.DesignStepSpec, ArtifactID: "spec", LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true}
	rows := map[record.DesignStep]record.DesignGate{record.DesignStepSpec: opened}

	decision := decideAwait(awaitView{Root: root, Slotted: true, Gates: rows})
	if decision.Act != actStay {
		t.Fatalf("the decision for a root whose gate has opened = %+v, want it left where it stands: its architect has no work of its own to hand over, and its children are waiting for their rows", decision)
	}

	// The control: the same root before its gate ever opened, where the architect is writing the
	// spec, is not stopped by this rule — whatever the rest of the rules then decide, rule 0 is
	// not what answers it.
	writing := opened
	writing.ApprovedVersion, writing.EverOpened = nil, false
	if decision := decideAwait(awaitView{Root: root, Slotted: true, Gates: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: writing}}); decision.Why == neverStarted {
		t.Fatalf("a root whose gate has never opened and holds its row is stopped by the row rule: that root is the one the wait is for")
	}
}

// neverStarted is rule 0's answer for a root the daemon has not started, word for word as
// workflow's decideAwait writes it (awaiting_reconcile.go). The row the rule reads says the
// daemon started the issue rather than that a worker of it has work, since an issue keeps its
// row while it waits as well (record.KeepsSlot).
const neverStarted = "the daemon has not started it, so it holds no admission slot"

// A root re-admitted and still waiting in line for its row holds none, so it enters no wait on
// a person however its hand-back stands. Re-admission clears the mark (record.Store.StartHandout),
// but the architect of the generation before is live until the linger's queued suspend runs, and
// a declaration of its makes the mark non-zero again, so rule 1 does not keep such a root out.
// Rule 0 does, and only because it reads the root's own row: the row says the daemon started
// this root, and one that was never promoted holds none. Were it to enter the wait, the next
// pass would promote it as an answered root rather than through its admission — no lifecycle
// opened, no issue branch written — and the pass after that would read an in_progress root of
// no running tree and let it linger (admit's releaseUnslotted). (Round 4, the reviewer's
// medium finding on rule 0.)
func TestAQueuedRootWithAHandBackEntersNoWaitOnAPerson(t *testing.T) {
	view := awaitRootView()
	view.Root.Generation, view.Root.Status, view.Root.DispatchStatus = 2, "todo", "todo"
	view.Slotted = false
	if decision := decideAwait(view); decision.Act != actStay || decision.Why != neverStarted {
		t.Fatalf("the decision for a re-admitted root still waiting in line = %+v, want it left where it stands: the daemon never started it", decision)
	}

	// The control: the same root once admission promoted it and gave it its row enters the wait.
	view.Slotted = true
	if decision := decideAwait(view); decision.Act != actEnter {
		t.Fatalf("the decision for the same root holding its row = %+v, want it to enter the wait", decision)
	}
}

// ReconcileAwaiting reads the admission rows of the record, so the rule above holds of the pass
// and not only of its decision: over one record holding two roots that have each handed a
// question to a person, it moves the one the daemon started, which holds a row, and leaves the
// one still waiting in line, which holds none.
func TestReconcileAwaitingMovesOnlyTheRootHoldingItsSlot(t *testing.T) {
	const slottedKey, queuedKey = "LEGION-208", "LEGION-300"
	pool := migratedPool(t)
	slotted := askRoot()
	slotted.HandedOver = true
	queued := record.Issue{Key: queuedKey, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: queuedKey,
		Project: "LEGION", Title: "a root waiting in line", Phase: phase.Admitted, Generation: 2, Status: "todo", DispatchStatus: "todo",
		Rank: "V", HandedOver: true, LastDispatchSeq: 1}
	handedBack := record.EventMark{Seq: 5, At: time.Date(2026, 9, 22, 23, 0, 0, 0, time.UTC)}
	for _, root := range []record.Issue{slotted, queued} {
		seedIssue(t, pool, root)
		seedArchitectWorking(t, pool, root)
		seedRecord(t, pool, func(tx pgx.Tx) error {
			store := record.NewStore()
			if err := store.PutRootAsk(context.Background(), tx, record.RootAsk{Issue: root.Key, Ask: "ask-" + root.Key,
				Kind: record.AskQuestion, Open: true, Seq: 5, WaitingOn: "human", WaitingOnSeq: 5, ByTree: true}); err != nil {
				return err
			}
			return store.Declare(context.Background(), tx, root.Key, handedBack)
		})
	}
	seedSlot(t, pool, record.Slot{Issue: slottedKey, Index: 0, AdmittedAt: time.Date(2026, 9, 22, 22, 0, 0, 0, time.UTC)})

	entered := reconcileRoots(t, pool, slottedKey, queuedKey)
	if len(entered) != 1 || entered[0] != slottedKey {
		t.Fatalf("the roots that entered the wait = %v, want only the one the daemon started, which holds a row", entered)
	}
	if got := phaseOf(t, pool, queuedKey); got != phase.Admitted {
		t.Fatalf("the root still waiting in line = %s, want it left admitted: the daemon never started it, so it holds no row", got)
	}
	if got := phaseOf(t, pool, slottedKey); got != phase.AwaitingPerson {
		t.Fatalf("the root holding its row = %s, want it waiting on its person", got)
	}
}

// reconcileRoots runs one ReconcileAwaiting pass over keys, read from the record as they stand,
// under the root-issues design gate, and returns the roots that entered the wait.
func reconcileRoots(t *testing.T, pool *pgxpool.Pool, keys ...string) []string {
	t.Helper()
	var entered []string
	seedRecord(t, pool, func(tx pgx.Tx) error {
		ctx, _ := intake.WithCommitHooks(context.Background())
		store := record.NewStore()
		issues := make([]record.Issue, 0, len(keys))
		for _, key := range keys {
			issue, err := store.Issue(ctx, tx, key)
			if err != nil {
				return err
			}
			issues = append(issues, *issue)
		}
		var err error
		entered, _, err = testEngine(config.DesignGateRootIssues, nil).ReconcileAwaiting(ctx, tx, "LEGION", issues, nil)
		return err
	})
	return entered
}

// seedArchitectWorking records root's architect claim working a task it took, so the root's
// architect owes nothing (decideAwait's rule 5) and the wait may begin.
func seedArchitectWorking(t *testing.T, pool *pgxpool.Pool, root record.Issue) {
	t.Helper()
	project, err := claim.ProjectToken(root.Project)
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	token, err := claim.NewToken(project, root.Key, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("architect token: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $2, $3, $4, 'architect', $5, 'session-architect', '', 'working', 0, 0, 0, 0)`,
		string(token), root.Project, root.Tree, root.Key, int64(root.Generation)); err != nil {
		t.Fatalf("seed the architect's claim: %v", err)
	}
}
