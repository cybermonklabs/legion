package record

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A phase worker's start that another role's start takes out (Store.EnqueueStart) may be the one
// start of a claim the project release stopped: the daemon's boot writes none for a phase owner a
// queued start already covers (workflow's startReleaseStopped), and the outbox's phase fence would
// start that claim with no task rather than finish the row (the daemon's releaseStoppedStart). So
// the statement that takes the row out leaves that role the start the fence makes, due at once.
// It leaves none where no such start is owed: for a row of the caller's own role, whose later
// start starts the claim itself; for a row whose role's claim the daemon suspended, which the
// phase fence finishes; and for a row of an earlier generation, which the outbox's generation
// fence finishes before its phase fence reads it.
func TestEnqueueStartLeavesTheReleaseStoppedRoleOfTheRowItTakesOutAStart(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Now().UTC().Truncate(time.Microsecond)
	for _, tc := range []struct {
		name string
		key  string
		// role, stands and generation are the role, phase and generation of the start the issue
		// owes before the call, a start the release held, and by is who suspended that role's
		// claim.
		role       claim.Role
		stands     phase.Phase
		generation uint64
		by         supervise.SuspendSource
		// started is whether the call leaves that role a start of its own.
		started bool
		why     string
	}{
		{name: "a tester's start an implementer's takes out", key: "LEGION-301", role: claim.RoleTester, stands: phase.Testing,
			generation: 3, by: supervise.SuspendedByProjectRelease, started: true,
			why: "the tester's start was its claim's one start, which the outbox's phase fence would have run as a start with no task"},
		{name: "an implementer's start a later one of its own takes out", key: "LEGION-302", role: claim.RoleImplementer,
			stands: phase.Implementing, generation: 3, by: supervise.SuspendedByProjectRelease,
			why: "the later start of the same role starts that claim itself"},
		{name: "a tester's start whose claim the daemon suspended", key: "LEGION-303", role: claim.RoleTester, stands: phase.Testing,
			generation: 3, by: supervise.SuspendedByDaemon,
			why: "the outbox's phase fence finishes the start of a claim no project release stopped"},
		{name: "a tester's start of an earlier generation", key: "LEGION-304", role: claim.RoleTester, stands: phase.Testing,
			generation: 2, by: supervise.SuspendedByProjectRelease,
			why: "the outbox's generation fence finishes a start of an earlier generation without acting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The issue stands in implementing at generation 3 (issueFixture), where a head that
			// started conflicting moved it, and this is the implementer's start of that move.
			later := SuperviseRequest{Op: "start", Tree: tc.key, Role: claim.RoleImplementer, Generation: 3, Phase: phase.Implementing,
				Task: "Merge main into the branch and resolve the conflict.", Entry: 2, NeedsSlot: true}
			inTx(t, st, func(tx pgx.Tx) {
				must(t, records.PutIssue(ctx, tx, issueFixture(tc.key)))
				putSuspendedClaim(t, tx, tc.key, tc.role, tc.by)
				held, err := NewOutboxRow(tc.key, SuperviseRequest{Op: "start", Tree: tc.key, Role: tc.role, Generation: tc.generation,
					Phase: tc.stands, Task: "Carry the phase on.", Entry: 1, NeedsSlot: true}, at)
				must(t, err)
				must(t, records.Enqueue(ctx, tx, held))
				row, err := NewOutboxRow(tc.key, later, at)
				must(t, err)
				must(t, records.EnqueueStart(ctx, tx, row))

				var others []startRow
				for _, start := range startRows(t, tx, tc.key) {
					if !reflect.DeepEqual(start.request, later) {
						others = append(others, start)
					}
				}
				if !tc.started {
					if len(others) != 0 {
						t.Fatalf("the starts of %s beside the implementer's = %+v, want none: %s", tc.key, others, tc.why)
					}
					return
				}
				want := ReleaseStoppedStart(tc.key, tc.role, 3)
				if len(others) != 1 || !reflect.DeepEqual(others[0].request, want) {
					t.Fatalf("the starts of %s beside the implementer's = %+v, want the one start %+v, resumed by rule and naming no phase, task, entry or slot mark: %s",
						tc.key, others, want, tc.why)
				}
				due, err := records.DueRows(ctx, tx, "LEGION", tc.key, []OutboxKind{OutboxKindSupervise}, at)
				must(t, err)
				if len(due) != 1 || due[0].ID != others[0].id {
					t.Fatalf("the starts of %s a lane may run at %s = %+v, want the %s's start alone: it waits for no slot, while the implementer's waits for the issue's",
						tc.key, at, due, tc.role)
				}
			})
		})
	}
}

// startRow is one supervise row of an issue: its id and its payload, strictly decoded.
type startRow struct {
	id      int64
	request SuperviseRequest
}

// startRows is every supervise row of issue, oldest first by id, each payload decoded as the
// outbox decodes it (DecodeOutboxPayload), so a row this reads is one a lane can run.
func startRows(t *testing.T, tx pgx.Tx, issue string) []startRow {
	t.Helper()
	rows, err := tx.Query(context.Background(), "select "+outboxColumns+" from outbox where issue = $1 and kind = $2 order by id",
		issue, string(OutboxKindSupervise))
	if err != nil {
		t.Fatalf("list the supervise rows of %s: %v", issue, err)
	}
	defer rows.Close()
	var starts []startRow
	for rows.Next() {
		row, err := scanOutbox(rows)
		if err != nil {
			t.Fatalf("read a supervise row of %s: %v", issue, err)
		}
		payload, err := DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode supervise row %d of %s: %v", row.ID, issue, err)
		}
		request, ok := payload.(SuperviseRequest)
		if !ok {
			t.Fatalf("supervise row %d of %s decodes to %T", row.ID, issue, payload)
		}
		starts = append(starts, startRow{id: row.ID, request: request})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the supervise rows of %s: %v", issue, err)
	}
	return starts
}

// putSuspendedClaim records role's claim on key, the root of its own tree, as suspended by by.
func putSuspendedClaim(t *testing.T, tx pgx.Tx, key string, role claim.Role, by supervise.SuspendSource) {
	t.Helper()
	project, err := claim.ProjectToken("legion")
	must(t, err)
	token, err := claim.NewToken(project, key, role)
	must(t, err)
	if _, err := tx.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
		values ($1, $2, $3, $3, $4, 1, 'ses', '/sessions/ses.jsonl', $5, 0, 0, 0, 0, $6)`,
		string(token), project, key, string(role), string(supervise.StateSuspended), string(by)); err != nil {
		t.Fatalf("record the %s's claim on %s suspended by %s: %v", role, key, by, err)
	}
}
