package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// statusWrites is how many Dispatch status writes are queued for issue: the rows that would tell
// Dispatch the issue moved. A release for a wait writes none, because nothing of the issue moved.
func statusWrites(t *testing.T, pool *pgxpool.Pool, issue string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from outbox where issue = $1 and kind = 'dispatch_status'`, issue).Scan(&count); err != nil {
		t.Fatalf("count the Dispatch status writes of %s: %v", issue, err)
	}
	return count
}

// seedWorkerWithLostTask records key as a started issue standing in stands, holding a row, whose
// phase worker has lost its task: the claim the daemon suspended still holds the task of this
// run, and nothing is queued to start the role again. It is seedSlotRepairRow's repaired fixture
// over a root of its own (seedStarted) — the one the repair of a slot whose worker has no task
// starts again whenever it reads that issue's row (resumeSlottedWorkers).
func seedWorkerWithLostTask(t *testing.T, pool *pgxpool.Pool, key string, stands phase.Phase) claim.Token {
	t.Helper()
	seedStarted(t, pool, key, stands, nil)
	role := workflow.RoleFor(stands)
	token := putWorkerClaim(t, pool, key, key, role, 1, supervise.StateSuspended, supervise.SuspendedByDaemon)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: key, Role: role,
			Claim: token, Assignment: seededAssignment(key)}); err != nil {
			t.Fatalf("record the %s's phases row of %s: %v", role, key, err)
		}
	})
	putWorkerTask(t, pool, token, 1, stands, true)
	return token
}

// Giving an admission slot back for a wait stops no work. The row says how many issues Legion is
// working on, and nothing else: the issue's worker keeps the session and the state it was in, no
// suspension is asked of it, its phase stands where it stood, and Dispatch is told nothing — a
// person reading the issue in Dispatch sees the same issue, because nothing about it changed.
//
// What the row does decide keeps its meaning, and this reads the four rules that ask it. The
// repair of a slot whose worker has no task walks the rows (resumeSlottedWorkers), so while an
// issue waits its lost worker is not started again, and the row's return is what starts it: the
// repair's reach is the issues the daemon is working on, which a waiting issue is not. A phase
// that ends while the wait stands owes its next worker a start held for the issue's own row
// (record.SuperviseRequest.NeedsSlot), which is the mark every phase start of a slot-holding
// phase carries, so the work resumes with the row and never before it. And the two rules that
// ask the row about a root both answer of a root whose design gate has never opened — its
// architect writing the design, the whole of that tree's work — that it holds its row: it may
// hand its work to a person (record.CanWaitOnPerson), and it is no root waiting for its tree to
// be opened with no architect running (record.WaitsForSlot), so a person's decision on it
// reaches that architect rather than being kept for a start that is never queued.
func TestReturningASlotStopsNothing(t *testing.T) {
	t.Run("the work of a waiting issue is untouched", func(t *testing.T) {
		const (
			waiter  = "LEGION-10"
			blocker = "LEGION-90"
		)
		pool := migratedPool(t)
		admission := quiet(t, 2)
		seedStarted(t, pool, waiter, phase.Implementing, nil)
		seedMirror(t, pool, waiter, "in_progress", blocker)
		seedMirror(t, pool, blocker, "in_progress")
		token := putWorkerClaim(t, pool, waiter, waiter, claim.RoleImplementer, 1, supervise.StateWorking, "")
		putWorkerTask(t, pool, token, 1, phase.Implementing, true)
		before := claimStates(t, pool)

		tick(t, pool, admission, "the-wait-begins")

		if holdsSlot(t, pool, waiter) {
			t.Fatalf("%s holds a row while it waits on %s, want none", waiter, blocker)
		}
		if got := claimStates(t, pool); got[string(token)] != before[string(token)] {
			t.Errorf("the implementer's claim = %s, want the %s it was in: the row says how many issues Legion is working on and nothing about the worker",
				got[string(token)], before[string(token)])
		}
		if suspends := superviseOps(t, pool, waiter, "suspend"); suspends != 0 {
			t.Errorf("the supervise rows of %s ask for %d suspensions, want none: no claim is suspended for a wait", waiter, suspends)
		}
		if got := issue(t, pool, waiter).Phase; got != phase.Implementing {
			t.Errorf("%s stands in %s, want implementing: the row went and the work stayed", waiter, got)
		}
		if got := statusWrites(t, pool, waiter); got != 0 {
			t.Errorf("the Dispatch status writes of %s = %d, want none: nothing of the issue moved, so Dispatch is told nothing", waiter, got)
		}
	})

	t.Run("a worker with no task is not started again while its issue waits", func(t *testing.T) {
		for _, stands := range []phase.Phase{phase.Implementing, phase.Merging} {
			t.Run(string(stands), func(t *testing.T) {
				const (
					waiter  = "LEGION-20"
					blocker = "LEGION-91"
				)
				pool := migratedPool(t)
				admission := quiet(t, 2)
				seedWorkerWithLostTask(t, pool, waiter, stands)
				seedMirror(t, pool, waiter, "in_progress", blocker)
				seedMirror(t, pool, blocker, "in_progress")

				tick(t, pool, admission, "the-wait-begins")

				if holdsSlot(t, pool, waiter) {
					t.Fatalf("%s holds a row while it waits, want none", waiter)
				}
				if got := startsOf(t, pool, waiter); len(got) != 0 {
					t.Fatalf("the starts of %s while it waits = %+v, want none: the repair of a slot whose worker has no task walks the rows, and this issue holds none",
						waiter, got)
				}

				dispatchEvent(t, pool, admission, "the-wait-ends", blocker, "done")

				if !holdsSlot(t, pool, waiter) {
					t.Fatalf("%s holds no row once its wait ended, want the one it gave back", waiter)
				}
				got := startsOf(t, pool, waiter)
				role := workflow.RoleFor(stands)
				if len(got) != 1 || got[0].role != role || !got[0].needsSlot {
					t.Fatalf("the starts of %s once it holds a row again = %+v, want the %s's one row, held for the issue's own slot: the row is back, so the repair reads the issue again",
						waiter, got, role)
				}
				if want := workflow.ResumePhaseTask(issue(t, pool, waiter)); got[0].task != want {
					t.Fatalf("the start of %s carries %q, want the phase's resume task %q", waiter, got[0].task, want)
				}
			})
		}
	})

	t.Run("the phase that ends while the wait stands owes a start held for the row", func(t *testing.T) {
		const (
			waiter  = "LEGION-30"
			blocker = "LEGION-92"
		)
		pool := migratedPool(t)
		admission := quiet(t, 2)
		engine := admission.engine
		seedStarted(t, pool, waiter, phase.Implementing, nil)
		seedMirror(t, pool, waiter, "in_progress", blocker)
		seedMirror(t, pool, blocker, "in_progress")
		inTx(t, pool, func(tx pgx.Tx) {
			ctx, records := context.Background(), record.NewStore()
			// The pull request the implementer's handoff is read against: its phase moves on
			// once its work is on a branch GitHub holds (the workflow's transition table).
			if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: waiter,
				Repo: "acme/widgets", Number: 42, Branch: "legion/" + waiter, HeadSHA: "head", CheckedHead: "head",
				Failing: []string{}}); err != nil {
				t.Fatalf("seed the pull request of %s: %v", waiter, err)
			}
			if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: waiter, Role: claim.RoleImplementer,
				Claim: "implementer", Assignment: seededAssignment(waiter)}); err != nil {
				t.Fatalf("seed the implementer's phases row of %s: %v", waiter, err)
			}
		})

		tick(t, pool, admission, "the-wait-begins")
		if holdsSlot(t, pool, waiter) {
			t.Fatalf("%s holds a row while it waits, want none", waiter)
		}

		if _, err := intake.ApplyFact(context.Background(), pool, "api", "implemented", intake.HandoffComplete{
			Generation: 1, Issue: waiter, Role: claim.RoleImplementer, Claim: "implementer", Summary: "implemented",
			Commit: "head", HandoffRecord: "implement", HandoffVersion: 1}, engine, admission); err != nil {
			t.Fatalf("complete the implementer's phase of %s: %v", waiter, err)
		}

		if got := issue(t, pool, waiter).Phase; got != phase.Testing {
			t.Fatalf("%s stands in %s once its implementer handed off, want testing: the wait stops no transition", waiter, got)
		}
		owed := startsOf(t, pool, waiter)
		if len(owed) != 1 || owed[0].role != claim.RoleTester || !owed[0].needsSlot || owed[0].claimable {
			t.Fatalf("the starts of %s = %+v, want the tester's one row, held for the issue's own slot and claimable by no lane: every phase start of a slot-holding phase waits for the row",
				waiter, owed)
		}
		if holdsSlot(t, pool, waiter) {
			t.Fatalf("%s holds a row while it still waits, want none: the phase moved and the wait stands", waiter)
		}

		dispatchEvent(t, pool, admission, "the-wait-ends", blocker, "done")

		if !holdsSlot(t, pool, waiter) {
			t.Fatalf("%s holds no row once its wait ended, want the one it gave back", waiter)
		}
		if got := startsOf(t, pool, waiter); len(got) != 1 || !got[0].claimable {
			t.Fatalf("the starts of %s once its wait ended = %+v, want the tester's one row claimable: the row's return is what delivers the task", waiter, got)
		}
	})

	t.Run("a root whose design gate has never opened keeps its row", func(t *testing.T) {
		const (
			root    = "LEGION-40"
			blocker = "LEGION-93"
		)
		pool := migratedPool(t)
		admission := quiet(t, 2)
		seedStarted(t, pool, root, phase.Admitted, nil)
		seedMirror(t, pool, root, "in_progress", blocker)
		seedMirror(t, pool, blocker, "in_progress")

		tick(t, pool, admission, "the-pass")

		if !holdsSlot(t, pool, root) {
			t.Fatalf("%s holds no row, want the one its architect is writing the design in: the whole of that tree's work is its own, so its link holds it at nothing", root)
		}
		stored := issue(t, pool, root)
		inTx(t, pool, func(tx pgx.Tx) {
			ctx, records := context.Background(), record.NewStore()
			issues, err := records.LiveIssues(ctx, tx, testProject)
			if err != nil {
				t.Fatalf("list the project's issues: %v", err)
			}
			facts, err := record.ReadLineFacts(ctx, tx, records, testProject, issues, admission.engine.GatedSteps())
			if err != nil {
				t.Fatalf("read the line facts: %v", err)
			}
			if !record.CanWaitOnPerson(stored, facts.Gates[stored.Tree], true) {
				t.Fatalf("the root whose gate has never opened may wait on no person, want it able to: the row it kept is what says the daemon started it")
			}
			waits, err := record.WaitsForSlot(ctx, tx, records, stored, admission.engine.GatedSteps())
			if err != nil {
				t.Fatalf("read whether %s waits for its slot: %v", root, err)
			}
			if waits {
				t.Fatalf("%s is read as a root waiting for its slot, want it read as one whose architect runs: a person's decision on it reaches that architect rather than being kept for a start that is never queued", root)
			}
		})
		if suspends := superviseOps(t, pool, root, "suspend"); suspends != 0 {
			t.Errorf("the supervise rows of %s ask for %d suspensions, want none", root, suspends)
		}
	})
}
