package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// stateProject is the Dispatch project key the daemon of these tests works for, and stateRun the
// run id its state rows carry. Its claims are of the project token "legion".
const (
	stateProject = "LEGION"
	stateRun     = "0b9f5c2e-4f1d-4c8a-9d57-2f3a6e8b1c40"
)

// stateTask is the task a claim of these tests is confirmed with and retires: a taken task, whose
// accepted take is its confirmation (supervise's accept).
var stateTask = supervise.Delivery{ID: "delivery-1", Task: "test it", QueuedAt: at(1), ConfirmedAt: at(3), Generation: 1}

// stateClaim is the claim of role on issue, in the tree issue roots, in state.
func stateClaim(t *testing.T, issue string, role claim.Role, state supervise.ClaimState) supervise.Claim {
	t.Helper()
	token, err := claim.NewToken("legion", issue, role)
	if err != nil {
		t.Fatalf("the token of %s's %s: %v", issue, role, err)
	}
	return supervise.Claim{Token: token, Project: "legion", Tree: issue, Issue: issue, Role: role, Generation: 1, State: state}
}

// controllerClaim is the project controller's claim in state: on no issue and in no tree.
func controllerClaim(state supervise.ClaimState) supervise.Claim {
	return supervise.Claim{Token: claim.ControllerToken("legion"), Project: "legion", Role: claim.RoleController, Generation: 1, State: state}
}

// stateWakes is each time the store told the state events' wake (PublishState), with the number
// of state rows another connection could read then: the rows the lane it wakes would find.
type stateWakes struct {
	visible []int
}

// publishStates turns s's state events on for stateProject and stateRun, through the queue the
// daemon hands it (record.Postgres.EnqueueStateChange), and answers the wakes the store tells from
// then on.
func publishStates(t *testing.T, s *Store) *stateWakes {
	t.Helper()
	wakes := &stateWakes{}
	s.PublishState(stateProject, stateRun, record.NewStore().EnqueueStateChange, func() {
		var visible int
		if err := s.pool.QueryRow(context.Background(), `select count(*) from outbox where kind = $1`,
			string(record.OutboxKindStateChange)).Scan(&visible); err != nil {
			t.Errorf("read the state rows when the store told their wake: %v", err)
		}
		wakes.visible = append(wakes.visible, visible)
	})
	return wakes
}

// recordIssue writes the issues row of key, of the Dispatch project project, as the workflow
// records an issue it runs.
func recordIssue(t *testing.T, s *Store, key, project string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ($1, $1, $2, 'recorded', 'implementing', 1, 'in_progress', 'A', 1)`, key, project); err != nil {
		t.Fatalf("record %s in %s: %v", key, project, err)
	}
}

// openTree opens the lifecycle of the tree key roots, which an admission binds to.
func openTree(t *testing.T, s *Store, key string) {
	t.Helper()
	if _, err := s.OpenTreeLifecycle(context.Background(), "legion", key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", key, err)
	}
}

// sentStates is every state row the outbox holds, each decoded as the daemon's executor decodes
// it (record.DecodeOutboxPayload), so a row this package writes in a shape the executor or the
// contract refuses fails here rather than in the lane; and the rows go, as a lane that published
// them deletes them. Each row is held to what every state row of the claim writes carries: the
// bare project key in its issue column, the project and the run in its payload, and the sequence
// its transaction's commit gave it.
func sentStates(t *testing.T, s *Store) []record.StateChange {
	t.Helper()
	ctx := context.Background()
	rows, err := s.pool.Query(ctx, `select id, kind, issue, payload, state_seq is not null from outbox
		where kind in ('state_change', 'state_admission', 'state_boot') order by id`)
	if err != nil {
		t.Fatalf("read the queued state rows: %v", err)
	}
	type stateRow struct {
		row       record.OutboxRow
		sequenced bool
	}
	queued, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (stateRow, error) {
		var r stateRow
		var kind string
		err := row.Scan(&r.row.ID, &kind, &r.row.Issue, &r.row.Payload, &r.sequenced)
		r.row.Kind = record.OutboxKind(kind)
		return r, err
	})
	if err != nil {
		t.Fatalf("read the queued state rows: %v", err)
	}
	changes := []record.StateChange{}
	for _, queued := range queued {
		payload, err := record.DecodeOutboxPayload(queued.row)
		if err != nil {
			t.Fatalf("decode the state row %d as the executor does: %v", queued.row.ID, err)
		}
		change, ok := payload.(record.StateChange)
		if !ok {
			t.Fatalf("the state row %d is a %s, want a state change", queued.row.ID, queued.row.Kind)
		}
		if queued.row.Issue != stateProject || change.Project != stateProject || change.Run != stateRun {
			t.Fatalf("the state change of %s is queued under %q for project %q and run %q, want %q, %q and %q",
				change.Issue, queued.row.Issue, change.Project, change.Run, stateProject, stateProject, stateRun)
		}
		if !queued.sequenced {
			t.Fatalf("the state change of %s has no sequence, which its transaction's commit gives it", change.Issue)
		}
		changes = append(changes, change)
	}
	if _, err := s.pool.Exec(ctx, `delete from outbox where kind in ('state_change', 'state_admission', 'state_boot')`); err != nil {
		t.Fatalf("send the queued state rows: %v", err)
	}
	return changes
}

// moved is each field of a state change as one line, `field[/role] old -> new`, as the tests
// compare and print it.
func moved(changes []record.StateFieldChange) []string {
	lines := make([]string, 0, len(changes))
	for _, change := range changes {
		field := string(change.Field)
		if change.Role != "" {
			field += "/" + string(change.Role)
		}
		lines = append(lines, fmt.Sprintf("%s %s -> %s", field, change.Old, change.New))
	}
	return lines
}

// claimMoved is the line of role's claim moving from one state to another, each served as the
// state string, or null for the empty state.
func claimMoved(role claim.Role, from, to supervise.ClaimState) string {
	served := func(state supervise.ClaimState) string {
		if state == "" {
			return "null"
		}
		return strconv.Quote(string(state))
	}
	return fmt.Sprintf("claim/%s %s -> %s", role, served(from), served(to))
}

// phaseUnrecorded is the line of an issue's phase moving from null to unrecorded, where the state
// route first lists an issue the daemon holds no record of (api.Unrecorded).
const phaseUnrecorded = `phase null -> "unrecorded"`

// queues runs write and holds what it committed to want: one state change of issue naming the
// lines of want in order, and one wake told with that row already visible; or, for no want, no
// state row and no wake.
func queues(t *testing.T, s *Store, wakes *stateWakes, name string, write func() error, issue string, want ...string) {
	t.Helper()
	wakes.visible = nil
	if err := write(); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	changes := sentStates(t, s)
	if len(want) == 0 {
		if len(changes) != 0 || len(wakes.visible) != 0 {
			t.Fatalf("%s queued the state changes %+v and told the wake %d times, want neither", name, changes, len(wakes.visible))
		}
		return
	}
	if len(changes) != 1 || changes[0].Issue != issue || !slices.Equal(moved(changes[0].Changes), want) {
		t.Fatalf("%s queued the state changes %+v, want one of %s naming %q", name, changes, issue, want)
	}
	if !slices.Equal(wakes.visible, []int{1}) {
		t.Fatalf("%s told the wake with %v state rows visible, want once, with its row", name, wakes.visible)
	}
}

// A claim's state is one of the fields Dispatch observes of an issue, and the claim write that
// moves it queues one state_change row in its own transaction, naming the claim with its role,
// from the state the row it replaced held to the one it writes, null where the store held no row
// yet. Every write that stores a claim does: PutClaim, PutClaimAndDelivery, RetireDelivery and
// AdmitClaim. A write of the state the row already holds queues none, and neither does any write
// of the controller's claim, which the state route shows on no issue. A claim on an issue the
// daemon holds no record of is shown at phase unrecorded, so the write that first lists such an
// issue names that phase too, from null, and a later write of it, the phase already served, names
// the claim alone. A write that rolls back leaves no row and no step of the sequence, and a store
// the daemon has not turned the events on for queues none. Each row is read back through record's
// decoder, which holds its payload to record.StateChange.
func TestAClaimWriteThatChangesItsStateQueuesOneStateEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const issue = "LEGION-301"
	tester := func(t *testing.T, state supervise.ClaimState) supervise.Claim {
		return stateClaim(t, issue, claim.RoleTester, state)
	}
	admit := func(s *Store, c supervise.Claim) func() error {
		return func() error { _, err := s.AdmitClaim(ctx, c); return err }
	}
	put := func(s *Store, c supervise.Claim) func() error {
		return func() error { return s.PutClaim(ctx, c) }
	}
	confirm := func(s *Store, c supervise.Claim) func() error {
		return func() error { return s.PutClaimAndDelivery(ctx, c, stateTask) }
	}
	retire := func(s *Store, c supervise.Claim) func() error {
		return func() error { return s.RetireDelivery(ctx, c, stateTask.ID) }
	}

	t.Run("a write that moves the claim's state queues one row from its old state to its new", func(t *testing.T) {
		s := migratedStore(t)
		wakes := publishStates(t, s)
		recordIssue(t, s, issue, stateProject)
		openTree(t, s, issue)
		for _, step := range []struct {
			name     string
			write    func() error
			from, to supervise.ClaimState
		}{
			{"AdmitClaim of a claim the store does not hold", admit(s, tester(t, supervise.StateLaunching)), "", supervise.StateLaunching},
			{"PutClaim", put(s, tester(t, supervise.StateRegistered)), supervise.StateLaunching, supervise.StateRegistered},
			{"PutClaimAndDelivery", confirm(s, tester(t, supervise.StateWorking)), supervise.StateRegistered, supervise.StateWorking},
			{"RetireDelivery", retire(s, tester(t, supervise.StateIdle)), supervise.StateWorking, supervise.StateIdle},
			{"AdmitClaim over the row the store holds", admit(s, tester(t, supervise.StateLaunching)), supervise.StateIdle, supervise.StateLaunching},
			{"PutClaim of a retirement", put(s, tester(t, supervise.StateRetired)), supervise.StateLaunching, supervise.StateRetired},
		} {
			queues(t, s, wakes, step.name, step.write, issue, claimMoved(claim.RoleTester, step.from, step.to))
		}
	})

	t.Run("a write of the state the row already holds queues none", func(t *testing.T) {
		s := migratedStore(t)
		// Both of the claim writes' switches, as a daemon with a workflow turns them on: a write
		// that moves the role holder and not the state queues the holder's publication alone.
		s.PublishRoleHolders(nil)
		wakes := publishStates(t, s)
		recordIssue(t, s, issue, stateProject)
		openTree(t, s, issue)
		queues(t, s, wakes, "the admission", admit(s, tester(t, supervise.StateLaunching)), issue,
			claimMoved(claim.RoleTester, "", supervise.StateLaunching))
		queues(t, s, wakes, "AdmitClaim again", admit(s, tester(t, supervise.StateLaunching)), issue)
		queues(t, s, wakes, "PutClaim again", put(s, tester(t, supervise.StateLaunching)), issue)
		queues(t, s, wakes, "the registration", put(s, tester(t, supervise.StateRegistered)), issue,
			claimMoved(claim.RoleTester, supervise.StateLaunching, supervise.StateRegistered))
		queues(t, s, wakes, "PutClaimAndDelivery again", confirm(s, tester(t, supervise.StateRegistered)), issue)
		queues(t, s, wakes, "RetireDelivery again", retire(s, tester(t, supervise.StateRegistered)), issue)
		sentRoleHolders(t, s)
		moving := tester(t, supervise.StateRegistered)
		moving.Session = "ses-2"
		queues(t, s, wakes, "a new session in the same state", put(s, moving), issue)
		if got := queuedRoleHolders(t, s); !slices.Equal(got, []string{issue}) {
			t.Fatalf("a new session in the same state queued the role-holder publications of %v, want %s's", got, issue)
		}
	})

	t.Run("the controller's claim queues none", func(t *testing.T) {
		s := migratedStore(t)
		traced, begun := tracedStore(t, s, false)
		wakes := publishStates(t, traced)
		for _, step := range []struct {
			name  string
			write func() error
		}{
			{"AdmitClaim", admit(traced, controllerClaim(supervise.StateLaunching))},
			{"PutClaim", put(traced, controllerClaim(supervise.StateRegistered))},
			{"PutClaimAndDelivery", confirm(traced, controllerClaim(supervise.StateWorking))},
			{"RetireDelivery", retire(traced, controllerClaim(supervise.StateIdle))},
		} {
			queues(t, traced, wakes, "the controller's "+step.name, step.write, "")
		}
		// It can queue nothing, and PutClaim still writes it in one transaction of its own, as it
		// writes every claim (TestEveryClaimWriteOpensOneTransaction); a worker's claim on the same
		// store takes one too, with the row it queues.
		before := begun.begun.Load()
		if err := traced.PutClaim(ctx, controllerClaim(supervise.StateSuspended)); err != nil {
			t.Fatalf("the controller's suspension: %v", err)
		}
		if got := begun.begun.Load() - before; got != 1 {
			t.Errorf("the controller's PutClaim began %d transactions, want one", got)
		}
		recordIssue(t, s, issue, stateProject)
		before = begun.begun.Load()
		queues(t, traced, wakes, "a tester's PutClaim on the same store", put(traced, tester(t, supervise.StateRegistered)), issue,
			claimMoved(claim.RoleTester, "", supervise.StateRegistered))
		if got := begun.begun.Load() - before; got != 1 {
			t.Errorf("a tester's PutClaim began %d transactions, want one with the row it queues", got)
		}
	})

	t.Run("a claim on an issue with no record also names its phase, unrecorded", func(t *testing.T) {
		s := migratedStore(t)
		wakes := publishStates(t, s)
		const unrecorded, otherProjects, recorded, fixture = "LEGION-302", "LEGION-303", "LEGION-304", "LEGION-305"
		// An issue recorded under another Dispatch project is no record of this daemon's: its
		// state route lists the issues of its own project alone.
		recordIssue(t, s, otherProjects, "OTHER")
		recordIssue(t, s, recorded, stateProject)
		for _, key := range []string{unrecorded, otherProjects, recorded} {
			openTree(t, s, key)
		}
		queues(t, s, wakes, "the first admission on an issue with no record",
			admit(s, stateClaim(t, unrecorded, claim.RoleImplementer, supervise.StateLaunching)), unrecorded,
			phaseUnrecorded, claimMoved(claim.RoleImplementer, "", supervise.StateLaunching))
		queues(t, s, wakes, "a later write of that claim",
			put(s, stateClaim(t, unrecorded, claim.RoleImplementer, supervise.StateRegistered)), unrecorded,
			claimMoved(claim.RoleImplementer, supervise.StateLaunching, supervise.StateRegistered))
		queues(t, s, wakes, "the admission of another role of that issue",
			admit(s, stateClaim(t, unrecorded, claim.RoleTester, supervise.StateLaunching)), unrecorded,
			claimMoved(claim.RoleTester, "", supervise.StateLaunching))
		queues(t, s, wakes, "the first admission on an issue another project records",
			admit(s, stateClaim(t, otherProjects, claim.RoleImplementer, supervise.StateLaunching)), otherProjects,
			phaseUnrecorded, claimMoved(claim.RoleImplementer, "", supervise.StateLaunching))
		queues(t, s, wakes, "the first admission on an issue the project records",
			admit(s, stateClaim(t, recorded, claim.RoleImplementer, supervise.StateLaunching)), recorded,
			claimMoved(claim.RoleImplementer, "", supervise.StateLaunching))
		queues(t, s, wakes, "the first PutClaim on an issue with no record",
			put(s, stateClaim(t, fixture, claim.RoleArchitect, supervise.StateIdle)), fixture,
			phaseUnrecorded, claimMoved(claim.RoleArchitect, "", supervise.StateIdle))
	})

	t.Run("a write that rolls back queues none", func(t *testing.T) {
		s := migratedStore(t)
		wakes := publishStates(t, s)
		recordIssue(t, s, issue, stateProject)
		openTree(t, s, issue)
		queues(t, s, wakes, "the admission", admit(s, tester(t, supervise.StateLaunching)), issue,
			claimMoved(claim.RoleTester, "", supervise.StateLaunching))
		counter := func() int64 {
			var last int64
			if err := s.pool.QueryRow(ctx, `select last_seq from state_publication where project = $1`, stateProject).Scan(&last); err != nil {
				t.Fatalf("read %s's state counter: %v", stateProject, err)
			}
			return last
		}
		before := counter()
		// A generation no bigint holds: the delivery's write is refused inside the transaction
		// that queued the failure's state row, so both roll back.
		refused := stateTask
		refused.Generation = 1 << 63
		queuesNothing := func(name string, write func() error, wantErr func(error) bool) {
			t.Helper()
			told := len(wakes.visible)
			if err := write(); err == nil || !wantErr(err) {
				t.Fatalf("%s answered %v, want it refused", name, err)
			}
			if changes := sentStates(t, s); len(changes) != 0 || len(wakes.visible) != told {
				t.Fatalf("%s rolled back and left the state changes %+v, with %d wakes told, want neither", name, changes, len(wakes.visible)-told)
			}
			if stored := onlyClaim(t, s); stored.State != supervise.StateLaunching {
				t.Fatalf("after %s rolled back the claim reads %s, want the launching row it was", name, stored.State)
			}
		}
		queuesNothing("a failure written with a delivery the store refuses", func() error {
			return s.PutClaimAndDelivery(ctx, tester(t, supervise.StateFailed), refused)
		}, func(error) bool { return true })
		queuesNothing("a retirement of a delivery the claim does not hold", retire(s, tester(t, supervise.StateRetired)),
			func(err error) bool { return errors.Is(err, supervise.ErrNoSuchDelivery) })
		if after := counter(); after != before {
			t.Fatalf("the rolled-back writes stepped %s's sequence from %d to %d, want no step", stateProject, before, after)
		}
	})

	t.Run("a store that has not turned the events on queues none", func(t *testing.T) {
		for _, holders := range []bool{false, true} {
			s := migratedStore(t)
			if holders {
				s.PublishRoleHolders(nil)
			}
			wakes := &stateWakes{}
			openTree(t, s, issue)
			name := fmt.Sprintf("with the role-holder publication on: %t, ", holders)
			for _, step := range []struct {
				name  string
				write func() error
			}{
				{"AdmitClaim", admit(s, tester(t, supervise.StateLaunching))},
				{"PutClaim", put(s, tester(t, supervise.StateRegistered))},
				{"PutClaimAndDelivery", confirm(s, tester(t, supervise.StateWorking))},
				{"RetireDelivery", retire(s, tester(t, supervise.StateIdle))},
			} {
				queues(t, s, wakes, name+step.name+" on an issue with no record", step.write, issue)
			}
		}
	})
}

// The take model's claim writes (dispatch://INFRA-2811) queue the state event of each move of the
// claim they make, in their own transaction, as every claim write does, and none where the claim's
// state stands. Each step writes the claim and its delivery as the claim's machine writes them,
// through the write it makes them with. The value's write (supervise's queue) and the accepted
// take (accept) write the claim at the state it stands at, ready, the take with the delivery taken
// (its number, ConfirmedAt and TakenText), so neither queues one. The start of the take's turn
// (turnStarted, startTakesTurn) writes the claim working with the delivery's TurnStarted, which
// queues the claim's move from ready. The turn-outcome report naming the number, ahead of its
// turn's end, finishes the task (finishTask) in a RetireDelivery of the claim still working, and
// queues none; the end of the turn writes the claim idle, which queues that move. A take given
// back before its turn started (giveBackTake) writes the delivery untaken over the claim at its
// state, and queues none. A take that reaches a claim a restart restored working with its task
// owed (takeInTurn) writes the claim idle in its own decision before the take is accepted over
// it: that write queues the claim's move from working, and the accept none.
func TestTheTakeModelsClaimWritesQueueTheStateEventOfEachMoveTheyMake(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const issue, session = "LEGION-306", "ses-implementer"
	s := migratedStore(t)
	wakes := publishStates(t, s)
	recordIssue(t, s, issue, stateProject)
	openTree(t, s, issue)
	implementer := func(state supervise.ClaimState, written, taken, finished uint64) supervise.Claim {
		c := stateClaim(t, issue, claim.RoleImplementer, state)
		c.Session = session
		c.TaskWritten, c.TaskTaken, c.TaskFinished = written, taken, finished
		if finished > 0 {
			c.TaskFinishedSession = session
		}
		return c
	}
	queued := func(number uint64) supervise.Delivery {
		return supervise.Delivery{ID: fmt.Sprintf("outbox:%d", number), Task: "implement it", Number: number, QueuedAt: at(1), Generation: 1}
	}
	taken := func(number uint64, started bool) supervise.Delivery {
		d := queued(number)
		d.ConfirmedAt, d.TakenText, d.TurnStarted = at(2), "implement it", started
		return d
	}
	put := func(c supervise.Claim) func() error { return func() error { return s.PutClaim(ctx, c) } }
	withTask := func(c supervise.Claim, d supervise.Delivery) func() error {
		return func() error { return s.PutClaimAndDelivery(ctx, c, d) }
	}
	finish := func(c supervise.Claim, id string) func() error {
		return func() error { return s.RetireDelivery(ctx, c, id) }
	}
	role := claim.RoleImplementer

	queues(t, s, wakes, "the admission", func() error { _, err := s.AdmitClaim(ctx, implementer(supervise.StateLaunching, 0, 0, 0)); return err },
		issue, claimMoved(role, "", supervise.StateLaunching))
	queues(t, s, wakes, "the ready", put(implementer(supervise.StateReady, 0, 0, 0)), issue,
		claimMoved(role, supervise.StateLaunching, supervise.StateReady))
	queues(t, s, wakes, "the write of task 1's value", withTask(implementer(supervise.StateReady, 1, 0, 0), queued(1)), issue)
	queues(t, s, wakes, "the accepted take of task 1", withTask(implementer(supervise.StateReady, 1, 1, 0), taken(1, false)), issue)
	queues(t, s, wakes, "the start of the take's turn", withTask(implementer(supervise.StateWorking, 1, 1, 0), taken(1, true)), issue,
		claimMoved(role, supervise.StateReady, supervise.StateWorking))
	queues(t, s, wakes, "the report that finishes task 1 ahead of its turn's end", finish(implementer(supervise.StateWorking, 1, 1, 1), "outbox:1"), issue)
	queues(t, s, wakes, "the end of the turn", put(implementer(supervise.StateIdle, 1, 1, 1)), issue,
		claimMoved(role, supervise.StateWorking, supervise.StateIdle))

	queues(t, s, wakes, "the write of task 2's value", withTask(implementer(supervise.StateIdle, 2, 1, 1), queued(2)), issue)
	queues(t, s, wakes, "the accepted take of task 2", withTask(implementer(supervise.StateIdle, 2, 2, 1), taken(2, false)), issue)
	queues(t, s, wakes, "the take-back of task 2 before its turn", withTask(implementer(supervise.StateIdle, 2, 2, 1), queued(2)), issue)
	// A compaction moves the claim working over the task owed and untaken, and a restart that cut
	// it off restores the claim working; the take is the worker's word that its host is idle.
	queues(t, s, wakes, "the compaction", put(implementer(supervise.StateWorking, 2, 2, 1)), issue,
		claimMoved(role, supervise.StateIdle, supervise.StateWorking))
	queues(t, s, wakes, "the take that reaches the claim restored working", put(implementer(supervise.StateIdle, 2, 2, 1)), issue,
		claimMoved(role, supervise.StateWorking, supervise.StateIdle))
	queues(t, s, wakes, "the accepted take over it", withTask(implementer(supervise.StateIdle, 2, 2, 1), taken(2, false)), issue)
}
