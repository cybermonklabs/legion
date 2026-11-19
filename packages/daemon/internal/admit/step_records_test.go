package admit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// stepWrites is every step record row issue's outbox holds, in the order they were written.
func stepWrites(t *testing.T, pool *pgxpool.Pool, issue string) []record.StepRecord {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload from outbox where kind = $1 and issue = $2 order by id`,
		string(record.OutboxKindStepRecord), issue)
	if err != nil {
		t.Fatalf("read the step records of %s: %v", issue, err)
	}
	defer rows.Close()
	var got []record.StepRecord
	for rows.Next() {
		var write record.StepRecord
		if err := rows.Scan(&write); err != nil {
			t.Fatalf("decode a step record of %s: %v", issue, err)
		}
		got = append(got, write)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the step records of %s: %v", issue, err)
	}
	return got
}

// stepRun is one write of a step record as these tests name it: a start (end "") or an end with
// its result, of kind and step of the task default, in generation, under the count of starts its
// run's id is computed from (workflow.StepID).
type stepRun struct {
	end        string
	kind       string
	step       int
	generation uint64
	starts     int64
}

func start(kind string, step int, generation uint64, starts int64) stepRun {
	return stepRun{kind: kind, step: step, generation: generation, starts: starts}
}

func end(result, kind string, step int, generation uint64, starts int64) stepRun {
	return stepRun{end: result, kind: kind, step: step, generation: generation, starts: starts}
}

// assertStepWrites fails unless key's step record rows are exactly want, in order.
func assertStepWrites(t *testing.T, pool *pgxpool.Pool, key string, want ...stepRun) {
	t.Helper()
	got := stepWrites(t, pool, key)
	var lines []string
	for _, w := range got {
		result := ""
		if w.Result != nil {
			result = *w.Result
		}
		lines = append(lines, fmt.Sprintf("%s %s/%d generation %d ended %t result %q", w.ID, w.Kind, w.Step, w.Generation, w.EndedAt != nil, result))
	}
	fail := func(why string) {
		t.Helper()
		t.Fatalf("%s: the step records of %s are\n\t%s\nwant %+v", why, key, strings.Join(lines, "\n\t"), want)
	}
	if len(got) != len(want) {
		fail(fmt.Sprintf("%d rows, want %d", len(got), len(want)))
	}
	for i, w := range want {
		g := got[i]
		result := ""
		if g.Result != nil {
			result = *g.Result
		}
		if g.ID != workflow.StepID(key, w.generation, record.StepPlace{Task: "default", Kind: w.kind, Place: w.step}, w.starts) || g.Kind != w.kind || g.Step != w.step ||
			g.Generation != w.generation || (g.EndedAt != nil) != (w.end != "") || result != w.end {
			fail(fmt.Sprintf("row %d is not %+v", i, w))
		}
	}
}

// countStart writes the start of the step key stands in, as the transition or the promotion that
// started it would have (workflow.Engine.StartStepRecord), in a fact of its own: its count raised
// and its start queued.
func countStart(t *testing.T, pool *pgxpool.Pool, engine intake.Handler, key string) {
	t.Helper()
	stored := issue(t, pool, key)
	workflowEngine := engine.(*workflow.Engine)
	ctx := workflowEngine.OpenFact(context.Background())
	inTx(t, pool, func(tx pgx.Tx) {
		if err := workflowEngine.StartStepRecord(ctx, tx, stored); err != nil {
			t.Fatalf("start the step of %s: %v", key, err)
		}
		if err := workflowEngine.SettleFact(ctx, tx); err != nil {
			t.Fatalf("settle the start of %s: %v", key, err)
		}
	})
}

// closeFinishingTree parks and closes the finishing tree's root (seedFinishingTree), a person's
// close, which lingers its tree.
func closeFinishingTree(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler) {
	t.Helper()
	apply(t, pool, admission, "root-parked", intake.DispatchIssue{Key: finishedRoot, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "backlog", Title: finishedRoot, Rank: "A", HandedOver: true}, engine)
	apply(t, pool, admission, "root-closed", intake.DispatchIssue{Key: finishedRoot, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.closed", Status: "done", Title: finishedRoot, Rank: "A", HandedOver: true}, engine)
}

// reopenFinishingTree sets the closed root todo again, a person's re-admission of its tree.
func reopenFinishingTree(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler) {
	t.Helper()
	apply(t, pool, admission, "root-reopened", intake.DispatchIssue{Key: finishedRoot, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated", Status: "todo", Title: finishedRoot, Rank: "A", HandedOver: true}, engine)
}

// handOverAtTheLingersEnd ends the closed tree's linger and applies the hand-over event of key,
// which admits it as a root of its own carrying its run.
func handOverAtTheLingersEnd(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler, key string) {
	t.Helper()
	expireLinger(t, pool, admission, engine)
	apply(t, pool, admission, key+"-handed-over", intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated", Status: "todo", Title: key, Parent: finishedRoot, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
	if got := issue(t, pool, key); got.Tree != key || got.Generation != 2 {
		t.Fatalf("the handed-over %s = %#v, want a root of its own at generation 2", key, got)
	}
}

// seedAwaitingPullRequest gives the finishing tree's member in merging its open pull request #5.
func seedAwaitingPullRequest(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{Issue: awaiting, Repo: "acme/widgets", Number: 5,
			Branch: "legion/LEGION-5", HeadSHA: "head-5", Generation: 1, State: record.PullRequestOpen}); err != nil {
			t.Fatalf("seed the pull request of %s: %v", awaiting, err)
		}
	})
}

// conflictReadOfAwaiting gives the member in merging its open pull request #5 onto main with a
// first read that found head-5 conflicting two minutes before fixedNow, the state a second read of
// that head applies on (dispatch://INFRA-4374 R2). Its arguments are a row's arrange's.
func conflictReadOfAwaiting(t *testing.T, pool *pgxpool.Pool, _ *Admission, _ intake.Handler) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{Issue: awaiting, Repo: "acme/widgets", Number: 5,
			Branch: "legion/LEGION-5", Base: "main", HeadSHA: "head-5", Generation: 1, State: record.PullRequestOpen, Failing: []string{},
			ConflictRead: &record.ConflictRead{Head: "head-5", Base: "main", At: fixedNow.Add(-2 * time.Minute)}}); err != nil {
			t.Fatalf("seed the pull request of %s with its first conflicting read: %v", awaiting, err)
		}
	})
}

// A member's linger ends its step (the settle of the fact that lingers its tree), and the hand-over
// at the close of that linger re-admits it, which raises its generation: the settle of that fact
// finds the run ended already, by the mark the store keeps, and writes no second end, and the
// promotion of the new root starts its new generation's step.
func TestAHandedOverMemberWhoseLingerEndedItsStepGetsNoSecondEnd(t *testing.T) {
	pool, admission, engine := seedFinishingTree(t)
	countStart(t, pool, engine, heldTesting)
	closeFinishingTree(t, pool, admission, engine)
	assertStepWrites(t, pool, heldTesting, start("step", 3, 1, 1), end("stopped", "step", 3, 1, 1))
	handOverAtTheLingersEnd(t, pool, admission, engine, heldTesting)
	assertStepWrites(t, pool, heldTesting, start("step", 3, 1, 1), end("stopped", "step", 3, 1, 1), start("step", 3, 2, 2))
}

// A run's re-admission ends that run when nothing ended it before: a child left mid-run in testing
// by a root that closed before hand-overs existed, so no linger of this release ended its step,
// handed to Legion by a person. Its readmit ends the old generation's run stopped, before the
// promotion starts the tester of the new generation.
func TestAHandedOverIssueEndsItsOldRun(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	root := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: "finished root", Tree: "LEGION-208", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", LastDispatchSeq: 1}
	parent := root.Key
	child := record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: "testing child", Tree: root.Key, Parent: &parent,
		Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "B", LastDispatchSeq: 1}
	putIssue(t, pool, root)
	putIssue(t, pool, child)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{Issue: child.Key, Repo: "acme/widgets", Number: 42,
			Branch: "legion/LEGION-209", HeadSHA: "head-42", State: record.PullRequestOpen}); err != nil {
			t.Fatalf("seed open pull request: %v", err)
		}
		if _, err := treelifecycle.Open(ctx, tx, "legion", root.Key, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open old tree lifecycle: %v", err)
		}
		lifecycle, err := treelifecycle.ReserveCleanup(ctx, tx, "legion", root.Key, treelifecycle.AuthorityWorkflow)
		if err != nil {
			t.Fatalf("reserve old tree cleanup: %v", err)
		}
		if err := treelifecycle.ConfirmCleanup(ctx, tx, "legion", root.Key, lifecycle.Epoch); err != nil {
			t.Fatalf("confirm old tree cleanup: %v", err)
		}
	})
	countStart(t, pool, engine, child.Key)

	apply(t, pool, admission, "person-hands-over", intake.DispatchIssue{Key: child.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: child.Title, Parent: root.Key, Rank: child.Rank, HandedOver: handed}, engine)

	if got := issue(t, pool, child.Key); got.Tree != child.Key || got.Generation != 2 || got.Phase != phase.Testing {
		t.Fatalf("handed-over child = %#v, want its own tree at generation 2 in testing", got)
	}
	assertStepWrites(t, pool, child.Key, start("step", 3, 1, 1), end("stopped", "step", 3, 1, 1), start("step", 3, 2, 2))
}

// A member awaiting its merge keeps its merge step open through the linger (the workflow's
// beginLinger), and the linger closes with no merge: the member is handed over, and its readmit
// ends that merge step stopped, once, before the promotion starts the new generation's merger.
func TestALingeringMemberThatNeverMergesEndsItsMergeStepOnce(t *testing.T) {
	pool, admission, engine := seedFinishingTree(t)
	countStart(t, pool, engine, awaiting)
	closeFinishingTree(t, pool, admission, engine)
	assertStepWrites(t, pool, awaiting, start("merge", 0, 1, 1))
	handOverAtTheLingersEnd(t, pool, admission, engine, awaiting)
	assertStepWrites(t, pool, awaiting, start("merge", 0, 1, 1), end("stopped", "merge", 0, 1, 1), start("merge", 0, 2, 2))
}

// The linger leaves a member's merge step open, and only one of three writes ends it before any
// new start of it: the promotion of the tree admitted again, which starts the member's merger
// again; the merge, landing between the root's readmit and that promotion; or the hand-over at the
// linger's close, where readmit ends it and the promotion of the new root ends nothing more.
func TestAReadmittedTreeEndsTheOpenMergeStepBeforeItStartsTheMemberAgain(t *testing.T) {
	t.Run("the promotion", func(t *testing.T) {
		pool, admission, engine := seedFinishingTree(t)
		seedAwaitingPullRequest(t, pool)
		countStart(t, pool, engine, awaiting)
		closeFinishingTree(t, pool, admission, engine)
		assertStepWrites(t, pool, awaiting, start("merge", 0, 1, 1))
		reopenFinishingTree(t, pool, admission, engine)
		assertStepWrites(t, pool, awaiting, start("merge", 0, 1, 1), end("stopped", "merge", 0, 1, 1), start("merge", 0, 1, 2))
	})
	t.Run("the merge before the promotion", func(t *testing.T) {
		pool, admission, engine := seedFinishingTree(t)
		seedAwaitingPullRequest(t, pool)
		countStart(t, pool, engine, awaiting)
		closeFinishingTree(t, pool, admission, engine)
		// Every slot is taken, so the reopened root waits for one, and its promotion with it.
		for _, key := range []string{"LEGION-50", "LEGION-51", "LEGION-52"} {
			seedSlotted(t, pool, key, "Z")
		}
		reopenFinishingTree(t, pool, admission, engine)
		if architectStarts(t, pool, finishedRoot) != 0 {
			t.Fatalf("the reopened root was promoted with every slot taken")
		}
		if result, err := intake.ApplyFact(context.Background(), pool, "github", "merged-5", intake.PullRequestMerged{Repo: "acme/widgets", Number: 5, MergeSHA: "merge-5"}, engine, admission); err != nil || result.Refusal != nil {
			t.Fatalf("merge %s = %+v, %v", awaiting, result.Refusal, err)
		}
		assertStepWrites(t, pool, awaiting, start("merge", 0, 1, 1), end("passed", "merge", 0, 1, 1))
		apply(t, pool, admission, "slot-freed", intake.DispatchIssue{Key: "LEGION-50", ResolvedRepo: "acme/widgets", Seq: 5, Type: "issue.updated", Status: "backlog", Title: "LEGION-50", Rank: "Z", HandedOver: true}, engine)
		if architectStarts(t, pool, finishedRoot) != 1 {
			t.Fatalf("the reopened root was not promoted once a slot was free")
		}
		assertStepWrites(t, pool, awaiting, start("merge", 0, 1, 1), end("passed", "merge", 0, 1, 1))
	})
	t.Run("a hand-over", func(t *testing.T) {
		pool, admission, engine := seedFinishingTree(t)
		seedAwaitingPullRequest(t, pool)
		countStart(t, pool, engine, awaiting)
		closeFinishingTree(t, pool, admission, engine)
		handOverAtTheLingersEnd(t, pool, admission, engine, awaiting)
		assertStepWrites(t, pool, awaiting, start("merge", 0, 1, 1), end("stopped", "merge", 0, 1, 1), start("merge", 0, 2, 2))
	})
	// The member is held from merging when the linger closes: the hand-over raises its generation
	// and the promotion starts no worker of a held member, so the promotion's own start
	// (workflow.Engine.StartStepRecord) opens the new generation's merge, after it ends the run of
	// the generation before, which the linger left open. Its merge then ends that new run passed.
	t.Run("a hand-over of a member held from merging", func(t *testing.T) {
		pool, admission, engine := seedFinishingTree(t)
		seedAwaitingPullRequest(t, pool)
		held := issue(t, pool, awaiting)
		held.Phase, held.Hold = phase.Held, &record.Hold{From: phase.Merging, Reason: record.HoldEscalated}
		putIssue(t, pool, held)
		countStart(t, pool, engine, awaiting)
		closeFinishingTree(t, pool, admission, engine)
		handOverAtTheLingersEnd(t, pool, admission, engine, awaiting)
		if got := issue(t, pool, awaiting); got.Generation != 2 {
			t.Fatalf("the handed-over member stands at generation %d, want 2", got.Generation)
		}
		if result, err := intake.ApplyFact(context.Background(), pool, "github", "merged-5", intake.PullRequestMerged{Repo: "acme/widgets", Number: 5, MergeSHA: "merge-5"}, engine, admission); err != nil || result.Refusal != nil {
			t.Fatalf("merge %s = %+v, %v", awaiting, result.Refusal, err)
		}
		assertStepWrites(t, pool, awaiting, start("merge", 0, 1, 1), end("stopped", "merge", 0, 1, 1), start("merge", 0, 2, 2), end("passed", "merge", 0, 2, 2))
	})
}

// putOwnArchitect records the claim of the accepting member's own architect in state, suspended by
// by, as the claim store holds it: idle while it holds its role, suspended once the outbox has run
// its tree's close or an operator suspended it.
func putOwnArchitect(t *testing.T, pool *pgxpool.Pool, state supervise.ClaimState, by supervise.SuspendSource) {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
		values ($1, $2, $3, $4, 'architect', 1, 'ses_own', '/sessions/ses_own.jsonl', $5, 0, 0, 0, 0, $6)
		on conflict (token) do update set state = excluded.state, suspended_by = excluded.suspended_by`,
		string(architectToken(t, inAccepting)), project, finishedRoot, inAccepting, string(state), string(by)); err != nil {
		t.Fatalf("record the architect claim of %s in state %s: %v", inAccepting, state, err)
	}
}

// A tree admitted again starts each member's step again: the linger ended each member's run
// stopped, and the promotion starts a new run of the same place, another record. A member whose
// phase worker the promotion starts gets it from that start (workflow.Engine.StartPromotedWorker);
// a member in accepting, whose role is an architect, and one held from testing, whose hold starts
// no worker, get it from the promotion's own start (workflow.Engine.StartStepRecord).
//
// The member in accepting is the work of the architect that owns it: its own, which held its role
// when the run began and which the tree's close then suspended. The promotion queues a start of it
// in the transaction that starts the new run, and once that start runs the member's own architect
// owns it again, so each write of its record names that architect. The control is the operator's
// suspension, which no start wakes before the operator's resume: the new run names the tree's
// architect, which owns the member until then.
func TestATreeAdmittedAgainStartsEachMemberAgain(t *testing.T) {
	tree, own := architectToken(t, finishedRoot), architectToken(t, inAccepting)
	for _, tc := range []struct {
		name string
		by   supervise.SuspendSource
		// again is the agent of the accepting member's new run.
		again claim.Token
	}{
		{name: "the tree's close suspended the member's own architect", by: supervise.SuspendedByDaemon, again: own},
		{name: "control: the operator suspended the member's own architect", by: supervise.SuspendedByOperator, again: tree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, admission, engine := seedFinishingTree(t)
			putOwnArchitect(t, pool, supervise.StateIdle, "")
			members := []struct {
				key  string
				kind string
				step int
			}{{planning, "step", 1}, {heldTesting, "step", 3}, {inAccepting, "step", 5}, {inRetro, "retro", 0}}
			for _, member := range members {
				countStart(t, pool, engine, member.key)
			}
			closeFinishingTree(t, pool, admission, engine)
			putOwnArchitect(t, pool, supervise.StateSuspended, tc.by)
			reopenFinishingTree(t, pool, admission, engine)
			for _, member := range members {
				assertStepWrites(t, pool, member.key, start(member.kind, member.step, 1, 1), end("stopped", member.kind, member.step, 1, 1), start(member.kind, member.step, 1, 2))
			}
			assertStepWrites(t, pool, unstarted)
			// The promotion's start of the member's own architect names its phase and carries no
			// task, a row effects leaves out, so it is counted here.
			var queued int
			if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = $1 and issue = $2
				and payload->>'op' = 'start' and payload->>'role' = 'architect' and payload->>'phase' = $3`,
				string(record.OutboxKindSupervise), inAccepting, string(phase.Accepting)).Scan(&queued); err != nil || queued != 1 {
				t.Fatalf("the promotion queued %d starts of %s's own architect in accepting (%v), want one", queued, inAccepting, err)
			}
			writes := stepWrites(t, pool, inAccepting)
			for i, want := range []claim.Token{own, own, tc.again} {
				if writes[i].Agent != string(want) {
					t.Fatalf("write %d of %s's acceptance names agent %q, want %s (the tree's is %s, its own %s)", i, inAccepting, writes[i].Agent, want, tree, own)
				}
			}
		})
	}
}
