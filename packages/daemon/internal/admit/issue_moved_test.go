package admit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// movedAtVersion is the version of the migration that adds the column, so the last part of the
// test can run that migration's own SQL whatever lands beside it.
const movedAtVersion = 20261111010031

// movedIssue is the issue the movements below are recorded against: a root running a tree of its
// own, in testing, with its admission slot and its tree lifecycle, so that every fact the test
// applies is one the workflow acts on rather than one a linger or a missing tree stops.
const movedIssue = "LEGION-208"

// The record keeps when the daemon last saw an issue move (record.Issue.MovedAt), which is what a
// measure of a tree that has stopped is read from: each of the facts that means the issue's own
// record changed leaves the mark at that fact's own moment, and the daemon's own bookkeeping
// leaves it where it stands.
//
// This test is in internal/admit because it is the one package that can show both halves. The six
// write sites are in internal/workflow and the boot's read of the Dispatch listing is
// Admission.Reconcile's, and admit imports workflow, so an Engine and an Admission stand side by
// side here exactly as the daemon runs them. internal/store, whose own migrateThrough helper would
// have served the last part, cannot: admit's own dependencies reach it, so a test in package store
// could not drive the listing at all. The last part therefore takes the column off a migrated pool
// and runs the change's own migration over the rows written without it, which is the same
// before-and-after the helper builds.
func TestTheRecordKeepsWhenAnIssueLastMoved(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	// The engine's clock is moved between facts, so each fact's own moment is a value no other
	// fact could have written. It is set below to run from just after the moment the record was
	// written, because a mark is only ever written forwards: a clock behind the row's own stamp
	// would prove nothing, since every write would be the monotone no-op the rule makes it.
	//
	// Admission keeps the package's fixedNow, which is before every mark here, so the starts
	// admission makes of its own — the repair of a slotted worker with no task, among them — are
	// exactly those monotone no-ops, and no assertion below is answered by one of them.
	now := fixedNow
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Clock: func() time.Time { return now }}, nil)
	admission := New(record.NewStore(), engine, 1, testProject, nil)
	admission.now = func() time.Time { return fixedNow }

	seedSlotted(t, pool, movedIssue, "U")
	// The root stands in testing: its phase's status is what Dispatch would show, and the record
	// holds in_progress as the status Dispatch last showed, the phase before's, so the first fact
	// below is an event the engine takes and keeps its own status over.
	stands := issue(t, pool, movedIssue)
	stands.Phase, stands.Status, stands.DispatchStatus, stands.LastDispatchSeq = phase.Testing, "testing", "in_progress", 1
	putIssue(t, pool, stands)
	recorded := movedAt(t, pool, movedIssue)
	if recorded.IsZero() {
		t.Fatalf("the record of %s carries no movement before any fact: the column is not null and every row is stamped when it is written", movedIssue)
	}

	base := recorded.Add(time.Minute)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	assertMoved := func(what string, want time.Time) {
		t.Helper()
		if got := movedAt(t, pool, movedIssue); !got.Equal(want) {
			t.Fatalf("after %s the record says %s last moved at %s, want %s, the moment of that fact",
				what, movedIssue, got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
		}
	}

	// A Dispatch event of the issue. Dispatch shows in_progress while the record stands in
	// testing, so the engine keeps its own status over the event (Engine.keepStatus) and records
	// the observation: the issue moved, whatever the engine then does with the event.
	now = at(1)
	apply(t, pool, admission, "dispatch-event", intake.DispatchIssue{Key: movedIssue, Seq: 2, Type: "issue.updated",
		Status: "in_progress", Title: "a person renamed it", Rank: "U", HandedOver: true, ResolvedRepo: "acme/widgets"}, engine)
	assertMoved("a Dispatch event of the issue", at(1))

	// A GitHub event of its pull request: the pull request its branch opened, recorded through
	// the one helper every pull request write in the workflow goes through.
	now = at(2)
	applyMovement(t, pool, admission, engine, "github", "pull-request-opened", intake.PullRequestOpened{
		Repo: "acme/widgets", Number: 7, Branch: claim.IssueBranch(movedIssue), HeadSHA: "a1", UpdatedAt: at(2)})
	assertMoved("a GitHub event of its pull request", at(2))

	// A recorded handoff: the tester records its phase's handoff and reports no verdict, so the
	// completion writes the phase row and moves the issue no further. It comes before the phase
	// change below so that this one write is the only thing that could have marked the movement.
	now = at(3)
	applyMovement(t, pool, admission, engine, "api", "tester-handoff", intake.HandoffComplete{Issue: movedIssue,
		Role: claim.RoleTester, Generation: 1, HandoffRecord: "handoff-test"})
	if got := issue(t, pool, movedIssue).Phase; got != phase.Testing {
		t.Fatalf("%s stands in %s after the tester's handoff, want testing still: the fact must record a handoff and move nothing else", movedIssue, got)
	}
	assertMoved("a recorded handoff", at(3))

	// A phase change: the tester sends the work back to implementing, which writes the issue's
	// phase and starts the implementer there.
	now = at(4)
	applyMovement(t, pool, admission, engine, "api", "backward-move", intake.BackwardMove{Issue: movedIssue,
		Requester: claim.RoleTester, To: phase.Implementing, Reason: "the tests need another round"})
	if got := issue(t, pool, movedIssue).Phase; got != phase.Implementing {
		t.Fatalf("%s stands in %s after the backward move, want implementing: the fact must be the phase change it is read as", movedIssue, got)
	}
	assertMoved("a phase change", at(4))

	// A start: the one admission makes for a member already standing mid-phase, written through
	// workflow.StartOrOwe, which is where every start of a phase worker is written and where the
	// movement is marked. It is handed the moment, not a clock of its own, which the engine's
	// clock standing far past it here proves.
	now = at(99)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := workflow.StartOrOwe(ctx, tx, record.NewStore(), movedIssue, record.SuperviseRequest{Op: "start",
			Tree: movedIssue, Role: claim.RoleImplementer, Task: "Carry the work on.", Generation: 1,
			Phase: phase.Implementing, ResumeTask: true}, at(5)); err != nil {
			t.Fatalf("start the implementer of %s again: %v", movedIssue, err)
		}
	})
	assertMoved("a start", at(5))

	// A completion: the implementer completes implementing, and the open pull request takes the
	// issue on to testing.
	now = at(6)
	applyMovement(t, pool, admission, engine, "api", "implementer-handoff", intake.HandoffComplete{Issue: movedIssue,
		Role: claim.RoleImplementer, Generation: 1, Commit: "c1", HandoffRecord: "handoff-implement", Summary: "written"})
	if got := issue(t, pool, movedIssue).Phase; got != phase.Testing {
		t.Fatalf("%s stands in %s after the implementer's completion, want testing: the completion must be the one that ends a phase", movedIssue, got)
	}
	assertMoved("a completion", at(6))

	// The boot's read of the Dispatch listing is the daemon catching up with what it already
	// knows, not news of the issue: the listing shows the status the record holds, so the read
	// records the listing's title and rank (Admission.applySummary) and moves the mark nowhere.
	// The clock is well past the last fact, so a mark written here could not be mistaken for the
	// one that stands.
	now = at(20)
	reconcile(t, pool, admission, []dispatch.IssueSummary{{Key: movedIssue, Title: "renamed in the listing",
		Status: "testing", Rank: "U", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 2}})
	if got := issue(t, pool, movedIssue).Title; got != "renamed in the listing" {
		t.Fatalf("the boot listing left %s titled %q, want the listing's own title: the read must be one that records something", movedIssue, got)
	}
	assertMoved("the boot's read of the Dispatch listing", at(6))

	// A Dispatch event the record has already applied — a redelivery at a sequence the record has
	// taken — stops at the engine's own fence and moves nothing.
	now = at(21)
	apply(t, pool, admission, "dispatch-redelivery", intake.DispatchIssue{Key: movedIssue, Seq: 2, Type: "issue.updated",
		Status: "in_progress", Title: "renamed in the listing", Rank: "U", HandedOver: true, ResolvedRepo: "acme/widgets"}, engine)
	assertMoved("a Dispatch event the record had already applied", at(6))

	// A claim's session activity report says a session is up, which is no proof that the issue it
	// holds has moved: it writes the claims table and never the issue.
	now = at(22)
	seedArchitectClaim(t, pool, movedIssue, true)
	records, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer records.Close()
	if known, err := records.PutClaimActivity(ctx, awaitingArchitect(t, movedIssue),
		claim.SessionActivity{At: time.Now().UTC(), BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"subagent"}}}); err != nil || !known {
		t.Fatalf("report the activity of the architect claim of %s = %t, %v", movedIssue, known, err)
	}
	assertMoved("a claim's session activity report", at(6))

	t.Run("the migration stamps every issue written before it", func(t *testing.T) {
		pool := migratedPool(t)
		// The table as it stood before the change, so the rows below are written as the release
		// before this one wrote them: PutIssue names the column nowhere, so it writes them
		// unchanged against a table that has not got it.
		if _, err := pool.Exec(ctx, "alter table issues drop column moved_at"); err != nil {
			t.Fatalf("take the column off the migrated pool: %v", err)
		}
		for _, key := range []string{"LEGION-301", "LEGION-302"} {
			putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: testProject, Title: key, Tree: key, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
		}

		// The change's own migration, run in one transaction as store's apply runs it, so now()
		// is that transaction's own moment and the whole backfill is that one value.
		var minted time.Time
		inTx(t, pool, func(tx pgx.Tx) {
			if err := tx.QueryRow(ctx, "select now()").Scan(&minted); err != nil {
				t.Fatalf("read the migration transaction's own time: %v", err)
			}
			if _, err := tx.Exec(ctx, movedAtMigration(t)); err != nil {
				t.Fatalf("run the migration that adds the column: %v", err)
			}
		})

		for _, key := range []string{"LEGION-301", "LEGION-302"} {
			if got := movedAt(t, pool, key); !got.Equal(minted.UTC()) {
				t.Errorf("%s, written before the change, says it last moved at %s, want %s, the migration's own time: an issue the deploy finds must read as moving then, not earlier, or its tree reads as quiet for longer than the measure has run",
					key, got.Format(time.RFC3339Nano), minted.UTC().Format(time.RFC3339Nano))
			}
		}
	})
}

// movedAt is when the record says key last moved, read through the record's own scan.
func movedAt(t *testing.T, pool *pgxpool.Pool, key string) time.Time {
	t.Helper()
	return issue(t, pool, key).MovedAt
}

// movedAtMigration is the SQL of the migration that adds the column, read from the embedded set
// rather than from a path, so the test runs what the daemon would run.
func movedAtMigration(t *testing.T) string {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if migration.Version == movedAtVersion {
			return migration.SQL
		}
	}
	t.Fatalf("the embedded migrations hold no version %d, the change that adds moved_at", movedAtVersion)
	return ""
}

// applyMovement applies a fact of any source through the engine and the admission, as the daemon
// applies one, and fails on a refusal: a fact the workflow refused changed nothing, and would
// prove nothing about a movement. The package's own applyFact takes the Dispatch and gate
// sources alone.
func applyMovement(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler, source, eventID string, fact intake.Fact) {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), pool, source, eventID, fact, engine, admission)
	if err != nil {
		t.Fatalf("ApplyFact %s: %v", eventID, err)
	}
	if result.Refusal != nil {
		t.Fatalf("ApplyFact %s was refused %s: %s", eventID, result.Refusal.Code, result.Refusal.Message)
	}
}
