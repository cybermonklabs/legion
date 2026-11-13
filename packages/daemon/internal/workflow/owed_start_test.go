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
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// servedEntry is the phase entry the role's worker is still on when the return lands: the entry
// before it, whose task its turn may outlive, since the daemon ends no turn when a phase ends.
const servedEntry = 1

// returnIssue is the issue every return of the table below stands on, which the package's own
// readers (issuePhase, startTask) name.
const returnIssue = "LEGION-208"

// seedReturningWorker records the claim of role as a worker between turns is left: live, holding
// no task, and still serving the phase entry it last worked. A claim in that state is what the
// return's start row finds, and what a completion of the earlier entry would be weighed against
// (api/handoff.go).
func seedReturningWorker(t *testing.T, pool *pgxpool.Pool, role claim.Role) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, returnIssue, role)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, serving_generation, given_entry, served_entry, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $2, $3, $3, $4, 1, 'ses', '/sessions/ses.jsonl', 'idle', 1, $5, $5, 0, 0, 0, 0)`,
		string(token), project, returnIssue, string(role), servedEntry); err != nil {
		t.Fatalf("seed the %s between turns: %v", role, err)
	}
	return token
}

// startRow is one supervise start row of the issue, as the one row a start is written as leaves
// it (StartOrOwe): it carries the task, the entry that task is of, and waits for the issue's
// slot.
type startRow struct {
	id        int64
	role      claim.Role
	task      string
	entry     int64
	needsSlot bool
	claimable bool
}

// startRowsOf is every supervise start row the issue holds, oldest first, each marked with
// whether a lane may run it now (record.Store.DueRows, the one predicate the outbox claims by).
func startRowsOf(t *testing.T, pool *pgxpool.Pool, issue string, now time.Time) []startRow {
	t.Helper()
	var rows []startRow
	seedRecord(t, pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		due, err := store.DueRows(t.Context(), tx, "LEGION", issue, []record.OutboxKind{record.OutboxKindSupervise}, now)
		if err != nil {
			return err
		}
		runnable := make(map[int64]struct{}, len(due))
		for _, row := range due {
			runnable[row.ID] = struct{}{}
		}
		all, err := tx.Query(t.Context(), `select id, coalesce(payload->>'role', ''), coalesce(payload->>'task', ''),
			coalesce((payload->>'entry')::bigint, 0), coalesce(payload->>'needsSlot', '') = 'true'
			from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, issue)
		if err != nil {
			return err
		}
		defer all.Close()
		for all.Next() {
			var row startRow
			if err := all.Scan(&row.id, &row.role, &row.task, &row.entry, &row.needsSlot); err != nil {
				return err
			}
			_, row.claimable = runnable[row.id]
			rows = append(rows, row)
		}
		return all.Err()
	})
	return rows
}

// assertTransitionApplied is A3's first assertion: the issue's phase, the role's phase entry on
// its phases row and the Dispatch status write are all there the moment the fact's transaction
// commits, whatever the admission cap is. ApplyFact commits one transaction and nothing else has
// run, so what is readable here was written in the fact's own.
//
// A start that hands the role work it was not already on raises that role's entry (enter); a held
// phase's retry hands back the work its worker already has, so it raises none and the row keeps
// the entry its worker is on.
func assertTransitionApplied(t *testing.T, pool *pgxpool.Pool, want phase.Phase, role claim.Role, status string, keeps bool) int64 {
	t.Helper()
	if got := issuePhase(t, pool); got != want {
		t.Fatalf("the issue is in %s, want %s: the transition applies in the fact's own transaction", got, want)
	}
	entry := entryRow(t, pool, role).Entry
	switch {
	case keeps && entry != servedEntry:
		t.Fatalf("the %s's row is at entry %d, want the %d its worker is still on: a retry hands back the work it has", role, entry, servedEntry)
	case !keeps && entry <= servedEntry:
		t.Fatalf("the %s's row is at entry %d, want one above the %d its worker is on: the transition hands it work it was not on", role, entry, servedEntry)
	}
	var written string
	if err := pool.QueryRow(context.Background(), `select coalesce((select payload->>'status' from outbox
		where kind = 'dispatch_status' and issue = $1 order by id desc limit 1), '')`, returnIssue).Scan(&written); err != nil {
		t.Fatalf("read the Dispatch status write: %v", err)
	}
	if written != status {
		t.Fatalf("the Dispatch status write says %q, want %q", written, status)
	}
	return entry
}

// assertOwesOneStart is A3's second and third assertions: the issue owes its worker exactly one
// start, however many transitions wrote one, and that row waits for the issue's slot and is
// runnable by nobody until it comes. The claim is left as a worker between turns is — ready or
// idle, holding no task — because nothing of the start runs before the slot.
func assertOwesOneStart(t *testing.T, pool *pgxpool.Pool, role claim.Role, entry int64, task string, now time.Time) {
	t.Helper()
	rows := startRowsOf(t, pool, returnIssue, now)
	if len(rows) != 1 {
		t.Fatalf("the issue holds the start rows %+v, want exactly one: a start is one row that carries the task", rows)
	}
	owed := rows[0]
	if !owed.needsSlot {
		t.Fatalf("the start row is %+v, want it marked for the issue's slot: the cap counts the issues at work", owed)
	}
	if owed.role != role || owed.entry != entry || owed.task != task || owed.claimable {
		t.Fatalf("the owed start is %+v, want the %s's, of entry %d carrying task %q, runnable by nobody while the issue holds no slot",
			owed, role, entry, task)
	}
	var state string
	var pending int
	if err := pool.QueryRow(context.Background(), `select c.state, (select count(*) from pending_task_deliveries d where d.claim_token = c.token)
		from claims c where c.issue = $1 and c.role = $2`, returnIssue, string(role)).Scan(&state, &pending); err != nil {
		t.Fatalf("read the %s's claim: %v", role, err)
	}
	if (state != string(supervise.StateReady) && state != string(supervise.StateIdle)) || pending != 0 {
		t.Fatalf("the %s's claim is %s with %d pending deliveries, want it ready or idle with none: its task waits in the outbox, not on the claim", role, state, pending)
	}
}

// assertNotStranded is A3's fourth assertion: nothing reports the worker whose task waits for a
// slot as a claim nothing will wake. A claim between turns is no candidate of Stranded at all,
// and the rule that carries the case is the one for a suspended worker: the start it waits for is
// queued, unclaimable until the slot comes, and StartFor counts that queued row, so the worker is
// covered whatever suspends it next.
func assertNotStranded(t *testing.T, pool *pgxpool.Pool, role claim.Role, token claim.Token, stands phase.Phase) {
	t.Helper()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	live := supervise.Claim{Token: token, Project: project, Tree: returnIssue, Issue: returnIssue, Role: role, Generation: 1, State: supervise.StateIdle}
	suspended := live
	suspended.State = supervise.StateSuspended
	seedRecord(t, pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		for _, c := range []supervise.Claim{live, suspended} {
			stranded, err := Stranded(t.Context(), tx, store, "LEGION", nil, []supervise.Claim{c}, nil, time.Now().UTC())
			if err != nil {
				return err
			}
			if len(stranded) != 0 {
				t.Fatalf("Stranded names %+v for the %s claim, want nothing: its start is queued, held for the issue's slot", stranded, c.State)
			}
		}
		runs, err := store.RoleRuns(t.Context(), tx, []record.RoleRunKey{{Token: token, Issue: returnIssue, Role: role, Generation: 1}})
		if err != nil {
			return err
		}
		root, err := store.Issue(t.Context(), tx, returnIssue)
		if err != nil {
			return err
		}
		if StartFor(runs[token], *root, 1, stands) {
			t.Fatalf("StartFor says the %s needs a start, want the queued one the slot holds counted", role)
		}
		return nil
	})
}

// assertTheSlotDeliversOnce is A3's fifth assertion: the slot's grant makes the owed row runnable
// and nothing else of the issue, so one task is delivered however many transitions wrote it, and
// that task is the last transition's. The row that carries it is of the later entry while the
// claim has served only the earlier one, which is the pair api/handoff.go refuses a completion
// on (internal/api's TestAStaleEntrysCompletionDoesNotTakeTheNextTestingPhases drives that
// refusal through the route).
func assertTheSlotDeliversOnce(t *testing.T, pool *pgxpool.Pool, role claim.Role, entry int64, task string, now time.Time) {
	t.Helper()
	seedSlot(t, pool, record.Slot{Issue: returnIssue, Index: 0, AdmittedAt: now})
	var runnable []startRow
	for _, row := range startRowsOf(t, pool, returnIssue, now) {
		if row.claimable && row.task != "" {
			runnable = append(runnable, row)
		}
	}
	if len(runnable) != 1 || runnable[0].role != role || runnable[0].task != task || runnable[0].entry != entry {
		t.Fatalf("the slot made %+v deliverable, want the one task %q of the %s, of entry %d", runnable, task, role, entry)
	}
	var served int64
	if err := pool.QueryRow(context.Background(), `select served_entry from claims where issue = $1 and role = $2`,
		returnIssue, string(role)).Scan(&served); err != nil {
		t.Fatalf("read what the %s has served: %v", role, err)
	}
	if served != servedEntry {
		t.Fatalf("the %s has served entry %d, want the earlier %d it was given", role, served, servedEntry)
	}
}

// A3. Every return the daemon makes for a worker applies its transition at once and owes that
// worker one start, which waits for the issue's admission slot and for nothing else.
//
// The cap counts the issues at work, so a worker's start is the one thing a transition may not do
// before its issue holds a slot. Everything else is written in the fact's own transaction: the
// phase, the role's phase entry and the work of it on its phases row, the Dispatch status and
// the notices. The start is one row (StartOrOwe), which carries the worker's task and which the
// outbox holds unclaimable while the slots table names no slot of the issue (store.go's
// outboxInOrder), so the worker comes back with its work rather than ahead of it.
//
// The rows are the returns the spec names, each of which starts a worker on an issue that held a
// slot before: a red head, a head that conflicts with its base, the merge queue's failed_checks
// removal, a push past READY, sign_off, a backward move and a held worker's retry. The eighth, a
// re-admitted tree's mid-phase children, is admission's own start and is proven where it lives
// (internal/admit's TestAReadmittedTreesMidPhaseChildrenEachWaitForTheirOwnSlot).
//
// Two transitions before the slot start the worker once: the second takes out the pending row
// and writes its own in its place (record.Store.EnqueueStart), carrying that row's creation time
// — the waiting line's ordering key — and the later task, so one task is delivered when the slot
// comes.
func TestEveryReturnAppliesAtOnceAndOwesItsWorkerOneStart(t *testing.T) {
	readyRoot := func(t *testing.T, pool *pgxpool.Pool, at phase.Phase, status string) {
		t.Helper()
		seedIssue(t, pool, record.Issue{Key: returnIssue, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: returnIssue, Project: "LEGION", Title: "root", Phase: at, Generation: 1, Status: status, Rank: "U", HandedOver: true})
		seedTreeLifecycle(t, pool, returnIssue)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: returnIssue, Repo: "sjawhar/legion",
			Number: 42, Branch: "legion/" + returnIssue, HeadSHA: "head"})
		seedPhase(t, pool, record.PhaseRow{Issue: returnIssue, Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: "READY #42 at head"})
	}
	for _, tc := range []struct {
		name string
		// seed records the issue as the return finds it, with the phases row of the role the
		// return starts naming the entry its worker is still on.
		seed func(t *testing.T, pool *pgxpool.Pool)
		// fact is the return, and second a transition that lands before the slot does.
		fact, second intake.Fact
		engine       func() *Engine
		role         claim.Role
		want         phase.Phase
		// status is the Dispatch status the transition writes, "" where the board already shows it.
		status string
		// keeps says the start hands the role back the work it already has, so it raises no
		// entry and the row keeps the one its worker is on.
		keeps bool
		// task is a phrase the delivered task carries, which tells the later transition's task
		// from the earlier one's.
		task string
	}{
		{
			name: "a head that conflicts with its base",
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				readyRoot(t, pool, phase.Merging, "retro")
				seedPhase(t, pool, record.PhaseRow{Issue: returnIssue, Role: claim.RoleImplementer, Claim: "implement-claim", Entry: servedEntry})
			},
			fact:   intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", Base: "main", Mergeable: record.MergeabilityConflicting},
			engine: func() *Engine { return testEngine(config.DesignGateRootIssues, nil) },
			role:   claim.RoleImplementer, want: phase.Implementing, status: "in_progress", task: "the head conflicts with main",
		},
		{
			name: "a backward move",
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				readyRoot(t, pool, phase.Merging, "retro")
				seedPhase(t, pool, record.PhaseRow{Issue: returnIssue, Role: claim.RoleReviewer, Claim: "review-claim", Entry: servedEntry,
					HandoffRecord: "review-1", HandoffVersion: 1, Summary: "reviewed"})
			},
			fact:   intake.BackwardMove{Issue: returnIssue, Requester: claim.RoleMerger, To: phase.Reviewing, Reason: "the packet names no migration"},
			engine: func() *Engine { return testEngine(config.DesignGateRootIssues, nil) },
			role:   claim.RoleReviewer, want: phase.Reviewing, status: "needs_review", task: "the packet names no migration",
		},
		{
			name: "a held worker's retry",
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				seedIssue(t, pool, record.Issue{Key: returnIssue, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
					Tree: returnIssue, Project: "LEGION", Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Testing},
					Generation: 1, Status: "testing", Rank: "U", HandedOver: true})
				seedTreeLifecycle(t, pool, returnIssue)
				seedPhase(t, pool, record.PhaseRow{Issue: returnIssue, Role: claim.RoleTester, Claim: "test-claim", Entry: servedEntry})
			},
			fact:   intake.RetryOrEscalate{Issue: returnIssue, Decision: intake.RetryDecision},
			engine: func() *Engine { return testEngine(config.DesignGateRootIssues, nil) },
			role:   claim.RoleTester, want: phase.Testing, keeps: true, task: "Reason: retry held phase.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			tc.seed(t, pool)
			token := seedReturningWorker(t, pool, tc.role)
			engine := tc.engine()

			applyRefusingNothing(t, pool, engine, tc.fact)
			if tc.second != nil {
				first := entryRow(t, pool, tc.role).Entry
				applyRefusingNothing(t, pool, engine, tc.second)
				if again := entryRow(t, pool, tc.role).Entry; again <= first {
					t.Fatalf("the second transition left the %s's row at entry %d, want one of its own above it", tc.role, first)
				}
			}

			entry := assertTransitionApplied(t, pool, tc.want, tc.role, tc.status, tc.keeps)
			task := startTask(t, pool, tc.role)
			if task == "" || !strings.Contains(task, tc.task) {
				t.Fatalf("the %s's task %q, want the last transition's, naming %q", tc.role, task, tc.task)
			}
			assertOwesOneStart(t, pool, tc.role, entry, task, engine.now())
			assertNotStranded(t, pool, tc.role, token, tc.want)
			assertTheSlotDeliversOnce(t, pool, tc.role, entry, task, engine.now())
		})
	}
}
