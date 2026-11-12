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
// This test is in internal/admit because it is the one package that can show every half. Nine
// of the eleven write sites are in internal/workflow — the resume of an answered root among
// them (workflow's ResumeAwaitedRoot), which only an admission pass reaches — and two are
// admission's own, the re-admission and the promotion of a root the line was holding; the
// boot's read of the Dispatch listing, which marks nothing of its own, is
// Admission.Reconcile's; admit imports workflow, so an Engine and an Admission stand side by
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

	// A Dispatch event the record has already applied — a redelivery at a sequence the record has
	// taken — stops at the engine's own fence and moves nothing.
	now = at(21)
	apply(t, pool, admission, "dispatch-redelivery", intake.DispatchIssue{Key: movedIssue, Seq: 2, Type: "issue.updated",
		Status: "in_progress", Title: "a person renamed it", Rank: "U", HandedOver: true, ResolvedRepo: "acme/widgets"}, engine)
	assertMoved("a Dispatch event the record had already applied", at(6))

	// The claim whose activity is reported below, and the store it is reported through: a claim's
	// session activity report says a session is up, which is no proof that the issue it holds has
	// moved, since it writes the claims table and never the issue. The report itself is made
	// further down, carrying a time of its own that stands ahead of the record's, which is the
	// only shape in which it could fail.
	seedArchitectClaim(t, pool, movedIssue, true)
	records, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer records.Close()

	// The two exclusions that are no fact of the workflow at all: the boot's read of the Dispatch
	// listing, which records the listing's title and rank (Admission.applySummary), and a claim's
	// activity report. Each is read against a clock standing ahead of every fact — the pass's own
	// for the listing, which is where production runs it, the daemon's clock being the wall clock
	// and the facts it applies older than that, and its own carried time for the report — because
	// that is the only placement in which either reading can fail. With the pass's clock behind
	// the facts, as the rest of this test runs it, admission keeps this package's fixedNow, every
	// time asserted here is the record's own stamp plus minutes, and MarkIssueMoved keeps the
	// later of the two, so a mark the listing's own path (admit's recordObservation) wrote at
	// either clock would be dropped as older and the assertion would pass though that path marked.
	//
	// Measured, with the pass's clock ahead: a mark at that clock is newer than the record's time
	// and the assertion sees it — making recordObservation mark at a.now() fails this test at the
	// listing's assertion below. A mark at the wall clock is not seen, and no fixture here can
	// show one: moved_at is stamped by the column's own default now(), every time this test
	// asserts is that stamp plus minutes, and the monotone rule drops a mark older than the one
	// that stands — making recordObservation mark at time.Now().UTC() leaves this test green.
	// That is the same limit the activity report below carries, and it is the limit of this
	// fixture, not of the rule: a daemon whose clock is the wall clock writes marks of the first
	// kind alone.
	ahead := at(30)
	admission.now = func() time.Time { return ahead }
	now = at(23)
	reconcile(t, pool, admission, []dispatch.IssueSummary{{Key: movedIssue, Title: "renamed in the listing",
		Status: "testing", Rank: "U", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 2}})
	if got := issue(t, pool, movedIssue).Title; got != "renamed in the listing" {
		t.Fatalf("the boot listing left %s titled %q, want the listing's own title: the read must be one that records something", movedIssue, got)
	}
	assertMoved("the boot's read of the Dispatch listing, with the pass's clock ahead of the facts", at(6))

	// The activity report, carrying a time of its own that stands ahead of the record's. A report
	// is the session saying it is up, so the time it carries is what a reading that took it for a
	// movement would write, and that is the shape this pins; a mark written from the wall clock
	// instead is older than every time this test asserts and is hidden by the same monotone rule,
	// which no fixture whose stamps run ahead of real time can show.
	if known, err := records.PutClaimActivity(ctx, awaitingArchitect(t, movedIssue),
		claim.SessionActivity{At: ahead, BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"subagent"}}}); err != nil || !known {
		t.Fatalf("report the activity of the architect claim of %s = %t, %v", movedIssue, known, err)
	}
	assertMoved("a claim's session activity report carrying a time ahead of the record's", at(6))

	admission.now = func() time.Time { return fixedNow }

	// A Dispatch event of the issue carrying the status the record already holds: a person renames
	// the running issue and re-ranks it while its status stands still. It is new to the record all
	// the same — the record takes the title, the rank and the event's own sequence — so it is a
	// movement, which is R3's first clause whole: a Dispatch event of the issue, and not only one
	// that moves its status. The engine's own handler reacts to a newer status alone and returns
	// at its status fence, so the mark is written above that fence (workflow's dispatchIssue) and
	// admission records the observation below it. This is the shape every edit of a running issue
	// in its steady state takes — a rename, a re-rank, a re-parent, a label change, the record and
	// the event carrying one status between them. The Dispatch events that must move nothing are
	// the ones this test reads beside it: a redelivery of an event the record has already taken,
	// the boot listing's read, and the daemon's own status write coming back, which is the case
	// below. The status is read from the record rather than written out here, so the event carries
	// whatever the facts above left the issue standing at.
	now = at(24)
	running := issue(t, pool, movedIssue)
	apply(t, pool, admission, "dispatch-edit", intake.DispatchIssue{Key: movedIssue, Seq: 3, Type: "issue.updated",
		Status: running.Status, Title: "a person renamed it while it ran", Rank: "V", HandedOver: true,
		ResolvedRepo: "acme/widgets"}, engine)
	if got := issue(t, pool, movedIssue); got.Title != "a person renamed it while it ran" || got.Rank != "V" {
		t.Fatalf("the edit of %s left it titled %q at rank %q, want the event's own title and rank: the event must be one the record takes",
			movedIssue, got.Title, got.Rank)
	}
	assertMoved("a Dispatch event carrying the status the record already holds", at(24))

	// The same event with the daemon as its author, which is the third exclusion: the status the
	// daemon writes to Dispatch comes back as an event its own session authored, and the daemon
	// talking to itself is no movement of the issue. The two paths that bring one back come round
	// in this one shape — the status repair, which queues the status of the phase the issue
	// already stands in (workflow's statusRepairTick), and the re-assertion the daemon queues when
	// somebody else wrote a status (workflow's sessionStatusWrite) — because each enqueues a
	// status write, the outbox writes it to Dispatch, and the consumer decodes the event that
	// write makes with intake.DispatchIssue.DaemonActor set, by the actor being the issue's own
	// daemon session (intake's decodeDispatchEvent; a route's own write carries the same flag,
	// through intake.OwnStatusWrite). Counting one would make a stopped tree move for ever:
	// nothing outside the daemon happened, and the daemon's own re-assertion would keep
	// refreshing the mark of an issue nobody is working, so the tree this measure exists to
	// notice would never reach the bound and the notice would never be sent.
	//
	// So the event is written in the shape those paths make and no other: the status the record
	// already holds, which is what both of them write, at the next sequence, which clears the
	// engine's sequence fence. Everything else about it is the person's edit above, so the only
	// thing between the two cases is the author — the mark the one writes and the other must not
	// is governed by fact.DaemonActor alone (workflow's dispatchIssue), standing as it does above
	// the status fence both of these events stop at. The engine's clock stands past every time
	// this test asserts, so a mark written for this event would be newer than the one the record
	// carries and this assertion would see it. The record still takes the event's own sequence,
	// below the engine, which is what says this case drove something.
	now = at(25)
	echoed := issue(t, pool, movedIssue)
	apply(t, pool, admission, "own-status-write-echo", intake.DispatchIssue{Key: movedIssue, Seq: 4, Type: "issue.updated",
		Status: echoed.Status, Title: echoed.Title, Rank: echoed.Rank, HandedOver: true, ResolvedRepo: "acme/widgets",
		DaemonActor: true}, engine)
	if got := issue(t, pool, movedIssue).LastDispatchSeq; got != 4 {
		t.Fatalf("the daemon's own status write left %s at sequence %d, want 4: the event must be one the record takes, or this case proves nothing",
			movedIssue, got)
	}
	assertMoved("the daemon's own status write coming back as an event its own session authored", at(24))

	// The four writes of a phase or of a start that no fact of the issue carries, and that no
	// Dispatch event, pull request or completion would ever mark: a root entering its wait on a
	// person (workflow's enterAwaiting), the promotion of a root the waiting line was holding
	// (admit's promoteHolds), the resume of a root a person answered (workflow's
	// ResumeAwaitedRoot, which that same promotion calls), and a re-admission (admit's
	// readmit). Each is driven by the fact that reaches it, as the daemon drives it, and the
	// engine's clock is held apart from the pass's so that every mark names the site that
	// wrote it.
	t.Run("the daemon marks the phases and the starts it writes itself", func(t *testing.T) {
		pool := migratedPool(t)
		const (
			waits      = "LEGION-210"
			promoted   = "LEGION-211"
			readmitted = "LEGION-212"
		)
		// A slotted root whose architect handed a question to a person, a root waiting in the
		// line behind it, and a closed root a person is about to set back to todo. The cap is
		// the two slots the first two take, so the re-admitted root below stays in the line and
		// the mark it carries can only be the re-admission's own.
		seedSlotted(t, pool, waits, "U")
		seedWaiting(t, pool, promoted, "V")
		seedArchitectClaim(t, pool, waits, true)
		seedHandBack(t, pool, waits, "ask-q", 5)
		putIssue(t, pool, record.Issue{Key: readmitted, Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Project: testProject, Title: readmitted, Tree: readmitted,
			Phase: phase.Done, Generation: 1, Status: record.DoneStatus, DispatchStatus: record.DoneStatus,
			Rank: "W", HandedOver: true, LastDispatchSeq: 1})

		var engineNow, passNow time.Time
		engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
			Clock: func() time.Time { return engineNow }}, nil)
		admission := New(record.NewStore(), engine, 2, testProject, nil)
		admission.now = func() time.Time { return passNow }

		// Every clock below runs from just after the stamp the seeding left, because a mark is
		// only ever written forwards: a time behind the row's own would be the monotone no-op
		// the rule makes it, and would prove nothing.
		base := settleMovements(t, pool, waits, promoted, readmitted).Add(time.Minute)
		at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
		marked := func(what, key string, want time.Time) {
			t.Helper()
			if got := movedAt(t, pool, key); !got.Equal(want) {
				t.Fatalf("after %s the record says %s last moved at %s, want %s, the moment of that write",
					what, key, got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
			}
		}

		// One pass writes two of them, each on its own clock: the root that now waits only on a
		// person enters that wait on the engine's, and the root the line was holding is promoted
		// on the pass's.
		engineNow, passNow = at(1), at(2)
		tick(t, pool, admission, "pass-1")
		if got := issue(t, pool, waits); !got.AwaitingPerson() {
			t.Fatalf("%s stands at %s, want it waiting on a person: the wait is the write this case is about", waits, got.Phase)
		}
		marked("a root entered its wait on a person", waits, at(1))
		if got := issue(t, pool, promoted); got.Status != "in_progress" {
			t.Fatalf("%s stands at %s/%s, want the promotion that opens its tree and starts its architect",
				promoted, got.Phase, got.Status)
		}
		marked("the promotion of a root the line was holding", promoted, at(2))

		// The person acts, and the next pass resumes the root where it stands. The resume is
		// the engine's write (workflow's ResumeAwaitedRoot), so it runs on the engine's clock,
		// and the pass's clock is left standing where it was: a mark written on the pass's
		// clock here would be the monotone no-op the rule makes it, so the time the record
		// carries after this pass can only be the resume's own.
		answerWait(t, pool, waits, record.AnsweredByAsk, "ask-q")
		engineNow = at(3)
		tick(t, pool, admission, "pass-2")
		if got := issue(t, pool, waits); got.Phase != phase.Admitted || got.Awaiting != nil {
			t.Fatalf("%s stands at %s holding %+v, want it admitted again with no wait: the resume is the write this case is about",
				waits, got.Phase, got.Awaiting)
		}
		marked("the resume of a root a person answered", waits, at(3))

		// A person sets the closed root back to todo. The engine's own handler marks that event
		// first, on its own clock, so the pass's clock standing after it is what says the
		// re-admission wrote the mark the record carries.
		engineNow, passNow = at(4), at(5)
		apply(t, pool, admission, "readmit", intake.DispatchIssue{Key: readmitted, ResolvedRepo: "acme/widgets",
			Seq: 2, Type: "issue.updated", Status: "todo", Title: readmitted, Rank: "W", HandedOver: handed}, engine)
		if got := issue(t, pool, readmitted); got.Generation != 2 || got.Phase != phase.Admitted {
			t.Fatalf("%s stands at %s in generation %d, want admitted in generation 2: the fact must be the re-admission it is read as",
				readmitted, got.Phase, got.Generation)
		}
		for _, slot := range slots(t, pool) {
			if slot.Issue == readmitted {
				t.Fatalf("%s holds admission slot %d: the cap must keep it in the line, so that the mark it carries is the re-admission's own",
					readmitted, slot.Index)
			}
		}
		marked("a re-admission", readmitted, at(5))
	})

	// A claim that fails takes its issue into held, which is a phase change and so a movement
	// the record keeps (dispatch://INFRA-2560, R3, and Engine.claimFailed). The measure reads
	// it: a tree stops at the moment its worker dies, so its quiet time runs from that moment
	// and not from whatever last touched the issue before it. Without the mark a tree whose
	// worker died hours into a silent phase reads as quiet for longer than it has been, and the
	// controller is told it stopped at the wrong moment.
	//
	// It is a case of its own because it is reached by a fact of its own: the supervisor's
	// report that a claim is dead, which no other movement of R3's list carries. The parts
	// above drive the six movements the requirement names and the four writes of the admission
	// pass, and none of them reaches this one; with the mark taken off that line, every check
	// of this issue but this case stays green, which is why the lock is here.
	//
	// The second report is the other half: an issue already held writes nothing, so a
	// supervisor that reports the same dead claim again moves the tree nowhere.
	t.Run("a failed claim's hold is a movement of its issue", func(t *testing.T) {
		pool := migratedPool(t)
		const issueKey = "LEGION-780"
		now := fixedNow
		engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
			Clock: func() time.Time { return now }}, nil)
		admission := New(record.NewStore(), engine, 1, testProject, nil)
		admission.now = func() time.Time { return fixedNow }

		seedSlotted(t, pool, issueKey, "U")
		stands := issue(t, pool, issueKey)
		stands.Phase, stands.Status, stands.DispatchStatus, stands.LastDispatchSeq = phase.Implementing, "in_progress", "in_progress", 1
		putIssue(t, pool, stands)
		before := movedAt(t, pool, issueKey)
		if before.IsZero() {
			t.Fatalf("the record of %s carries no movement before the failure: every row is stamped when it is written", issueKey)
		}

		// The clock stands past the row's own stamp, because a movement is only ever written
		// forwards: a clock behind it would prove nothing.
		died := before.Add(time.Minute)
		now = died
		applyMovement(t, pool, admission, engine, "api", "implementer-failed",
			intake.ClaimFailed{Issue: issueKey, Role: claim.RoleImplementer})

		if got := issue(t, pool, issueKey).Phase; got != phase.Held {
			t.Fatalf("%s stands in %s after its implementer's claim failed, want held: the fact must be the phase change this test reads", issueKey, got)
		}
		if got := movedAt(t, pool, issueKey); !got.Equal(died) {
			t.Fatalf("after its worker's claim failed the record says %s last moved at %s, want %s, the moment of that fact: a tree stops when its worker dies",
				issueKey, got.Format(time.RFC3339Nano), died.Format(time.RFC3339Nano))
		}

		// The same dead claim reported again: the issue is held already, so nothing is written
		// and the tree moves nowhere.
		now = died.Add(time.Hour)
		applyMovement(t, pool, admission, engine, "api", "implementer-failed-again",
			intake.ClaimFailed{Issue: issueKey, Role: claim.RoleImplementer})

		if got := movedAt(t, pool, issueKey); !got.Equal(died) {
			t.Fatalf("after a second report of the same dead claim the record says %s last moved at %s, want %s still: a repeat of a fact the record already took moves no tree",
				issueKey, got.Format(time.RFC3339Nano), died.Format(time.RFC3339Nano))
		}
	})

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
