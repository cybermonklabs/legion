package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// architectStalledWakes is every architect-stalled controller notice of issue the outbox holds,
// oldest first, as the reason each carries (record.ArchitectStalledNotice, admit's
// wakeStalledArchitect). A proof reads it to say that a root the daemon served raises no wake of
// any arm: each arm is a reading that the architect did not take its work, is not there, or has
// not moved, and none of the three is true of a root whose architect holds the task it was sent.
func architectStalledWakes(t *testing.T, pool *store.Pool, issue string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select coalesce(payload->>'reason', '') from outbox
		where issue = $1 and kind = $2 and payload->>'kind' = $3 order by id`,
		issue, string(record.OutboxKindControllerNotice), string(record.ArchitectStalledNotice))
	if err != nil {
		t.Fatalf("list the architect wakes of %s: %v", issue, err)
	}
	defer rows.Close()
	var reasons []string
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			t.Fatalf("read an architect wake of %s: %v", issue, err)
		}
		reasons = append(reasons, reason)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the architect wakes of %s: %v", issue, err)
	}
	return reasons
}

// dropArchitectStarts deletes every architect start row of the rig's root the outbox still holds,
// which is the start lost before any lane ran it: the shape the measured defect leaves behind, a
// root holding its slot with its architect's entry raised and nothing on its way to the claim
// (dispatch://INFRA-2614).
func dropArchitectStarts(t *testing.T, r *mainPathRig) {
	t.Helper()
	tag, err := r.pool.Exec(r.ctx, `delete from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = 'architect'`, r.key)
	if err != nil {
		t.Fatalf("drop the architect start rows of %s: %v", r.key, err)
	}
	if tag.RowsAffected() == 0 {
		t.Fatalf("no architect start row of %s to drop: the premise of this proof is a start the lane never ran", r.key)
	}
}

// architectStartCount is how many architect start rows issue holds, of any entry.
func architectStartCount(t *testing.T, pool *store.Pool, issue string) int {
	t.Helper()
	var held int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where issue = $1
		and kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'architect'`, issue).Scan(&held); err != nil {
		t.Fatalf("count the architect start rows of %s: %v", issue, err)
	}
	return held
}

// leftAloneClaim is one claim the rule must not send anything to, recorded beside the rig's root:
// its issue, the tree that issue stands in, the phase it stands in, the state of its claim and
// who suspended it. Each holds one of the cap's slots and an architect phases row at entry 1 with
// its claim queued no entry, which is the very difference the pass closes for the root beside
// them, so a claim left alone here is left alone for the reason its row names.
type leftAloneClaim struct {
	issue       string
	tree        string
	stands      phase.Phase
	role        claim.Role
	state       supervise.ClaimState
	suspendedBy supervise.SuspendSource
	why         string
}

// seedLeftAlone records one such claim: the issue, its slot, the role's phases row at entry 1 and
// the claim itself, queued no entry.
func seedLeftAlone(t *testing.T, r *mainPathRig, index int, row leftAloneClaim) claim.Token {
	t.Helper()
	issue := record.Issue{Key: row.issue, Tree: row.tree, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: mainPathProject, Title: row.issue, Phase: row.stands, Generation: 1,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: row.issue, HandedOver: true, LastDispatchSeq: 1}
	if row.tree != row.issue {
		parent := row.tree
		issue.Parent = &parent
	}
	r.put(issue)
	token := mustClaimToken(t, row.issue, row.role)
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		if err := r.records.PutSlot(r.ctx, tx, record.Slot{Issue: row.issue, Index: index, AdmittedAt: time.Now().UTC()}); err != nil {
			return err
		}
		return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: row.issue, Role: row.role, Claim: token,
			Entry: 1, EntryTask: "Carry " + row.issue + " on.", EntryRaisedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("seed the slot and the phases row of %s: %v", row.issue, err)
	}
	if err := r.trees.PutClaim(r.ctx, supervise.Claim{Token: token, Project: "legion", Tree: row.tree, Issue: row.issue,
		Role: row.role, Generation: 1, State: row.state, SuspendedBy: row.suspendedBy}); err != nil {
		t.Fatalf("record the %s claim of %s: %v", row.role, row.issue, err)
	}
	return token
}

// A root whose architect's claim was queued no entry is sent the work of the entry its phases row
// names, and that task reaches the claim. The root here is the one the measured defect leaves: it
// was admitted, its architect's entry was raised with that entry's work written on the row, and
// the start row that would have carried it to the claim was lost before any lane ran it
// (dispatch://INFRA-2614). Its architect is up and holds nothing.
//
// The whole path main runs is held here: the admission pass reads the claim's given entry below
// the entry its row names and writes the start (admit's owedEntryRoles and resumeSlottedWorkers),
// the outbox runner runs that row, and the agent is prompted that task. One entry reaches the
// claim once: four more passes write no second row, because a task the claim has not confirmed is
// a start already on its way (workflow.ArchitectStartPending).
//
// Three claims stand beside it, each holding a slot and each one the rule must leave alone: a
// child's own sub-architect, which is no tree root; an architect claim an operator suspended; and
// a phase worker of the root's own tree whose role no entry of its own is owed. None of them is
// sent anything, and each is read from the same pass that served the root.
func TestAQueuedRootWhoseArchitectWasQueuedNoEntryIsSentThatEntrysTask(t *testing.T) {
	r := newMainPathRig(t, true)
	r.put(r.waitingRoot())
	r.admitPass()
	dropArchitectStarts(t, r)
	r.launchArchitect()
	r.recordClaim()
	if given := r.machine().Claim().GivenEntry; given != 0 {
		t.Fatalf("the architect's claim was given entry %d before the pass, want 0: this proof is of a claim queued no entry", given)
	}
	if row := rolePhaseRow(t, r.pool, mainPathRoot, claim.RoleArchitect); row.Entry != 1 || row.EntryTask == "" {
		t.Fatalf("the architect's phases row = entry %d carrying %q, want entry 1 carrying that entry's work", row.Entry, row.EntryTask)
	}
	aside := []leftAloneClaim{
		{issue: "LEGION-452", tree: mainPathRoot, stands: phase.Admitted, role: claim.RoleArchitect,
			state: supervise.StateIdle, why: "a child's own sub-architect claim is outside the rule, since no daemon start creates one"},
		{issue: "LEGION-453", tree: "LEGION-453", stands: phase.Admitted, role: claim.RoleArchitect,
			state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByOperator,
			why: "`legion claims stop` stopped that architect and only the operator's own resume starts it again"},
		{issue: "LEGION-454", tree: mainPathRoot, stands: phase.Implementing, role: claim.RoleImplementer,
			state: supervise.StateRetired, why: "a retired claim was stopped, and a start here would hand the operator back the worker they stopped"},
	}
	left := map[string]claim.Token{}
	for index, row := range aside {
		left[row.issue] = seedLeftAlone(t, r, index+1, row)
	}
	before := len(r.prompts())

	r.admitPass()
	if held := architectStartCount(t, r.pool, mainPathRoot); held != 1 {
		t.Fatalf("the pass wrote %d architect start rows of %s, want the one carrying the work of its entry", held, mainPathRoot)
	}
	r.runStarts()

	pending := r.machine().Claim().Pending
	if pending == nil || pending.Task == "" {
		t.Fatalf("the architect's pending task after the pass = %#v, want the work of the entry its row names", pending)
	}
	if pending.Entry != 1 {
		t.Fatalf("the architect holds a task of entry %d, want the 1 its phases row names: the start carries the entry it serves", pending.Entry)
	}
	if given := r.machine().Claim().GivenEntry; given != 1 {
		t.Fatalf("the architect's claim was given entry %d, want 1: the delivery records the entry it carried", given)
	}
	prompts := r.prompts()
	if len(prompts) != before+1 || prompts[len(prompts)-1].Message != pending.Task {
		t.Fatalf("the architect was prompted %#v, want the one task of its entry", prompts[before:])
	}
	// The lane that ran that row finished it, which deletes it, so a row standing in the outbox
	// now is a row a later pass wrote.
	if held := architectStartCount(t, r.pool, mainPathRoot); held != 0 {
		t.Fatalf("%s holds %d architect start rows after its own was run, want none", mainPathRoot, held)
	}

	for range 4 {
		r.admitPass()
	}

	if held := architectStartCount(t, r.pool, mainPathRoot); held != 0 {
		t.Fatalf("four more passes wrote %d architect start rows of %s, want none: a task the claim has not confirmed is a start "+
			"already on its way, and a pass runs on every fact of the project", held, mainPathRoot)
	}
	if given := r.machine().Claim().GivenEntry; given != 1 {
		t.Fatalf("the architect's claim was given entry %d after four more passes, want the 1 it was given once", given)
	}
	if got := len(r.prompts()); got != before+1 {
		t.Fatalf("the architect was prompted %d times, want the one prompt of its entry: one entry reaches a claim once", got-before)
	}
	if reasons := architectStalledWakes(t, r.pool, mainPathRoot); len(reasons) != 0 {
		t.Fatalf("the architect wakes of %s = %q, want none: its architect holds the task of its entry", mainPathRoot, reasons)
	}

	for _, row := range aside {
		if starts := entryStartRows(t, r.pool, row.issue); len(starts) != 0 {
			t.Fatalf("the start rows written for %s = %+v, want none: %s", row.issue, starts, row.why)
		}
		var given int64
		if err := r.pool.QueryRow(r.ctx, `select given_entry from claims where token = $1`,
			string(left[row.issue])).Scan(&given); err != nil {
			t.Fatalf("read the claim of %s: %v", row.issue, err)
		}
		if given != 0 {
			t.Fatalf("the claim of %s was given entry %d, want 0: %s", row.issue, given, row.why)
		}
	}
}
