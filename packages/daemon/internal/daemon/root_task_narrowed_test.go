package daemon

import (
	"context"
	"encoding/json"
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

// heldAndReplaced keeps two of the person's answers on key, the first held by the delivery
// outbox:7 of token's claim and the second held by no delivery, since a newer decision on the same
// reference replaced the row that delivery held: a task naming both, which it answers encoded as
// the delivery's Kept, is narrowed of the second at its send (the daemon's rootTask).
func heldAndReplaced(t *testing.T, pool *store.Pool, key string, token claim.Token) (kept, replaced record.DecisionMark, encoded []byte) {
	t.Helper()
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	kept = record.DecisionMark{Issue: key, Ref: "dispatch://" + key + "/ask/ask-0001", Decision: record.DecisionAskAnswered,
		Actor: "user:alice", At: at}
	replaced = record.DecisionMark{Issue: key, Ref: "dispatch://" + key + "/ask/ask-0002", Decision: record.DecisionAskAnswered,
		Actor: "user:alice", At: at.Add(time.Minute)}
	if _, err := pool.Exec(context.Background(), `insert into parked_decisions (issue, ref, decision, actor, at, held_by_claim, held_by_delivery)
		values ($1, $2, $3, $4, $5, $6, 'outbox:7'), ($1, $7, $3, $4, $8, null, null)`,
		key, kept.Ref, string(kept.Decision), kept.Actor, kept.At, string(token), replaced.Ref, at.Add(time.Hour)); err != nil {
		t.Fatalf("keep the decisions of %s, the first held by the architect's task: %v", key, err)
	}
	encoded, err := json.Marshal(record.KeptDecisions{Decisions: []record.DecisionMark{kept, replaced}})
	if err != nil {
		t.Fatalf("encode what the held task names: %v", err)
	}
	return kept, replaced, encoded
}

// A root a hand-over of the release before carried in keeps the take-the-phase-up instruction its
// architect's held task carried when a decision leaves that task. The narrowing reads whether the
// root was carried from the root's own record, as every start of a root architect does
// (workflow.ArchitectStartOf, workflow.ArchitectCarried): the first boot's conversion copied the
// generation of the carried run onto it (record.Issue.CarriedGeneration, the daemon's
// finishHandOver), and the root stands past admitted in that run. A narrowing that missed the carry
// would take the root for one past admitted under its own power, whose rewrite is the decisions'
// text alone.
//
// Whether the held task carried the instruction is the entry its delivery is of, and not its
// text (workflow.NarrowRootTask). The held task here ends in words a later release could have
// written in place of the derivation's closing sentence, and the instruction is kept all the same.
// The control is the same delivery of no entry, which a release before the architect held an
// entry queued, and whose narrowing is the decisions' own text alone.
func TestTheHeldTaskOfARootAHandOverCarriedInKeepsItsPhaseWhenADecisionLeaves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	const key = "LEGION-312"
	steps := config.DesignGateRootIssues.Steps()
	// The root as the conversion's finish of a hand-over that carried its run leaves it, at the
	// generation of that run, which its architect has since taken up in implementing.
	putChainIssue(t, pool, records, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: heldTaskProject, Tree: key, Title: "Carried", Phase: phase.Implementing,
		Generation: 2, Status: "in_progress", Rank: "A", LastDispatchSeq: 1, HandedOver: true, TakenBack: true, HoldsGate: true,
		CarriedGeneration: 2})
	root := readIssue(t, pool, key)
	if carried, _ := workflow.ArchitectCarried(*root); !carried {
		t.Fatalf("the carried root = %+v, want it recorded as carried into its generation: the case's premise", root)
	}

	token := mustClaimToken(t, key, claim.RoleArchitect)
	kept, replaced, encoded := heldAndReplaced(t, pool, key, token)
	decided, _ := workflow.KeptDecisionsTask(workflow.KeptGates{}, steps, key, record.KeptDecisions{Decisions: []record.DecisionMark{kept, replaced}})
	derived := workflow.ArchitectStartTask(workflow.ArchitectStart{Root: *root, Carried: true, Gate: record.TreeGate{Steps: steps},
		Decided: decided})
	if !strings.Contains(derived, workflow.ArchitectCarriedPhaseLabel) {
		t.Fatalf("the derived task = %q, want the carried shape: the case's premise", derived)
	}
	const reworded = " A later release's own closing words."
	held := derived + reworded

	w := &workflowRuntime{pool: pool, records: records, gatedSteps: steps}
	narrow := func(entry int64) string {
		t.Helper()
		task, _, _, err := w.rootTask(ctx, supervise.Claim{Token: token, Project: "legion", Tree: key, Issue: key, Role: claim.RoleArchitect},
			supervise.Delivery{ID: "outbox:7", Task: held, Kept: encoded, Entry: entry})
		if err != nil {
			t.Fatalf("narrow the held task of %s at entry %d: %v", key, entry, err)
		}
		return task
	}
	stays, _ := workflow.KeptDecisionsTask(workflow.KeptGates{}, steps, key, record.KeptDecisions{Decisions: []record.DecisionMark{kept}})
	task := narrow(1)
	want := workflow.ArchitectStartTask(workflow.ArchitectStart{Root: *root, Carried: true, Gate: record.TreeGate{Steps: steps},
		Decided: stays})
	if task != want {
		t.Fatalf("the narrowed task of the carried root = %q, want %q: the take-the-phase-up instruction, naming the one decision the task still holds", task, want)
	}
	if strings.Contains(task, replaced.Ref) || strings.Contains(task, reworded) {
		t.Fatalf("the narrowed task %q still names %s, which a newer decision replaced, or keeps the held text's own words", task, replaced.Ref)
	}
	if task := narrow(0); task != stays {
		t.Fatalf("the narrowed task of a held task of no entry = %q, want the decisions' own text %q", task, stays)
	}
}

// A gate holder whose record stores another issue's tree — an issue first recorded under a parent
// that has since left Legion's work, so it holds its own gate on the Dispatch chain — has its
// architect's held task narrowed over its own key (R15 and R18 of dispatch://INFRA-3320): the
// decisions that stay are written again as the decisions of the gate holder's own tree
// (workflow.KeptDecisionsTask over the claim's issue), never of the tree its record and its claim
// store (decision 13 of dispatch://INFRA-4730 keeps that tree in the start, and so on the claim).
func TestTheHeldTaskOfAGateHolderStoredUnderAnotherTreeNamesItsOwnTreeWhenADecisionLeaves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	const oldRoot, holder = "LEGION-313", "LEGION-314"
	steps := config.DesignGateRootIssues.Steps()
	parent := oldRoot
	putChainIssue(t, pool, records, record.Issue{Key: oldRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: heldTaskProject, Tree: oldRoot, Title: "Old root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "T"})
	putChainIssue(t, pool, records, record.Issue{Key: holder, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: heldTaskProject, Tree: oldRoot, Parent: &parent, Title: "Gate holder", Phase: phase.Admitted, Generation: 1,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		chains, err := record.ReadChains(ctx, tx, records, heldTaskProject, []string{holder})
		if err != nil {
			return err
		}
		if chains.GateRoot(holder) != holder {
			t.Fatalf("%s's gate root is %q, want itself: the case's premise", holder, chains.GateRoot(holder))
		}
		return nil
	}); err != nil {
		t.Fatalf("read the chain of %s: %v", holder, err)
	}

	token := mustClaimToken(t, holder, claim.RoleArchitect)
	kept, replaced, encoded := heldAndReplaced(t, pool, holder, token)
	w := &workflowRuntime{pool: pool, records: records, gatedSteps: steps}
	task, _, _, err := w.rootTask(ctx, supervise.Claim{Token: token, Project: "legion", Tree: oldRoot, Issue: holder, Role: claim.RoleArchitect},
		supervise.Delivery{ID: "outbox:7", Task: "a person decided on the tree: " + kept.Ref + " and " + replaced.Ref, Kept: encoded})
	if err != nil {
		t.Fatalf("narrow the held task of %s: %v", holder, err)
	}
	want, _ := workflow.KeptDecisionsTask(workflow.KeptGates{}, steps, holder, record.KeptDecisions{Decisions: []record.DecisionMark{kept}})
	if task != want || strings.Contains(task, oldRoot) {
		t.Fatalf("the narrowed task of %s = %q, want %q: the one decision it still holds, decided on %s's own tree and not on %s's",
			holder, task, want, holder, oldRoot)
	}
}
