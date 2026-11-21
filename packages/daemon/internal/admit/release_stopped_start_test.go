package admit

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// releaseFenceRow is one row of the repair of a held tester start whose issue has left testing.
// Every row is the same fixture — a member standing in held, where a hold moved it after its
// tester's start was written, owing that start and holding no slot, with the tester's claim
// suspended — and each row changes the one thing it is about, which why names.
type releaseFenceRow struct {
	name string
	// root and key are the tree's root and the member owed the start.
	root, key string
	// generation is the member's; the start it owes names generation 1.
	generation uint64
	// lingers records the root as a park left it (seedTreeAndMember).
	lingers bool
	// status is the member's Dispatch status, "" for the in_progress seedTreeAndMember writes.
	status string
	// by is who suspended the tester's claim.
	by supervise.SuspendSource
	// started is whether the pass leaves the tester a start in place of the one it ends.
	started bool
	why     string
}

// seedReleaseFenceRow records row's fixture: its tree, the member, the tester's claim and the
// start of testing the member owes.
func seedReleaseFenceRow(t *testing.T, pool *pgxpool.Pool, row releaseFenceRow) {
	t.Helper()
	seedTreeAndMember(t, pool, row.root, row.key, phase.Held, row.generation, row.lingers)
	if row.status != "" {
		member := issue(t, pool, row.key)
		member.Status, member.DispatchStatus = row.status, row.status
		putIssue(t, pool, member)
	}
	putWorkerClaim(t, pool, row.root, row.key, claim.RoleTester, 1, supervise.StateSuspended, row.by)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := workflow.StartOrOwe(context.Background(), tx, record.NewStore(), row.key, record.SuperviseRequest{Op: "start",
			Tree: row.root, Role: claim.RoleTester, Generation: 1, Phase: phase.Testing, Task: "Test the change at its head.",
			Entry: seededEntry}, fixedNow); err != nil {
			t.Fatalf("owe %s its tester's start through the workflow's own writer: %v", row.key, err)
		}
	})
	if owed := owedStarts(t, pool, row.key); len(owed) != 1 {
		t.Fatalf("the starts %s owes before the pass = %+v, want the one its fixture wrote: the row's own premise", row.key, owed)
	}
}

// startRequests is every supervise start row of issue, oldest first, each payload decoded as the
// outbox decodes it (record.DecodeOutboxPayload), so a row this reads is one a lane can run.
func startRequests(t *testing.T, pool *pgxpool.Pool, issue string) []record.SuperviseRequest {
	t.Helper()
	var starts []record.SuperviseRequest
	inTx(t, pool, func(tx pgx.Tx) {
		rows, err := tx.Query(context.Background(), `select payload from outbox
			where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, issue)
		if err != nil {
			t.Fatalf("list the starts of %s: %v", issue, err)
		}
		payloads, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			t.Fatalf("read the starts of %s: %v", issue, err)
		}
		for _, payload := range payloads {
			decoded, err := record.DecodeOutboxPayload(record.OutboxRow{Kind: record.OutboxKindSupervise, Issue: issue, Payload: payload})
			if err != nil {
				t.Fatalf("decode a start of %s: %v", issue, err)
			}
			request, ok := decoded.(record.SuperviseRequest)
			if !ok {
				t.Fatalf("a start of %s decodes to %T", issue, decoded)
			}
			starts = append(starts, request)
		}
	})
	return starts
}

// A start the project release held for a stopped claim can outlive its phase with no later start
// taking it out: a hold moves a tester's issue into held before a lane runs the tester's start,
// which then waits for a slot it is never given. Admission's repair ends such a row
// (finishFencedStarts), and the row may be the stopped tester's one start, since the daemon's boot
// writes none for a phase owner a queued start already covers (workflow's startReleaseStopped). So
// where the outbox's phase fence would have started that claim — the issue in the workflow, its
// tree running, at the row's generation, and the claim stored suspended by the project release —
// the repair writes that start in the row's place: resumed by rule and naming no phase, task,
// entry or slot mark, so a lane runs it at once. Every other row it ends alone, as it did: one
// whose claim the daemon suspended, one whose root lingers, one whose issue left the workflow and
// one of an earlier generation.
func TestAHeldStartWhosePhaseTheIssueLeftStillStartsTheClaimTheProjectReleaseStopped(t *testing.T) {
	rows := []releaseFenceRow{
		{name: "a tester the project release stopped", root: "LEGION-910", key: "LEGION-911", generation: 1,
			by: supervise.SuspendedByProjectRelease, started: true,
			why: "the held start was the stopped tester's one start, which the outbox's phase fence would have run as a start with no task"},
		{name: "a tester the daemon suspended", root: "LEGION-920", key: "LEGION-921", generation: 1,
			by:  supervise.SuspendedByDaemon,
			why: "this row changes the claim's source alone: the outbox's phase fence finishes the start of a claim no project release stopped"},
		{name: "a tester of a tree that lingers", root: "LEGION-930", key: "LEGION-931", generation: 1, lingers: true,
			by:  supervise.SuspendedByProjectRelease,
			why: "this row changes the root's own record alone, to the one a park left: the boot leaves a lingering tree's claims to its re-admission, which starts every role of a mid-phase member (startMidPhaseMembers)"},
		{name: "a tester of an issue that left the workflow", root: "LEGION-940", key: "LEGION-941", generation: 1, status: "backlog",
			by:  supervise.SuspendedByProjectRelease,
			why: "this row changes the member's status alone: the boot leaves the claims of an issue out of the workflow to its close, and the outbox starts nothing for such an issue"},
		{name: "a tester's start of an earlier generation", root: "LEGION-950", key: "LEGION-951", generation: 2,
			by:  supervise.SuspendedByProjectRelease,
			why: "this row changes the member's generation alone: the outbox's generation fence finishes a start of an earlier generation without acting, and that run's claim is its re-admission's to start"},
	}
	pool := migratedPool(t)
	// One slot, taken, so the pass promotes none of these issues.
	capFiller(t, pool, "LEGION-900", "LEGION-901")
	for _, row := range rows {
		seedReleaseFenceRow(t, pool, row)
	}
	tick(t, pool, quiet(t, 1), "tick")

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if owed := owedStarts(t, pool, row.key); len(owed) != 0 {
				t.Fatalf("the starts %s owes after the pass = %+v, want none: the repair ends a held start whose phase the issue has left", row.key, owed)
			}
			starts := startRequests(t, pool, row.key)
			if !row.started {
				if len(starts) != 0 {
					t.Fatalf("the starts of %s after the pass = %+v, want none: %s", row.key, starts, row.why)
				}
				return
			}
			want := record.ReleaseStoppedStart(row.root, claim.RoleTester, 1)
			if len(starts) != 1 || !reflect.DeepEqual(starts[0], want) {
				t.Fatalf("the starts of %s after the pass = %+v, want the one start %+v, resumed by rule and naming no phase, task, entry or slot mark: %s",
					row.key, starts, want, row.why)
			}
			if got := startsOf(t, pool, row.key); len(got) != 1 || !got[0].claimable {
				t.Fatalf("the starts of %s after the pass = %+v, want the tester's start claimable now: it waits for no slot", row.key, got)
			}
		})
	}
}
