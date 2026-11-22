package admit

import (
	"context"
	"fmt"
	"slices"
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
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// architectSweepGeneration is the generation every root of this file stands at. A hand-over row
// is written at that same generation for a carried root, which is what says the root stands in
// the run a hand-over carried it into (record.Store.HandedOverRoot).
const architectSweepGeneration = 2

// architectSweepFixture is one root the sweep is asked about. The base shape, which every case
// changes one field of, is the root the measured defect stands on: a root of its own tree at
// admitted, in the workflow, holding one of the cap's slots, its architect's phases row at
// seededEntry naming the work of that entry, and an architect claim that was given the entry
// before it.
type architectSweepFixture struct {
	key string
	// tree is the issue's tree, its own key where this is empty.
	tree string
	// stands is the phase the issue is recorded in, and status the Dispatch status with it.
	stands phase.Phase
	status string
	// lingers records the root as a park left it: a linger deadline ahead of it.
	lingers bool
	// carried records a hand-over row at generation, which is what says the root stands in the
	// run a hand-over carried it into, and gateCarried marks that row as one whose hand-over
	// brought the old root's design gate (record.HandedOverRoot.GateCarried).
	carried     bool
	gateCarried bool
	// gateOpened registers an approved design gate of the root, which is the root record.AtWork
	// answers false for and this rule reaches.
	gateOpened bool
	// unslotted leaves the root holding none of the cap's slots.
	unslotted bool
	// entry is the entry the architect's phases row names, 0 for a root whose architect holds no
	// row at all, which was handed nothing and is owed nothing.
	entry int64
	// claimed records an architect claim in state, suspended by suspendedBy, given the entry
	// given and holding a delivery of its own where holdsTask is set. A fixture without one is a
	// tree whose architect claim does not exist, which the rule's own start creates. The delivery
	// is of the root's run at no entry, at taskEntry where that is set, and of no run at all, an
	// operator's own task, where taskOfNoRun is set.
	claimed     bool
	state       supervise.ClaimState
	suspendedBy supervise.SuspendSource
	given       int64
	holdsTask   bool
	confirmed   bool
	taskEntry   int64
	taskOfNoRun bool
}

// architectSweepRoot is the base fixture: the shape the rule sends the work of its entry to.
func architectSweepRoot(key string) architectSweepFixture {
	return architectSweepFixture{key: key, stands: phase.Admitted, status: "in_progress", entry: seededEntry,
		claimed: true, state: supervise.StateIdle, given: seededEntry - 1}
}

// seedArchitectSweepRoot records one fixture: the issue, its tree lifecycle, its slot, its
// hand-over row, its design gate, the architect's phases row and the architect's claim.
func seedArchitectSweepRoot(t *testing.T, pool *pgxpool.Pool, f architectSweepFixture) {
	t.Helper()
	ctx := context.Background()
	tree := f.tree
	if tree == "" {
		tree = f.key
	}
	root := record.Issue{Key: f.key, Tree: tree, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: "the root " + f.key, Phase: f.stands, Generation: architectSweepGeneration,
		Status: f.status, DispatchStatus: f.status, Rank: f.key, HandedOver: true, LastDispatchSeq: 1}
	if tree != f.key {
		parent := tree
		root.Parent = &parent
	}
	if f.lingers {
		until := fixedNow.Add(time.Hour)
		root.LingerUntil = &until
	}
	putIssue(t, pool, root)
	seedLifecycle(t, pool, tree)
	if !f.unslotted {
		takeSlot(t, pool, f.key)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if f.carried {
			if err := records.PutHandedOverRoot(ctx, tx, record.HandedOverRoot{Issue: f.key,
				Generation: architectSweepGeneration, GateCarried: f.gateCarried}); err != nil {
				t.Fatalf("record %s as a root a hand-over created: %v", f.key, err)
			}
		}
		if f.gateOpened {
			version := 1
			if err := records.PutGate(ctx, tx, record.DesignGate{Issue: f.key, ArtifactID: "artifact-" + f.key,
				LatestVersion: 1, ApprovedVersion: &version, EverOpened: true}); err != nil {
				t.Fatalf("record the open design gate of %s: %v", f.key, err)
			}
		}
	})
	if f.entry != 0 {
		putArchitectRow(t, pool, f.key, f.entry, fixedNow)
	}
	if !f.claimed {
		return
	}
	token := putWorkerClaim(t, pool, tree, f.key, claim.RoleArchitect, architectSweepGeneration, f.state, f.suspendedBy)
	if f.given != 0 {
		markGivenEntry(t, pool, token, f.given)
	}
	if f.holdsTask {
		putWorkerTask(t, pool, token, architectSweepGeneration, "", f.confirmed)
		generation := int64(architectSweepGeneration)
		if f.taskOfNoRun {
			generation = 0
		}
		if _, err := pool.Exec(ctx, `update pending_task_deliveries set entry = $2, generation = $3 where claim_token = $1`,
			string(token), f.taskEntry, generation); err != nil {
			t.Fatalf("give the task the architect claim of %s holds entry %d of generation %d: %v", f.key, f.taskEntry, generation, err)
		}
	}
}

// architectSweepStarts is every architect start row of root in the outbox, oldest first, with
// the id that tells a row written once from a row written again (record.Store.EnqueueStart).
func architectSweepStarts(t *testing.T, pool *pgxpool.Pool, root string) []convergeStart {
	t.Helper()
	var starts []convergeStart
	for _, start := range convergeStarts(t, pool, root) {
		if start.role == claim.RoleArchitect {
			starts = append(starts, start)
		}
	}
	return starts
}

// The reach of the rule over a root's own architect: the three roots whose architect was queued
// no entry and is sent the work of the entry its phases row names. Each is the same fixture with
// one field changed, and the refusals below are the same fixture again, so each answer turns on
// the one thing its case names.
//
// The second is the measured defect (dispatch://INFRA-2614): record.AtWork answers false for a
// root at admitted whose design gate has opened, while the root's own architect is the only role
// at work on the root itself. The fourth is the first root again, its claim holding the task of
// the entry before unconfirmed: a task the row has moved past is no start on its way, so it holds
// nothing back (dispatch://INFRA-2614 R4).
func TestTheArchitectSweepReachesEveryRootWhoseArchitectIsOwedItsEntry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*architectSweepFixture)
		// label is the shape of the first task the derivation writes for the root: the work of
		// its entry is derived again from the record, since its row keeps no wait a resume
		// answered.
		label string
		why   string
	}{
		{
			name: "a root at admitted whose gate has never opened", change: func(*architectSweepFixture) {},
			label: workflow.ArchitectFreshTreeLabel,
			why:   "its architect is writing the design, which is the whole of the tree's work",
		},
		{
			name: "a root at admitted whose gate has opened", change: func(f *architectSweepFixture) { f.gateOpened = true },
			label: workflow.ArchitectGateOpenLabel,
			why:   "the children do the work after that opening, and the root's own architect is still the role at work here",
		},
		{
			name: "a root a hand-over carried in, in the phase that came with it",
			change: func(f *architectSweepFixture) {
				f.stands, f.carried = phase.Implementing, true
			},
			label: workflow.ArchitectCarriedPhaseLabel,
			why:   "the carry put it in that phase, and its architect has still to be told the tree is its own",
		},
		{
			name: "a root whose architect holds the unconfirmed task of the entry before",
			change: func(f *architectSweepFixture) {
				f.state, f.holdsTask, f.taskEntry = supervise.StateReady, true, seededEntry-1
			},
			label: workflow.ArchitectFreshTreeLabel,
			why: "the row has moved past the entry that task is of, so it is no start on its way, and the start of the row's " +
				"entry takes its place in the claim's hands (workflow.ArchitectStartPending, supervise.Delivery.SupersededBy)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			const root = "LEGION-810"
			fixture := architectSweepRoot(root)
			tc.change(&fixture)
			seedArchitectSweepRoot(t, pool, fixture)

			applySweep(t, pool, admission, "architect-reach")

			starts := architectSweepStarts(t, pool, root)
			if len(starts) != 1 {
				t.Fatalf("the architect starts of %s = %+v, want the one the rule writes: %s", root, starts, tc.why)
			}
			if starts[0].entry != seededEntry || starts[0].task == "" {
				t.Fatalf("the architect start of %s = %+v, want the work of entry %d: %s", root, starts[0], seededEntry, tc.why)
			}
			if !strings.Contains(starts[0].task, tc.label) {
				t.Fatalf("the task sent to %s = %q, want the first-task instruction the derivation writes, labelled %q: %s", root, starts[0].task, tc.label, tc.why)
			}
		})
	}
}

// What the rule leaves alone. Each case is the fixture the reach cases send work to with one
// field changed, so every one of them is quiet for the reason it names and for no other: a
// shape that went quiet because its fixture never stood up would prove nothing.
func TestTheArchitectSweepRefusesEveryRootItsReachLeavesOut(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*architectSweepFixture)
		why    string
	}{
		{
			name: "a root that lingers", change: func(f *architectSweepFixture) { f.lingers, f.status = true, "done" },
			why: "nothing of a tree that has left the workflow is started again until its re-admission (workflow.ArchitectHears)",
		},
		{
			name: "a root Dispatch has taken out of the workflow", change: func(f *architectSweepFixture) { f.status = "backlog" },
			why: "its architect cannot hear anything the daemon sends it (workflow.ArchitectHears, record.OutOfWorkflow)",
		},
		{
			name: "a root waiting in todo for a slot", change: func(f *architectSweepFixture) { f.unslotted, f.status = true, "todo" },
			why: "its tree is not open yet: the promotion that opens it writes the architect's start (record.WaitsForSlot)",
		},
		{
			name: "an issue that is no tree root", change: func(f *architectSweepFixture) { f.tree = "LEGION-800" },
			why: "a child's own sub-architect claim is outside the rule, since its start carries no task and raises no entry",
		},
		{
			name: "a root past admitted under its own power", change: func(f *architectSweepFixture) { f.stands = phase.Implementing },
			why: "the phase it moved itself into has a role that owns its work, and the entry its architect's row names is the derivation's, not a resume's",
		},
		{
			name: "a root in accepting", change: func(f *architectSweepFixture) { f.stands, f.status = phase.Accepting, "needs_review" },
			why: "the notice that moved it there is what tells its architect",
		},
		{
			name: "a root whose architect claim an operator suspended",
			change: func(f *architectSweepFixture) {
				f.state, f.suspendedBy = supervise.StateSuspended, supervise.SuspendedByOperator
			},
			why: "`legion claims suspend` suspended that architect, and an operator's suspension holds against every rule of the daemon's (supervise's Claim.RuleHolds)",
		},
		{
			name: "a root whose architect claim is retired", change: func(f *architectSweepFixture) { f.state = supervise.StateRetired },
			why: "a retired claim was stopped, and a start here would hand the operator back the worker they stopped",
		},
		{
			name: "a root whose architect holds a task of no run it has not confirmed",
			change: func(f *architectSweepFixture) {
				f.state, f.holdsTask, f.taskOfNoRun = supervise.StateReady, true, true
			},
			why: "a task of no run, an operator's own, is one no start of the root's run takes the place of, so a task in the " +
				"claim's hands is not owed again (workflow.ArchitectStartPending)",
		},
		{
			name: "a root whose architect claim was given this entry", change: func(f *architectSweepFixture) { f.given = seededEntry },
			why: "the claim was queued this entry's work already (workflow.OwedEntry)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			const root = "LEGION-811"
			fixture := architectSweepRoot(root)
			tc.change(&fixture)
			seedArchitectSweepRoot(t, pool, fixture)

			applySweep(t, pool, admission, "architect-refusal")

			if starts := architectSweepStarts(t, pool, root); len(starts) != 0 {
				t.Fatalf("the architect starts of %s = %+v, want none: %s", root, starts, tc.why)
			}
		})
	}
}

// A root architect whose phases row names no entry is handed one: the sweep raises the entry
// with the task the derivation composes over the record as it stands, writes that task as the
// work of the entry on the row, and the start it writes carries both
// (workflow.Engine.StartArchitect). Two roots stand in that shape — one the daemon of the
// release before this one admitted after this release's migration ran, and one whose architect
// claim exists while no transition of this generation raised an entry for it — and each would
// otherwise hold its slot with an architect that was handed nothing at all.
//
// A second pass raises no second entry and writes no second row: by then the row names entry 1
// and the claim's own start is on its way (workflow.ArchitectStartPending).
//
// The claim's skips stand in front of the raise. An architect holding a task it has not
// confirmed — the old release's own start, still unanswered — is passed over until it confirms
// it, so no second prompt reaches a worker that is reading the first.
//
// A phase worker of the same shape is owed nothing: no transition of its issue has ever handed
// that role work, and nothing here composes work for a role the workflow has not started
// (workflow.OwedEntry).
func TestTheArchitectSweepRaisesTheEntryOfARootWhoseArchitectRowNamesNone(t *testing.T) {
	t.Run("the root whose architect row names no entry", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		const root = "LEGION-812"
		fixture := architectSweepRoot(root)
		fixture.entry, fixture.given = 0, 0
		seedArchitectSweepRoot(t, pool, fixture)

		applySweep(t, pool, admission, "architect-raise-1")

		row := architectRow(t, pool, root)
		if row.Entry != 1 || row.EntryTask == "" || row.EntryRaisedAt.IsZero() {
			t.Fatalf("the architect row of %s = entry %d, task %q, raised at %s; want entry 1 carrying the derivation's own work",
				root, row.Entry, row.EntryTask, row.EntryRaisedAt)
		}
		if want := architectStart(t, pool, admission, root, ""); row.EntryTask != want {
			t.Fatalf("the work the raise wrote = %q, want the derivation's %q", row.EntryTask, want)
		}
		starts := architectSweepStarts(t, pool, root)
		if len(starts) != 1 || starts[0].entry != 1 || starts[0].task != row.EntryTask {
			t.Fatalf("the architect starts of %s = %+v, want the one of entry 1 carrying %q", root, starts, row.EntryTask)
		}
		if !strings.Contains(starts[0].task, workflow.ArchitectFreshTreeLabel) || row.EntryAwaiting != nil {
			t.Fatalf("the task sent to %s = %q, wait kept %+v; want the first-task instruction the derivation writes, derived again at a later send",
				root, starts[0].task, row.EntryAwaiting)
		}

		applySweep(t, pool, admission, "architect-raise-2")

		if after := architectRow(t, pool, root); after.Entry != 1 {
			t.Fatalf("the architect row of %s after a second pass = entry %d, want the 1 the first raised: one entry is raised once", root, after.Entry)
		}
		if again := architectSweepStarts(t, pool, root); len(again) != 1 || again[0] != starts[0] {
			t.Fatalf("the architect starts of %s after a second pass = %+v, want the one row the first wrote", root, again)
		}
	})

	t.Run("the architect still holding the older release's task", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		const root = "LEGION-813"
		fixture := architectSweepRoot(root)
		fixture.entry, fixture.given = 0, 0
		fixture.state, fixture.holdsTask, fixture.confirmed = supervise.StateReady, true, false
		seedArchitectSweepRoot(t, pool, fixture)

		applySweep(t, pool, admission, "architect-raise-pending")

		if row := architectRow(t, pool, root); row.Entry != 0 {
			t.Fatalf("the architect row of %s = entry %d, want none raised: a task in the claim's hands is not owed again", root, row.Entry)
		}
		if starts := architectSweepStarts(t, pool, root); len(starts) != 0 {
			t.Fatalf("the architect starts of %s = %+v, want none while its claim holds an unconfirmed task", root, starts)
		}
	})

	t.Run("the phase worker whose row names no entry", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 2)
		const root, member = "LEGION-814", "LEGION-815"
		seedArchitectSweepRoot(t, pool, architectSweepRoot(root))
		putIssue(t, pool, record.Issue{Key: member, Tree: root, Parent: record.ParentOf(root), Repo: "acme/widgets",
			DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: "the member " + member,
			Phase: phase.Testing, Generation: architectSweepGeneration, Status: "in_progress", DispatchStatus: "in_progress",
			Rank: member, HandedOver: true, LastDispatchSeq: 1})
		takeSlot(t, pool, member)
		putWorkerClaim(t, pool, root, member, claim.RoleTester, architectSweepGeneration, supervise.StateIdle, "")

		applySweep(t, pool, admission, "worker-no-row")

		if starts := convergeStarts(t, pool, member); len(starts) != 0 {
			t.Fatalf("the starts of %s = %+v, want none: a phase worker whose row names no entry is owed none", member, starts)
		}
	})
}

// Ten passes write one start row. The rule writes a root architect's start directly, so no
// owed-start replacement dedupes it (workflow.StartOrOwe is for phase workers alone): the term
// that keeps one entry to one row is the start already on its way (workflow.ArchitectStartPending,
// workflow.OwedEntry), and without it a sweep a minute would write a row a minute.
//
// Three roots stand together: one whose architect claim does not exist at all, which is the row
// the rule creates the claim with; one whose claim was given the entry before, which is sent the
// work once; and one whose claim an operator stopped, which is left alone throughout.
func TestTheArchitectSweepWritesOneStartOverRepeatedPasses(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 3)
	const unclaimed, behind, stopped = "LEGION-820", "LEGION-821", "LEGION-822"
	noClaim := architectSweepRoot(unclaimed)
	noClaim.claimed = false
	seedArchitectSweepRoot(t, pool, noClaim)
	seedArchitectSweepRoot(t, pool, architectSweepRoot(behind))
	operatorStopped := architectSweepRoot(stopped)
	operatorStopped.state, operatorStopped.suspendedBy = supervise.StateSuspended, supervise.SuspendedByOperator
	seedArchitectSweepRoot(t, pool, operatorStopped)

	var first []convergeStart
	for pass := range 10 {
		applySweep(t, pool, admission, fmt.Sprintf("architect-repeat-%d", pass))
		if pass == 0 {
			first = append(architectSweepStarts(t, pool, unclaimed), architectSweepStarts(t, pool, behind)...)
		}
	}

	for _, key := range []string{unclaimed, behind} {
		starts := architectSweepStarts(t, pool, key)
		if len(starts) != 1 {
			t.Fatalf("the architect starts of %s after ten passes = %+v, want the one the first pass wrote", key, starts)
		}
		if !slices.Contains(first, starts[0]) {
			t.Fatalf("the architect start of %s after ten passes = %+v, want the %+v the first pass wrote: a replacement is a row of its own", key, starts[0], first)
		}
	}
	if starts := architectSweepStarts(t, pool, stopped); len(starts) != 0 {
		t.Fatalf("the architect starts of %s after ten passes = %+v, want none: a claim an operator stopped is left alone by every pass", stopped, starts)
	}
}

// The task the rule sends is derived again from the record as it stands, rather than read off
// the phases row the entry was raised with. The reason is measured: a kept decision is claimed by
// the outbox row that names it, and record.Postgres.UnclaimedKept locks and reads only what
// nothing claims, so a text composed once and sent again could name a decision another telling
// has already claimed. The row's own stored text is a different string here, which is what makes
// the two readings tell apart.
func TestTheArchitectSweepSendsTheTaskTheDerivationGivesOverTheRecord(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	const root = "LEGION-830"
	seedArchitectSweepRoot(t, pool, architectSweepRoot(root))

	applySweep(t, pool, admission, "architect-derivation")

	starts := architectSweepStarts(t, pool, root)
	if len(starts) != 1 {
		t.Fatalf("the architect starts of %s = %+v, want the one the rule writes", root, starts)
	}
	if want := architectStart(t, pool, admission, root, ""); starts[0].task != want {
		t.Fatalf("the task the rule sent = %q, want the derivation's own %q over the record as it stands: the text stored on the "+
			"phases row could name a kept decision another telling has already claimed", starts[0].task, want)
	}
	if row := architectRow(t, pool, root); row.Entry != seededEntry {
		t.Fatalf("the architect's row after the re-send = entry %d, want the %d it stood at: the rule sends the work of an entry and raises none", row.Entry, seededEntry)
	}
}

// A root put back to admitted by the resume of a person's answer is sent the work of its entry
// when the resume's start was lost, and the resume here is a real one: LEGION-100 waits on its
// question ask-q, alice answers it, and the admission pass of that answer resumes the root
// (workflow.Engine.ResumeAwaitedRoot). The resume raises the architect's entry one above the entry
// the wait began on (workflow.Engine.EnterArchitect), writes its task as the work of the raised
// entry and keeps beside it the wait alice answered, and queues the architect's start at that
// entry. That start carries the wait and no text, since its task is rendered at its send from the
// record as it stands then. The architect took the work of the entry the wait began on, so its
// claim stands at that entry.
//
// The lane then finishes the resume's start row without the task reaching the claim (runStart),
// as the outbox's fences or a lost lane leave one. The claim stands one entry below its row, and
// the sweep is the only thing left that sends the raised entry's work. One sweep writes one
// architect start at the raised entry, built from the wait the row keeps
// (workflow.Engine.SendArchitectStart): the resume's own start again, that wait and no text,
// claiming nothing, which its send renders into the resume's task, naming the answer. A second
// sweep, while that row is still queued, writes no second row.
//
// The control is the same resume with its start row left in the outbox: that row already carries
// the raised entry (workflow.OwedEntry), so the sweep writes nothing beside it.
func TestTheArchitectSweepSendsTheEntryAResumeRaisedWhenItsStartWasLost(t *testing.T) {
	const root = "LEGION-100"
	answeredAt := fixedNow
	answered := record.Awaiting{Ask: "ask-q", Since: seqMark(10), AnsweredAt: &answeredAt, AnsweredBy: record.AnsweredByAsk,
		AnsweredDetail: "ask-q"}
	// resumed records LEGION-100 waiting on ask-q, its architect's row and claim both at
	// seededEntry, and applies alice's answer. It checks what the resume wrote — the root back at
	// admitted, the architect's row one entry up carrying the resume's task and the wait alice
	// answered, and one start row of that entry carrying that wait and no text, which its send
	// renders into that same task — and answers the pool, the admission, the row and that start.
	resumed := func(t *testing.T) (*pgxpool.Pool, *Admission, record.PhaseRow, convergeStart) {
		t.Helper()
		pool := migratedPool(t)
		admission := quiet(t, 1)
		awaitingRoot(t, pool, record.Awaiting{Ask: "ask-q", Since: seqMark(10)})
		putArchitectRow(t, pool, root, seededEntry, handedBackAt)
		token := putWorkerClaim(t, pool, root, root, claim.RoleArchitect, 1, supervise.StateIdle, "")
		markGivenEntry(t, pool, token, seededEntry)

		applyFact(t, pool, admission, "answer", answeredBy("ask-q", 11))

		if got := issue(t, pool, root); got.Phase != phase.Admitted || got.Awaiting != nil {
			t.Fatalf("%s after alice's answer stands in %s waiting on %+v, want it resumed at admitted with no wait", root, got.Phase, got.Awaiting)
		}
		row := architectRow(t, pool, root)
		if row.Entry != seededEntry+1 || !strings.Contains(row.EntryTask, "ask-q") || !sameWait(row.EntryAwaiting, answered) {
			t.Fatalf("the architect row of %s after the resume = entry %d carrying %q, keeping the wait %+v; want entry %d carrying the resume's task, "+
				"which names ask-q, and the wait %+v", root, row.Entry, row.EntryTask, row.EntryAwaiting, seededEntry+1, answered)
		}
		starts := architectSweepStarts(t, pool, root)
		if len(starts) != 1 || starts[0].entry != row.Entry {
			t.Fatalf("the architect starts of %s after the resume = %+v, want the one of entry %d", root, starts, row.Entry)
		}
		assertResumeStart(t, pool, root, row.Entry, answered, row.EntryTask)
		return pool, admission, row, starts[0]
	}

	t.Run("the resume's start was lost", func(t *testing.T) {
		pool, admission, row, _ := resumed(t)
		runStart(t, pool, root)
		if starts := architectSweepStarts(t, pool, root); len(starts) != 0 {
			t.Fatalf("the architect starts of %s after the lane finished the resume's = %+v, want none", root, starts)
		}
		var given int64
		if err := pool.QueryRow(context.Background(), `select given_entry from claims where token = $1`,
			string(architectToken(t, root))).Scan(&given); err != nil {
			t.Fatalf("read the entry the architect claim of %s was given: %v", root, err)
		}
		if given != seededEntry {
			t.Fatalf("the architect claim of %s was given entry %d, want the %d the wait began on: the lost row gave it nothing of entry %d",
				root, given, seededEntry, row.Entry)
		}

		applySweep(t, pool, admission, "resume-lost-1")

		starts := architectSweepStarts(t, pool, root)
		if len(starts) != 1 {
			t.Fatalf("the architect starts of %s after the sweep = %+v, want the one the sweep writes for the entry %d the resume raised",
				root, starts, row.Entry)
		}
		if starts[0].entry != row.Entry {
			t.Fatalf("the architect start of %s after the sweep = %+v, want it at the entry %d the resume raised", root, starts[0], row.Entry)
		}
		// The sweep's start is the resume's own: the wait the row keeps and no text, rendered at
		// its send into the task that names the answer.
		assertResumeStart(t, pool, root, row.Entry, answered, row.EntryTask)
		if after := architectRow(t, pool, root); after.Entry != row.Entry {
			t.Fatalf("the architect row of %s after the sweep = entry %d, want the %d the resume raised: the sweep sends the work of an entry and raises none",
				root, after.Entry, row.Entry)
		}

		applySweep(t, pool, admission, "resume-lost-2")

		if again := architectSweepStarts(t, pool, root); len(again) != 1 || again[0] != starts[0] {
			t.Fatalf("the architect starts of %s after a second sweep = %+v, want only the %+v the first wrote: that row is still on its way",
				root, again, starts[0])
		}
	})

	t.Run("the resume's start still stands", func(t *testing.T) {
		pool, admission, _, start := resumed(t)

		applySweep(t, pool, admission, "resume-standing")

		if starts := architectSweepStarts(t, pool, root); len(starts) != 1 || starts[0] != start {
			t.Fatalf("the architect starts of %s after the sweep = %+v, want only the %+v the resume wrote: a start at the raised entry is already on its way",
				root, starts, start)
		}
	})
}

// A person's word on a document can move between the resume that raised an entry and the sweep's
// send of that entry again, and the send tells the person's word as it stands then
// (dispatch://INFRA-3136/spec R2). LEGION-100 waits on its spec at version 4, and alice requests
// changes to version 4: the admission pass of that review resumes the root, raising the
// architect's entry with the task that asks for the revision (workflow.Engine.ResumeAwaitedRoot).
// The resume's start is lost, and alice then approves version 4, which Dispatch records after her
// review. The sweep's start at the resume's entry, rendered as its send renders it (startText,
// as the daemon's rootTask renders it, through workflow.ResumeTask), says the approval followed
// the review and names no revision, and says the gate is open: what the resume would say now.
//
// The control is the task the row keeps, which the release before this one sent again as written:
// it asks for the revision alice's approval answered. A sweep that sent that text again fails this
// test on it.
func TestTheArchitectSweepsResendOfALostResumeTellsTheApprovalThatSupersededItsReview(t *testing.T) {
	const root = "LEGION-100"
	pool := migratedPool(t)
	admission := quiet(t, 1)
	awaitingRoot(t, pool, record.Awaiting{GateVersion: 4, Since: seqMark(10)})
	putArchitectRow(t, pool, root, seededEntry, handedBackAt)
	token := putWorkerClaim(t, pool, root, root, claim.RoleArchitect, 1, supervise.StateIdle, "")
	markGivenEntry(t, pool, token, seededEntry)

	applyFact(t, pool, admission, "changes", intake.DispatchArtifact{Key: root, ArtifactID: "artifact-root",
		Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: seqMark(11)})
	if got := issue(t, pool, root); got.Phase != phase.Admitted || got.Awaiting != nil {
		t.Fatalf("%s after alice's review stands in %s waiting on %+v, want it resumed at admitted", root, got.Phase, got.Awaiting)
	}
	resumed := architectRow(t, pool, root)
	const revise = "A person requested changes to your"
	if resumed.Entry != seededEntry+1 || resumed.EntryAwaiting == nil || resumed.EntryAwaiting.AnsweredBy != record.AnsweredByChanges ||
		!strings.Contains(resumed.EntryTask, revise) || !strings.Contains(resumed.EntryTask, "name the rollback path") {
		t.Fatalf("the architect row of %s after the resume = entry %d carrying %q, keeping the wait %+v; want entry %d asking for alice's "+
			"revision and keeping the changes request's wait, or this case proves nothing", root, resumed.Entry, resumed.EntryTask,
			resumed.EntryAwaiting, seededEntry+1)
	}
	answered := *resumed.EntryAwaiting

	// The resume's start is lost: nothing of it reaches the claim, which stands at the entry the
	// wait began on. Then alice approves the version she reviewed.
	runStart(t, pool, root)
	applyFact(t, pool, admission, "approved", intake.DispatchArtifact{Key: root, ArtifactID: "artifact-root",
		Kind: intake.DispatchArtifactApproved, Version: 4, Mark: seqMark(12)})
	applySweep(t, pool, admission, "resume-lost-after-the-approval")

	starts := architectStartsOf(t, pool, root)
	if len(starts) != 1 || starts[0].Entry != resumed.Entry {
		t.Fatalf("the architect starts of %s after the approval and the sweep = %+v, want the one the sweep writes for the entry %d the resume raised",
			root, starts, resumed.Entry)
	}
	if starts[0].Task != "" || !sameWait(starts[0].Awaiting, answered) {
		t.Errorf("the sweep's start of %s = task %q answering %+v, want the resume's own start: no text, and the wait %+v the row keeps",
			root, starts[0].Task, starts[0].Awaiting, answered)
	}
	sent := startText(t, pool, root, config.DesignGateRootIssues, starts[0])
	t.Logf("the sweep's start of %s, rendered as its send renders it: %q", root, sent)
	for _, superseded := range []string{revise, "Read the review, and revise", "name the rollback path"} {
		if strings.Contains(sent, superseded) {
			t.Errorf("the task sent again for entry %d is %q; it holds %q, a revision alice's approval of version 4 answered after her review",
				resumed.Entry, sent, superseded)
		}
	}
	for _, want := range []string{"was approved at version 4 after it", "asks for no revision now", workflow.DesignApprovedWake} {
		if !strings.Contains(sent, want) {
			t.Errorf("the task sent again for entry %d is %q; it does not say %q, which the resume says of the record as it stands now",
				resumed.Entry, sent, want)
		}
	}
}

// A root a hand-over carried in is inside the reach in every phase of the run that carried it,
// and the task the sweep sends it names the phase it stands in now and the run that hand-over
// carried it into (dispatch://INFRA-2614 R3, R5). The record holds no fact that says the root
// still stands in the phase that came with it — the hand-over row names no phase
// (workflow.ArchitectCarried) — so the task says what holds of a root in its carried phase and
// of one that has moved on in that same run, and never that the phase it stands in is the one
// the carry put it in. The gate clause is the hand-over row's own
// (record.HandedOverRoot.GateCarried): a child handed over from a tree whose root was reopened in
// its linger comes with no gate.
func TestTheArchitectSweepTellsACarriedRootThePhaseItStandsInNow(t *testing.T) {
	for _, tc := range []struct {
		name        string
		stands      phase.Phase
		gateCarried bool
		says        string
	}{
		{
			name: "the phase that came with it", stands: phase.Implementing, gateCarried: true,
			says: "stands in implementing, in the run a hand-over carried it into, with the design gate that hand-over brought.",
		},
		{
			name: "a phase of that run it has moved on to", stands: phase.Testing, gateCarried: true,
			says: "stands in testing, in the run a hand-over carried it into, with the design gate that hand-over brought.",
		},
		{
			name: "a phase of that run it has moved on to, with no gate carried", stands: phase.Testing,
			says: "stands in testing, in the run a hand-over carried it into, and that hand-over brought no design gate.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			const root = "LEGION-816"
			fixture := architectSweepRoot(root)
			fixture.stands, fixture.carried, fixture.gateCarried = tc.stands, true, tc.gateCarried
			seedArchitectSweepRoot(t, pool, fixture)

			applySweep(t, pool, admission, "carried-phase")

			starts := architectSweepStarts(t, pool, root)
			if len(starts) != 1 || starts[0].entry != seededEntry {
				t.Fatalf("the architect starts of %s = %+v, want the one carrying the work of entry %d: a carried root is inside "+
					"the reach in every phase of the run that carried it", root, starts, seededEntry)
			}
			if !strings.Contains(starts[0].task, tc.says) {
				t.Fatalf("the task sent to %s = %q, want it to say %q", root, starts[0].task, tc.says)
			}
			if strings.Contains(starts[0].task, "the phase a hand-over carried it in at") {
				t.Fatalf("the task sent to %s = %q; it names the phase it stands in as the phase the hand-over carried it in at, "+
					"which the record does not say", root, starts[0].task)
			}
		})
	}
}

// Every role of the daemon is inside the rule, read from claim.Roles so that a role added to the
// daemon fails this test until somebody decides where it stands. Five of them are reached as the
// role of the phase their issue stands in (workflow.RoleFor, record.AtWork); the architect is
// reached by a reading of its own, which is wider at admitted and narrower everywhere else
// (workflow.ArchitectAtWork):
//
//   - the architect is reached on a root standing at admitted, whatever its design gate, on a
//     root a hand-over carried in, in every phase of the run that carried it
//     (TestTheArchitectSweepTellsACarriedRootThePhaseItStandsInNow), and on a root
//     past admitted whose architect was not given the entry a resume raised
//     (TestTheArchitectSweepResendsALostResumeOfARootTheAnswerPromotedPastAdmitted);
//   - the one claim that reach leaves out is a child's own sub-architect, whose start carries no
//     task and raises no entry and which workflow.ArchitectHears answers false for.
//
// Each role is seeded behind the entry its phases row names and read for the one start the rule
// writes it, so "inside the rule" is what the pass does rather than a list.
func TestTheArchitectSweepRoleTotalityCoversEveryRoleOfClaimRoles(t *testing.T) {
	// reached is where each role is put at work: the phase its issue stands in, and the keys of
	// that issue and of its tree. The architect's names the root it stands on, which holds the
	// slot itself.
	type standing struct {
		stands    phase.Phase
		key, root string
	}
	reached := map[claim.Role]standing{
		claim.RoleArchitect:   {stands: phase.Admitted, key: "LEGION-840", root: "LEGION-840"},
		claim.RolePlanner:     {stands: phase.Planning, key: "LEGION-851", root: "LEGION-861"},
		claim.RoleImplementer: {stands: phase.Implementing, key: "LEGION-852", root: "LEGION-862"},
		claim.RoleTester:      {stands: phase.Testing, key: "LEGION-853", root: "LEGION-863"},
		claim.RoleReviewer:    {stands: phase.Reviewing, key: "LEGION-854", root: "LEGION-864"},
		claim.RoleMerger:      {stands: phase.Merging, key: "LEGION-855", root: "LEGION-865"},
	}
	for _, role := range claim.Roles {
		if _, named := reached[role]; !named {
			t.Fatalf("claim.Roles holds the role %s and this test's table does not: decide where it stands in the rule, and write it here", role)
		}
	}
	for role := range reached {
		if !slices.Contains(claim.Roles, role) {
			t.Fatalf("this test's table holds the role %s, which claim.Roles does not: remove it here", role)
		}
	}

	pool := migratedPool(t)
	admission := quiet(t, len(reached)+2)
	const subArchitect, subArchitectTree = "LEGION-845", "LEGION-844"
	for role, at := range reached {
		if role == claim.RoleArchitect {
			continue
		}
		seedConvergeRow(t, pool, convergeRow{root: at.root, key: at.key, stands: at.stands,
			claimed: true, state: supervise.StateIdle, given: seededEntry - 1})
	}
	atAdmitted, carried := reached[claim.RoleArchitect].key, "LEGION-841"
	seedArchitectSweepRoot(t, pool, architectSweepRoot(atAdmitted))
	carriedRoot := architectSweepRoot(carried)
	carriedRoot.stands, carriedRoot.carried = phase.Reviewing, true
	seedArchitectSweepRoot(t, pool, carriedRoot)
	child := architectSweepRoot(subArchitect)
	child.tree = subArchitectTree
	seedArchitectSweepRoot(t, pool, child)

	applySweep(t, pool, admission, "architect-totality")

	for role, at := range reached {
		if role == claim.RoleArchitect {
			continue
		}
		starts := convergeStarts(t, pool, at.key)
		if len(starts) != 1 || starts[0].role != role || starts[0].entry != seededEntry {
			t.Fatalf("the start rows of the %s of %s = %+v, want the one carrying entry %d: every role of claim.Roles is inside the rule",
				role, at.key, starts, seededEntry)
		}
	}
	for _, root := range []string{atAdmitted, carried} {
		if starts := architectSweepStarts(t, pool, root); len(starts) != 1 || starts[0].entry != seededEntry {
			t.Fatalf("the architect starts of %s = %+v, want the one carrying entry %d: the architect's reach is a root at admitted and a root a hand-over carried in",
				root, starts, seededEntry)
		}
	}
	if starts := architectSweepStarts(t, pool, subArchitect); len(starts) != 0 {
		t.Fatalf("the architect starts of %s = %+v, want none: a child's own sub-architect is the one claim the reach leaves out", subArchitect, starts)
	}
}
