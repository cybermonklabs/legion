package admit

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// architectWakeGeneration is the run every root of this file stands at, where no case says
// otherwise. The mark that keeps one arm to one wake is keyed by it (record.ArchitectStall).
const architectWakeGeneration = 2

// pastTheHandshake is how long ago a fixture queues a task or raises an entry when the case is
// about an architect that has had the work long enough: one minute past the bound the two
// handshake arms read (workflow.StrandedReleaseWake). insideTheHandshake is the other side of
// that bound, and pastTheGate and insideTheGate the two sides of the gate arm's own
// (workflow.ArchitectGateWake).
var (
	pastTheHandshake   = workflow.StrandedReleaseWake + time.Minute
	insideTheHandshake = workflow.StrandedReleaseWake - time.Minute
	pastTheGate        = workflow.ArchitectGateWake + 30*time.Minute
	insideTheGate      = workflow.ArchitectGateWake - 30*time.Minute
)

// architectWakeFixture is one root the stall reading is asked about. The base shape
// (architectWakeRoot) is a root of its own tree at admitted, in the workflow, holding one of the
// cap's slots admitted now, its architect's phases row at seededEntry raised now, and an
// architect claim that holds the confirmed task of that entry: a root the reading raises nothing
// for. Each case changes the fields its own arm is about.
type architectWakeFixture struct {
	key        string
	generation uint64
	// stands is the phase the root is recorded in, and status the Dispatch status with it.
	stands phase.Phase
	status string
	// carried records a hand-over row at the root's generation, which is what says the phase it
	// stands in came with it (record.Store.HandedOverRoot).
	carried bool
	// registered gives the root a design-gate row its architect registered, which is what the
	// gate arm reads the absence of.
	registered bool
	// admittedAt is the moment the root's admission slot records (record.Slot.AdmittedAt).
	admittedAt time.Time
	// entry is the entry the architect's phases row names, and raisedAt when the transition
	// raised it. A fixture with no entry holds no phases row at all.
	entry    int64
	raisedAt time.Time
	// claimed records the architect's claim in state, given the entry given. A fixture without
	// one is a root whose architect claim does not exist.
	claimed bool
	state   supervise.ClaimState
	given   int64
	// task gives that claim the delivery of entry, queued at queuedAt and confirmed where
	// confirmed is set: the handshake the first arm reads.
	task      bool
	queuedAt  time.Time
	confirmed bool
}

// architectWakeRoot is the base fixture: the root the reading raises no arm for.
func architectWakeRoot(key string) architectWakeFixture {
	return architectWakeFixture{key: key, generation: architectWakeGeneration, stands: phase.Admitted, status: "in_progress",
		admittedAt: fixedNow, entry: seededEntry, raisedAt: fixedNow, claimed: true, state: supervise.StateIdle,
		given: seededEntry, task: true, queuedAt: fixedNow, confirmed: true}
}

// seedArchitectWakeRoot records one fixture: the issue, its tree lifecycle, its slot, its
// hand-over row, its design gate, the architect's phases row, and the architect's claim with the
// task it holds.
func seedArchitectWakeRoot(t *testing.T, pool *pgxpool.Pool, f architectWakeFixture) {
	t.Helper()
	ctx := context.Background()
	putIssue(t, pool, record.Issue{Key: f.key, Tree: f.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: "the root " + f.key, Phase: f.stands, Generation: f.generation,
		Status: f.status, DispatchStatus: f.status, Rank: f.key, HandedOver: true, LastDispatchSeq: 1})
	seedLifecycle(t, pool, f.key)
	takeSlotAt(t, pool, f.key, f.admittedAt)
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if f.carried {
			if err := records.PutHandedOverRoot(ctx, tx, record.HandedOverRoot{Issue: f.key, Generation: f.generation}); err != nil {
				t.Fatalf("record %s as a root a hand-over created: %v", f.key, err)
			}
		}
		if f.registered {
			if err := records.PutGate(ctx, tx, record.DesignGate{Issue: f.key, ArtifactID: "artifact-" + f.key, LatestVersion: 1}); err != nil {
				t.Fatalf("record the design gate %s registered: %v", f.key, err)
			}
		}
	})
	if f.entry != 0 {
		putArchitectRow(t, pool, f.key, f.entry, f.raisedAt)
	}
	if !f.claimed {
		return
	}
	token := putWorkerClaim(t, pool, f.key, f.key, claim.RoleArchitect, f.generation, f.state, "")
	if f.given != 0 {
		markGivenEntry(t, pool, token, f.given)
	}
	if f.task {
		putArchitectTask(t, pool, token, f.generation, f.entry, f.queuedAt, f.confirmed)
	}
}

// takeSlotAt gives key one of the cap's slots, admitted at at, which is the moment the gate arm
// weighs its bound from. takeSlot is the same slot admitted at fixedNow.
func takeSlotAt(t *testing.T, pool *pgxpool.Pool, key string, at time.Time) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		held, err := records.Slots(context.Background(), tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: key, Index: len(held), AdmittedAt: at}); err != nil {
			t.Fatalf("put the slot of %s: %v", key, err)
		}
	})
}

// putArchitectTask records the task an architect's claim holds: the entry it is of, the moment
// it was queued and, where confirmed is set, the moment a turn of the claim confirmed it. Those
// three are the handshake the first arm reads (record.RoleClaim.Pending).
func putArchitectTask(t *testing.T, pool *pgxpool.Pool, token claim.Token, generation uint64, entry int64, queued time.Time, confirmed bool) {
	t.Helper()
	var at *time.Time
	if confirmed {
		took := queued.Add(time.Minute)
		at = &took
	}
	if _, err := pool.Exec(context.Background(), `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at,
		generation, phase, entry, confirmed_at) values ($1, 'outbox:21', 'Carry the tree on.', $2, $3, '', $4, $5)`,
		string(token), queued, int64(generation), entry, at); err != nil {
		t.Fatalf("record the task the architect claim %s holds: %v", token, err)
	}
}

// architectWakes is every architect-stalled controller notice of root in the outbox, oldest
// first, as the reason each carries.
func architectWakes(t *testing.T, pool *pgxpool.Pool, root string) []string {
	t.Helper()
	var reasons []string
	for _, row := range controllerNotices(t, pool) {
		if notice := row.payload.(record.ControllerNotice); row.issue == root && notice.Kind == record.ArchitectStalledNotice {
			reasons = append(reasons, notice.Reason)
		}
	}
	return reasons
}

// architectWakeMarks is every arm root is marked told for, at generation, in arm order.
func architectWakeMarks(t *testing.T, pool *pgxpool.Pool, root string, generation uint64) []record.ArchitectStallArm {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select arm from architect_stalled_notices
		where issue = $1 and tree = $1 and generation = $2 order by arm`, root, int64(generation))
	if err != nil {
		t.Fatalf("list the arms %s is marked told for: %v", root, err)
	}
	defer rows.Close()
	var arms []record.ArchitectStallArm
	for rows.Next() {
		var arm string
		if err := rows.Scan(&arm); err != nil {
			t.Fatalf("read an arm %s is marked told for: %v", root, err)
		}
		arms = append(arms, record.ArchitectStallArm(arm))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the arms %s is marked told for: %v", root, err)
	}
	return arms
}

// assertArchitectWakes reads root's wakes against the arms it must raise, in the order the pass
// raises them, each wake beginning with the words of the arm that raised it
// (record.ArchitectStallArm.Words).
func assertArchitectWakes(t *testing.T, pool *pgxpool.Pool, root string, arms []record.ArchitectStallArm, why string) {
	t.Helper()
	reasons := architectWakes(t, pool, root)
	if len(reasons) != len(arms) {
		t.Fatalf("the architect wakes of %s = %q, want one for each of %v: %s", root, reasons, arms, why)
	}
	for i, arm := range arms {
		if !strings.HasPrefix(reasons[i], arm.Words()+":") {
			t.Errorf("the wake %d of %s = %q, want it to begin with the words of the %s arm, %q: %s",
				i, root, reasons[i], arm, arm.Words(), why)
		}
		if !strings.Contains(reasons[i], root) {
			t.Errorf("the wake %d of %s = %q, want it to name the root it is about: %s", i, root, reasons[i], why)
		}
	}
}

// Each of the three arms wakes the project's controller once for a root and a run, however many
// passes read it. The pass runs on every fact of the project and on the sweep's own minute, so a
// wake decided by the reading alone would reach the controller again on each of them; the mark
// the arm writes is what keeps it to one (record.Store.TellArchitectsStalled).
//
// Each root is the base fixture — the one the reading raises nothing for — with the fields its
// own arm is about changed, so each wake turns on the thing its case names and on nothing else.
//
// The root with no architect claim at all is one the first pass also writes the start of its
// architect for, before it reads the arms: the wake stands, since a root with no claim that long
// is stalled whatever that pass writes, and its reason says the start was written in the same
// transaction rather than that no start waits in the queue, which after that pass is false.
func TestEachArchitectWakeArmTellsTheControllerOnceForARootAndRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		arm    record.ArchitectStallArm
		change func(*architectWakeFixture)
		why    string
		// says is a phrase the wake's reason must carry, and never one it must not, where the
		// case names one.
		says, never string
	}{
		{
			name: "the architect took nothing", arm: record.ArchitectTookNothing,
			change: func(f *architectWakeFixture) {
				f.queuedAt, f.confirmed = fixedNow.Add(-pastTheHandshake), false
			},
			why: "the task of its entry has stood queued to the claim past the bound with no turn confirming it, and the daemon writes no second start while it stands",
		},
		{
			name: "the architect's claim failed", arm: record.ArchitectClaimUnreachable,
			change: func(f *architectWakeFixture) {
				f.state, f.task = supervise.StateFailed, false
			},
			why: "a failed architect claim of a running tree is a claim workflow.Stranded names, and this arm is the wake for it",
		},
		{
			name: "the root holds no architect claim at all", arm: record.ArchitectClaimUnreachable,
			change: func(f *architectWakeFixture) {
				f.claimed, f.task = false, false
				f.raisedAt = fixedNow.Add(-pastTheHandshake)
			},
			why:   "the work of its entry has stood past the bound and no claim was ever created for it, and the first pass wrote the start of its architect again",
			says:  "the daemon wrote the start of its architect again in the same transaction as this wake",
			never: "no start of one waits in the queue",
		},
		{
			name: "the root is slow before its gate", arm: record.ArchitectSlowBeforeGate,
			change: func(f *architectWakeFixture) {
				f.admittedAt, f.raisedAt = fixedNow.Add(-pastTheGate), fixedNow.Add(-pastTheGate)
				f.queuedAt = fixedNow.Add(-pastTheGate)
			},
			why: "it has stood at admitted past the gate bound with no step of its design gate registered",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			const root = "LEGION-820"
			fixture := architectWakeRoot(root)
			tc.change(&fixture)
			seedArchitectWakeRoot(t, pool, fixture)

			for _, pass := range []string{"wake-1", "wake-2", "wake-3"} {
				applySweep(t, pool, admission, pass)
			}

			assertArchitectWakes(t, pool, root, []record.ArchitectStallArm{tc.arm}, tc.why)
			if marks := architectWakeMarks(t, pool, root, architectWakeGeneration); !slices.Equal(marks, []record.ArchitectStallArm{tc.arm}) {
				t.Fatalf("the arms %s is marked told for = %v, want the one %s: %s", root, marks, tc.arm, tc.why)
			}
			reason := architectWakes(t, pool, root)[0]
			if tc.says != "" && !strings.Contains(reason, tc.says) {
				t.Fatalf("the wake of %s = %q, want it to say %q: %s", root, reason, tc.says, tc.why)
			}
			if tc.never != "" && strings.Contains(reason, tc.never) {
				t.Fatalf("the wake of %s = %q, want it not to say %q: %s", root, reason, tc.never, tc.why)
			}
			if tc.says != "" {
				if starts := architectStartsOf(t, pool, root); len(starts) != 1 {
					t.Fatalf("the architect starts of %s = %+v, want the one the first pass wrote: the reason says it wrote one", root, starts)
				}
			}
		})
	}
}

// A root two arms read raises both, each once: its architect's claim failed, and that same claim
// holds the task of its entry queued past the bound with no turn confirming it. The two arms are
// marked apart (the arm is part of the mark's key), so neither case hides the other, and a
// second pass adds nothing.
func TestARootTwoArchitectWakeArmsReadRaisesBothOfThem(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	const root = "LEGION-821"
	fixture := architectWakeRoot(root)
	fixture.state, fixture.confirmed = supervise.StateFailed, false
	fixture.queuedAt = fixedNow.Add(-pastTheHandshake)
	seedArchitectWakeRoot(t, pool, fixture)

	applySweep(t, pool, admission, "two-arms-1")
	applySweep(t, pool, admission, "two-arms-2")

	assertArchitectWakes(t, pool, root, []record.ArchitectStallArm{record.ArchitectTookNothing, record.ArchitectClaimUnreachable},
		"the claim is failed and it holds the unconfirmed task of its entry, which are two facts and two wakes")
	marks := architectWakeMarks(t, pool, root, architectWakeGeneration)
	if !slices.Equal(marks, []record.ArchitectStallArm{record.ArchitectClaimUnreachable, record.ArchitectTookNothing}) {
		t.Fatalf("the arms %s is marked told for = %v, want both of them once", root, marks)
	}
}

// A root whose architect cannot hear raises the first arm not at all: nothing a controller does
// reaches a root a person has moved out of the workflow, so the wake would ask for an act with
// nowhere to land (workflow.ArchitectHears). Its claim holds exactly the shape the arm reads —
// the task of its entry, queued past the bound, unconfirmed — so the status is the whole of what
// quiets it.
func TestAnArchitectWakeIsNotRaisedForARootThatCannotHearItsEntry(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	const deaf, heard = "LEGION-822", "LEGION-823"
	for _, key := range []string{deaf, heard} {
		fixture := architectWakeRoot(key)
		fixture.queuedAt, fixture.confirmed = fixedNow.Add(-pastTheHandshake), false
		if key == deaf {
			fixture.status = "backlog"
		}
		seedArchitectWakeRoot(t, pool, fixture)
	}

	applySweep(t, pool, admission, "cannot-hear")

	if reasons := architectWakes(t, pool, deaf); len(reasons) != 0 {
		t.Fatalf("the architect wakes of the root in backlog = %q, want none: a root out of the workflow hears nothing a controller does", reasons)
	}
	assertArchitectWakes(t, pool, heard, []record.ArchitectStallArm{record.ArchitectTookNothing},
		"this root differs from the quiet one in its Dispatch status alone, so the status is what the first arm reads")
}

// The roots the reading leaves alone: one whose architect confirmed the task of its entry inside
// the bound, and one whose architect registered its design gate. Each is the base fixture with
// the one field its case names, so a root that went quiet because its fixture never stood up
// would prove nothing.
func TestNoArchitectWakeIsRaisedForARootThatIsMovingAlong(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*architectWakeFixture)
		why    string
	}{
		{
			name: "an architect that confirmed inside the bound",
			change: func(f *architectWakeFixture) {
				f.queuedAt = fixedNow.Add(-insideTheHandshake)
			},
			why: "a turn of the claim took the task, so the architect took what it was sent",
		},
		{
			name: "a gate registered inside the gate bound",
			change: func(f *architectWakeFixture) {
				f.registered = true
				f.admittedAt, f.raisedAt, f.queuedAt = fixedNow.Add(-pastTheGate), fixedNow.Add(-pastTheGate), fixedNow.Add(-pastTheGate)
			},
			why: "the gate arm reads a root with no step of its design gate registered, and this root registered one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			const root = "LEGION-824"
			fixture := architectWakeRoot(root)
			tc.change(&fixture)
			seedArchitectWakeRoot(t, pool, fixture)

			applySweep(t, pool, admission, "quiet-1")
			applySweep(t, pool, admission, "quiet-2")

			if reasons := architectWakes(t, pool, root); len(reasons) != 0 {
				t.Fatalf("the architect wakes of %s = %q, want none: %s", root, reasons, tc.why)
			}
		})
	}
}

// The claim workflow.Stranded names is the claim the second arm wakes the controller for, once.
// That report names a failed architect claim of a running tree and wakes nobody itself, and the
// two readings must agree: Stranded is asked here of the claims the store holds for the very
// fixture the pass then reads, so a change that moved one of them apart from the other fails this
// test.
func TestTheArchitectWakeOfAClaimStrandedNamesIsRaisedOnce(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	const root = "LEGION-825"
	fixture := architectWakeRoot(root)
	fixture.state, fixture.task = supervise.StateFailed, false
	seedArchitectWakeRoot(t, pool, fixture)
	token := architectToken(t, root)

	claims := storedClaims(t, pool)
	var named []supervise.Claim
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		named, err = workflow.Stranded(context.Background(), tx, record.NewStore(), testProject, nil, claims, nil, fixedNow)
		if err != nil {
			t.Fatalf("read the stranded claims: %v", err)
		}
	})
	if len(named) != 1 || named[0].Token != token {
		t.Fatalf("the stranded claims = %+v, want the failed architect claim of %s: the arm exists because that report wakes nobody", named, root)
	}

	applySweep(t, pool, admission, "stranded-1")
	applySweep(t, pool, admission, "stranded-2")

	assertArchitectWakes(t, pool, root, []record.ArchitectStallArm{record.ArchitectClaimUnreachable},
		"the claim Stranded names raises this arm, and the mark keeps it to one wake")
}

// A re-admitted root starts its own bound, and is told again. The mark is keyed by the tree's
// generation, so the run that was told says nothing about the run after it; the bound the gate
// arm reads runs from the re-admission's own slot and entry, so a root admitted again is quiet
// until it has stood its own 90 minutes.
func TestTheArchitectWakeBoundStartsAgainWhenARootIsReadmitted(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	const root = "LEGION-826"
	fixture := architectWakeRoot(root)
	fixture.generation = architectWakeGeneration
	fixture.admittedAt, fixture.raisedAt, fixture.queuedAt = fixedNow.Add(-pastTheGate), fixedNow.Add(-pastTheGate), fixedNow.Add(-pastTheGate)
	seedArchitectWakeRoot(t, pool, fixture)

	applySweep(t, pool, admission, "readmit-1")
	assertArchitectWakes(t, pool, root, []record.ArchitectStallArm{record.ArchitectSlowBeforeGate},
		"the root stood at admitted past the bound with nothing registered")

	// The re-admission: the next generation of the tree, a slot taken now and the entry its
	// promotion raised now, which is what the record carries after promoteHolds.
	readmitted := issue(t, pool, root)
	readmitted.Generation = architectWakeGeneration + 1
	putIssue(t, pool, readmitted)
	takeSlotAt(t, pool, root, fixedNow)
	putArchitectRow(t, pool, root, seededEntry+1, fixedNow)
	markGivenEntry(t, pool, architectToken(t, root), seededEntry+1)

	applySweep(t, pool, admission, "readmit-2")
	assertArchitectWakes(t, pool, root, []record.ArchitectStallArm{record.ArchitectSlowBeforeGate},
		"the re-admitted run is inside its own bound, so the first run's wake is still the only one")

	// The new run stands its own 90 minutes: the slot and the entry it was re-admitted with move
	// back past the bound, which is what the record reads after that time has passed.
	takeSlotAt(t, pool, root, fixedNow.Add(-pastTheGate))
	putArchitectRow(t, pool, root, seededEntry+1, fixedNow.Add(-pastTheGate))

	applySweep(t, pool, admission, "readmit-3")
	assertArchitectWakes(t, pool, root, []record.ArchitectStallArm{record.ArchitectSlowBeforeGate, record.ArchitectSlowBeforeGate},
		"the run after the re-admission is a run of its own, and it has now stood its own bound")
	if marks := architectWakeMarks(t, pool, root, architectWakeGeneration+1); len(marks) != 1 {
		t.Fatalf("the arms the re-admitted %s is marked told for = %v, want the gate arm once for the new run", root, marks)
	}
}

// A root a hand-over carried in raises the gate arm not at all. It stands in the phase that came
// with it, where its tree's work already runs under a phase worker, and that arm reads a root
// standing at admitted. The sweep's architect reach does serve such a root, so the quiet is the
// arm's own reading and not a root the pass never looked at.
func TestTheGateArchitectWakeIsNotRaisedForARootAHandoverCarriedIn(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	const root = "LEGION-827"
	fixture := architectWakeRoot(root)
	fixture.stands, fixture.carried, fixture.status = phase.Implementing, true, "in_progress"
	fixture.admittedAt, fixture.raisedAt, fixture.queuedAt = fixedNow.Add(-pastTheGate), fixedNow.Add(-pastTheGate), fixedNow.Add(-pastTheGate)
	seedArchitectWakeRoot(t, pool, fixture)

	applySweep(t, pool, admission, "handover-1")
	applySweep(t, pool, admission, "handover-2")

	if reasons := architectWakes(t, pool, root); len(reasons) != 0 {
		t.Fatalf("the architect wakes of the carried root = %q, want none: the gate arm reads a root standing at admitted", reasons)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		reached, err := workflow.ArchitectAtWork(context.Background(), tx, record.NewStore(), issue(t, pool, root))
		if err != nil || !reached {
			t.Fatalf("the architect reach over the carried root = %t, %v, want the root inside it: the quiet above must be the arm's own reading", reached, err)
		}
	})
}

// A root a person answered after a six hour wait raises the gate arm not at all within 90
// minutes of its resume, and once after that. Its admission is six hours old — the root keeps
// the slot it holds through the whole wait (record.KeepsSlot) — so a bound read from the
// admission alone would wake the controller the moment the answer landed. The bound runs from
// the later of the admission and the entry the resume raised, which is the issue's own decision.
func TestTheGateArchitectWakeReadsTheLaterOfTheAdmissionAndTheEntryRaised(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	const root = "LEGION-828"
	fixture := architectWakeRoot(root)
	fixture.admittedAt = fixedNow.Add(-6 * time.Hour)
	fixture.raisedAt, fixture.queuedAt = fixedNow.Add(-insideTheGate), fixedNow.Add(-insideTheGate)
	seedArchitectWakeRoot(t, pool, fixture)

	applySweep(t, pool, admission, "answered-1")
	if reasons := architectWakes(t, pool, root); len(reasons) != 0 {
		t.Fatalf("the architect wakes of the answered root = %q, want none: its resume raised the entry inside the bound, and that is the later moment", reasons)
	}

	// The resume's own 90 minutes pass: the entry the answer raised moves past the bound, and
	// the admission is where it was.
	putArchitectRow(t, pool, root, seededEntry, fixedNow.Add(-pastTheGate))

	applySweep(t, pool, admission, "answered-2")
	applySweep(t, pool, admission, "answered-3")
	assertArchitectWakes(t, pool, root, []record.ArchitectStallArm{record.ArchitectSlowBeforeGate},
		"the root has now stood its own 90 minutes since the work came back to it, and the wake is told once")
}

// The entry sweep's gate opens for an arm no wake has told yet, and the wake the sweep's pass
// then sends is what closes it. A root whose architect took nothing is owed nothing the pass
// sends — its task stands unconfirmed, so the pass writes no second start
// (workflow.ArchitectStartPending) — so a gate that asked only whether a role is owed its entry's
// work would answer no, and in a quiet project the controller would hear of the stall only at the
// next fact. A root beside it that is moving along raises nothing, so it holds the gate open
// neither before the wake nor after it.
func TestTheArchitectWakeOpensTheEntrySweepsGateUntilItIsTold(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	admission := quiet(t, 2)
	const stalled, moving = "LEGION-840", "LEGION-841"
	fixture := architectWakeRoot(stalled)
	fixture.queuedAt, fixture.confirmed = fixedNow.Add(-pastTheHandshake), false
	seedArchitectWakeRoot(t, pool, fixture)
	seedArchitectWakeRoot(t, pool, architectWakeRoot(moving))
	owes := func() bool {
		t.Helper()
		var owed bool
		if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
			var err error
			owed, err = admission.OwesEntryWork(ctx, tx)
			return err
		}); err != nil {
			t.Fatalf("ask the entry sweep's gate: %v", err)
		}
		return owed
	}

	if !owes() {
		t.Fatalf("the gate over %s, whose architect took nothing and whose arm no wake has told, answers no: the sweep would apply no fact for it", stalled)
	}
	applySweep(t, pool, admission, "gate-1")
	assertArchitectWakes(t, pool, stalled, []record.ArchitectStallArm{record.ArchitectTookNothing},
		"the pass the gate opened for reads the arm and tells the controller")
	if reasons := architectWakes(t, pool, moving); len(reasons) != 0 {
		t.Fatalf("the architect wakes of %s = %q, want none: its architect took the task it was sent", moving, reasons)
	}
	if owes() {
		t.Fatalf("the gate after the wake of %s was told answers yes: the sweep would apply a fact a minute for a stall already told", stalled)
	}
}
