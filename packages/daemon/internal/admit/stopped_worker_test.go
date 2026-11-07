package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// An operator's `legion claims stop` of a phase worker holds against admission's repair of a slot
// whose worker has no task (resumeSlottedWorkers): the stop retires the claim, and the repair
// passes over a retired claim, so no pass hands the operator back, a second later, the worker they
// stopped. What the stop costs is read here too: the issue keeps its admission slot with nobody in
// it, which the workflow's own starts — the held phase's retry, the tree's re-admission — are what
// end.
//
// The fixture is one the repair would otherwise act on. The member holds a slot, stands in a worker
// phase of a tree that runs, and owes no queued start, so every term of the repair's rule is met
// but the claim's state. The claim holds the task of that phase and generation confirmed, which a
// claim that is not working no longer works (supervise.Delivery.StillWorked), so what the repair
// reads is not its older skip of a task the claim still has in hand.
//
// The contrast run is what says so. The same fixture with the claim suspended by the daemon rather
// than retired — the one difference between the two runs — gets its start from the first pass. A
// fixture the repair never reached would give that run no start either, and the retired run would
// prove nothing.
func TestARetiredWorkersSlotIsNotRepaired(t *testing.T) {
	const root, member = "LEGION-240", "LEGION-241"
	// stopped records a tree that runs, its member slotted mid-testing, and the member's tester
	// claim in state with suspendedBy, holding that phase's confirmed task. Nothing else differs
	// between the two runs: the claim's session, its task and the slot are the same in both.
	stopped := func(t *testing.T, state supervise.ClaimState, suspendedBy supervise.SuspendSource) *pgxpool.Pool {
		t.Helper()
		pool := migratedPool(t)
		// The root waits in accepting while its member works, so it holds no slot of its own and
		// the member's is the one slot the cap gives out.
		putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: testProject, Title: "root", Tree: root, Phase: phase.Accepting, Generation: 1, Status: "needs_review",
			DispatchStatus: "needs_review", Rank: "A", HandedOver: true, LastDispatchSeq: 1})
		seedLifecycle(t, pool, root)
		parent := root
		putIssue(t, pool, record.Issue{Key: member, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: testProject, Title: "member", Tree: root, Parent: &parent, Phase: phase.Testing, Generation: 1,
			Status: "testing", DispatchStatus: "testing", Rank: "B", LastDispatchSeq: 2})
		takeSlot(t, pool, member)
		project, err := claim.ProjectToken(testProject)
		if err != nil {
			t.Fatalf("project token: %v", err)
		}
		token, err := claim.NewToken(project, member, claim.RoleTester)
		if err != nil {
			t.Fatalf("claim token of the tester of %s: %v", member, err)
		}
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			serving_generation, last_start_row, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
			values ($1, $2, $3, $4, $5, 1, 'ses_tester', '/sessions/ses_tester.jsonl', $6, 1, 0, 0, 0, 0, 0, $7)`,
			string(token), project, root, member, string(claim.RoleTester), string(state), string(suspendedBy)); err != nil {
			t.Fatalf("record the %s tester claim of %s: %v", state, member, err)
		}
		if _, err := pool.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, generation, phase, confirmed_at)
			values ($1, 'outbox:11', 'Carry on testing.', $2, 1, $3, $2)`,
			string(token), fixedNow, string(phase.Testing)); err != nil {
			t.Fatalf("record the confirmed task of the %s tester claim of %s: %v", state, member, err)
		}
		return pool
	}
	// The fact that drives the Dispatch pass: a todo issue of the project nobody handed to Legion,
	// which admission records nothing of and promotes nothing for (Admission.applyFact). So the
	// repair is the only thing the pass can write for the member.
	elsewhere := intake.DispatchIssue{Key: "LEGION-900", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated",
		Status: "todo", Title: "an issue of another team", Rank: "Z"}

	pool := stopped(t, supervise.StateRetired, "")
	admission := quiet(t, 1)
	for _, pass := range []struct {
		name string
		run  func()
	}{
		{name: "the pass a Dispatch fact drives", run: func() { applyFact(t, pool, admission, "dispatch-1", elsewhere) }},
		{name: "the controller tick", run: func() { tick(t, pool, admission, "tick-1") }},
		{name: "the boot's read", run: func() { reconcile(t, pool, admission, nil) }},
	} {
		pass.run()
		if got := startsOf(t, pool, member); len(got) != 0 {
			t.Fatalf("after %s the tester of %s has the start rows %+v, want none: the operator stopped that worker, so its claim is retired and no pass starts its phase again",
				pass.name, member, got)
		}
		if !holdsSlot(t, pool, member) {
			t.Fatalf("after %s the slots are %#v, want %s to keep its own: the stop leaves the slot held with no worker in it until a start of the workflow's own",
				pass.name, slots(t, pool), member)
		}
	}

	// The same fixture, the claim suspended by the daemon instead of stopped. The repair reaches
	// it and starts the phase again, as two rows: the relaunch, which brings the session back
	// carrying no task, and the one row that carries the task, held for the issue's own slot
	// (workflow.StartOrOwe).
	resumable := stopped(t, supervise.StateSuspended, supervise.SuspendedByDaemon)
	applyFact(t, resumable, quiet(t, 1), "dispatch-1", elsewhere)
	rows := startsOf(t, resumable, member)
	var relaunch, tasked []midPhaseStart
	for _, row := range rows {
		if row.task == "" {
			relaunch = append(relaunch, row)
			continue
		}
		tasked = append(tasked, row)
	}
	if len(tasked) != 1 || tasked[0].role != claim.RoleTester || !tasked[0].needsSlot || len(relaunch) != 1 {
		t.Fatalf("the suspended tester of %s has the start rows %+v, want exactly one carrying a task, the tester's, marked for the issue's slot, beside its one relaunch: the repair acts on this fixture, so what the retired run above reads is the claim's state",
			member, rows)
	}
}
