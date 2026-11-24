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
	// awaiting is the wait of a root that stands in awaiting_person, nil for every other.
	awaiting *record.Awaiting
	// carried records a hand-over row at the root's generation, which is what says the root
	// stands in the run a hand-over carried it into (record.Store.HandedOverRoot).
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
	// one is a root whose architect claim does not exist. lastReady is the moment the claim's
	// agent last said it was ready (claims.last_ready), none where it is the zero time.
	claimed   bool
	state     supervise.ClaimState
	given     int64
	lastReady time.Time
	// task gives that claim the delivery of entry, queued at queuedAt, confirmed where confirmed
	// is set and marked as a task a let-go took back from a turn where interrupted is set: the
	// handshake the first arm reads. taskGeneration and taskEntry give that delivery a run and an
	// entry of their own, where they are not 0: the task of a run or an entry the row has moved
	// past. firstQueuedAt records the claim's handshake of the task's entry and run as first
	// queued then (claims.handshake_queued_at), none where it is the zero time.
	task           bool
	queuedAt       time.Time
	confirmed      bool
	interrupted    bool
	taskGeneration uint64
	taskEntry      int64
	firstQueuedAt  time.Time
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
		Project: testProject, Title: "the root " + f.key, Phase: f.stands, Generation: f.generation, Awaiting: f.awaiting,
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
	if !f.lastReady.IsZero() {
		if _, err := pool.Exec(ctx, `update claims set last_ready = $2 where token = $1`, string(token), f.lastReady); err != nil {
			t.Fatalf("record when the agent of the claim %s was last ready: %v", token, err)
		}
	}
	if f.task {
		generation, entry := f.generation, f.entry
		if f.taskGeneration != 0 {
			generation = f.taskGeneration
		}
		if f.taskEntry != 0 {
			entry = f.taskEntry
		}
		putArchitectTask(t, pool, token, generation, entry, f.queuedAt, f.confirmed, f.interrupted)
		if !f.firstQueuedAt.IsZero() {
			putArchitectHandshake(t, pool, token, entry, generation, f.firstQueuedAt, time.Time{})
		}
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
// it was queued, where confirmed is set the moment a turn of the claim confirmed it, and where
// interrupted is set the mark a let-go's take-back leaves on it (supervise.Delivery.Interrupted).
// Those are the handshake the first arm reads (record.RoleClaim.Pending).
func putArchitectTask(t *testing.T, pool *pgxpool.Pool, token claim.Token, generation uint64, entry int64, queued time.Time,
	confirmed, interrupted bool,
) {
	t.Helper()
	var at *time.Time
	if confirmed {
		took := queued.Add(time.Minute)
		at = &took
	}
	if _, err := pool.Exec(context.Background(), `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at,
		generation, phase, entry, confirmed_at, interrupted) values ($1, 'outbox:21', 'Carry the tree on.', $2, $3, '', $4, $5, $6)`,
		string(token), queued, int64(generation), entry, at, interrupted); err != nil {
		t.Fatalf("record the task the architect claim %s holds: %v", token, err)
	}
}

// putArchitectHandshake records the claim's handshake of entry, of generation's run, as the claim's
// machine keeps it (record.RoleClaim.Handshake): a task of that entry first queued at queued, and
// first confirmed by a turn at confirmed, none where confirmed is the zero time. The first arm
// counts the entry's task from the queued moment, and the gate arm counts its bound from no
// earlier than the confirmed one where there is one.
func putArchitectHandshake(t *testing.T, pool *pgxpool.Pool, token claim.Token, entry int64, generation uint64, queued, confirmed time.Time) {
	t.Helper()
	var at *time.Time
	if !confirmed.IsZero() {
		at = &confirmed
	}
	if _, err := pool.Exec(context.Background(), `update claims set handshake_entry = $2, handshake_generation = $3, handshake_queued_at = $4,
		handshake_confirmed_at = $5 where token = $1`, string(token), entry, int64(generation), queued, at); err != nil {
		t.Fatalf("record the handshake of entry %d of the claim %s as first queued at %s and confirmed at %v: %v",
			entry, token, queued, at, err)
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
			why: "a failed architect claim of a running tree with no start of its run queued is a claim no start will reach, and this arm is the wake for it",
		},
		{
			name: "the root holds no architect claim at all", arm: record.ArchitectClaimUnreachable,
			change: func(f *architectWakeFixture) {
				f.claimed, f.task = false, false
				f.raisedAt = fixedNow.Add(-pastTheHandshake)
			},
			why:   "the work of its entry has stood past the bound and no claim was ever created for it, and the first pass wrote the start of its architect again",
			says:  "the daemon wrote the start of its architect again in the same transaction as this wake",
			never: "no start of one queued in the last",
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
			registerController(t, pool)
			admission := quiet(t, 1)
			const root = "LEGION-820"
			fixture := architectWakeRoot(root)
			tc.change(&fixture)
			seedArchitectWakeRoot(t, pool, fixture)

			// The reading the state serves names the arm before any pass has told anybody of it,
			// by the predicates the wake below is told from (ArchitectStallArms).
			if arms := architectStallArmsOf(t, pool, admission, root); !slices.Equal(arms, []record.ArchitectStallArm{tc.arm}) {
				t.Fatalf("the state's reading of %s names the arms %v before any pass, want the one %s: %s", root, arms, tc.arm, tc.why)
			}
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
	registerController(t, pool)
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

// A claim that still holds a task its root's own record has superseded raises no took-nothing
// wake, and the pass that reads it writes the start of the entry the architect's row names
// (dispatch://INFRA-2614 R4, R5, R7). Such a task is of a run before the root's generation, or of
// the root's run at an entry below the row's: the work of a run re-admission ended, or of an entry
// the row has moved past. It is no start on its way, so the sweep passes over it
// (workflow.ArchitectStartPending) and the start it writes takes the task's place in the claim's
// hands (supervise.Delivery.SupersededBy). Nor is it the task of the row's entry, R7's "its
// entry's task", so the first arm reads nothing of it. The arms are read after the pass's sends,
// so an arm that read the task the claim was given would tell the controller, in the pass that
// writes the start, that the daemon writes no second start. Each task here was queued past the
// bound and no turn confirmed it:
//
//   - a root at admitted, re-admitted at generation 2, its row at the entry the re-admission's
//     promotion raised, whose claim was given the entry before and still holds that entry's task
//     of generation 1: the start of the row's entry is written, derived again from the record;
//   - a root in planning whose row stands at an entry a resume raised, whose claim still holds
//     the task of the entry before, of the same run: the start of the resume's entry is written,
//     carrying the resume's own text;
//   - the control, a root whose claim holds the task of the row's own entry, which it was given:
//     the sweep writes nothing for it, and the arm names it;
//   - a root in planning whose row stands at an entry the derivation raised: the reach leaves it
//     out past admitted, so nothing is written and nothing is named.
func TestATaskItsRowSupersededRaisesNoTookNothingWakeAndTheRowsEntryIsSent(t *testing.T) {
	const resumed = "A person answered your ask ask-q; read it."
	for _, tc := range []struct {
		name   string
		change func(*architectWakeFixture)
		// resume raises the architect's row to entry seededEntry+1 carrying the resume's text,
		// marked to be sent as written where verbatim is set.
		resume, verbatim bool
		want             []record.ArchitectStallArm
		// sent is the entry of the one start the passes write, 0 for none, and task the text it
		// carries where the case names one.
		sent int64
		task string
		why  string
	}{
		{
			name: "the task of the run before the root's re-admission",
			change: func(f *architectWakeFixture) {
				f.entry, f.given = seededEntry+1, seededEntry
				f.taskGeneration, f.taskEntry = architectWakeGeneration-1, seededEntry
			},
			sent: seededEntry + 1,
			why: "re-admission ended the run that task is of, so the start of the row's entry takes its place, and the claim was " +
				"never queued the row's entry to take",
		},
		{
			name: "the task of the entry below a resume's entry", resume: true, verbatim: true,
			change: func(f *architectWakeFixture) { f.stands = phase.Planning },
			sent:   seededEntry + 1, task: resumed,
			why: "the resume's entry keeps the root in the reach past admitted until its claim is given it, and the task of the " +
				"entry before is one that entry has moved past",
		},
		{
			name: "the task of the row's own entry", change: func(*architectWakeFixture) {},
			want: []record.ArchitectStallArm{record.ArchitectTookNothing},
			why:  "the claim was given the row's entry and took nothing of it, so the sweep writes nothing and the wake is the one thing left",
		},
		{
			name: "a root past admitted whose row's entry the derivation raised", resume: true,
			change: func(f *architectWakeFixture) { f.stands = phase.Planning },
			why: "a root past admitted under its own power is outside the reach when no resume's entry is owed, so no task of it " +
				"was the sweep's to send",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			registerController(t, pool)
			admission := quiet(t, 1)
			const root = "LEGION-826"
			fixture := architectWakeRoot(root)
			fixture.queuedAt, fixture.confirmed = fixedNow.Add(-pastTheHandshake), false
			tc.change(&fixture)
			seedArchitectWakeRoot(t, pool, fixture)
			if tc.resume {
				inTx(t, pool, func(tx pgx.Tx) {
					if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: root, Role: claim.RoleArchitect,
						Claim: architectToken(t, root), Entry: seededEntry + 1, EntryTask: resumed,
						EntryVerbatim: tc.verbatim, EntryRaisedAt: fixedNow.Add(-pastTheHandshake)}); err != nil {
						t.Fatalf("raise the architect's entry of %s above the entry its claim was given: %v", root, err)
					}
				})
			}

			if arms := architectStallArmsOf(t, pool, admission, root); !slices.Equal(arms, tc.want) {
				t.Fatalf("the state's reading of %s names the arms %v, want %v: %s", root, arms, tc.want, tc.why)
			}
			applySweep(t, pool, admission, "superseded-1")
			applySweep(t, pool, admission, "superseded-2")
			assertArchitectWakes(t, pool, root, tc.want, tc.why)

			starts := architectStartsOf(t, pool, root)
			if tc.sent == 0 {
				if len(starts) != 0 {
					t.Fatalf("the architect starts of %s = %+v, want none: %s", root, starts, tc.why)
				}
				return
			}
			if len(starts) != 1 {
				t.Fatalf("the architect starts of %s after two passes = %+v, want the one start of entry %d: %s", root, starts, tc.sent, tc.why)
			}
			start := starts[0]
			if start.Entry != tc.sent || start.Generation != architectWakeGeneration || start.Task == "" ||
				(tc.task != "" && start.Task != tc.task) {
				t.Fatalf("the architect start of %s = entry %d of generation %d carrying %q, want the work of entry %d of generation %d: %s",
					root, start.Entry, start.Generation, start.Task, tc.sent, architectWakeGeneration, tc.why)
			}
		})
	}
}

// A root whose architect cannot hear raises the first arm not at all: nothing a controller does
// reaches a root a person has moved out of the workflow, so the wake would ask for an act with
// nowhere to land (workflow.ArchitectHears). Its claim holds exactly the shape the arm reads —
// the task of its entry, queued past the bound, unconfirmed — so the status is the whole of what
// quiets it.
func TestAnArchitectWakeIsNotRaisedForARootThatCannotHearItsEntry(t *testing.T) {
	pool := migratedPool(t)
	registerController(t, pool)
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
			registerController(t, pool)
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

// The first arm counts a task a let-go took back from a turn (supervise.Delivery.Interrupted) only
// while the claim's agent can take a prompt, and from the later of the task's queueing and that
// agent's last ready (record.RoleClaim.LastReady). Every relaunch takes the claim's task back so:
// the task keeps the moment it was queued, loses its confirmation and waits for the relaunched
// agent. Each case is a root whose architect's claim holds the task of the entry it was given,
// queued forty minutes ago, unconfirmed and taken back:
//   - the claim launching: the relaunch has brought up no agent that could take the task, and a
//     turn of the architect did confirm it before the let-go, so no arm holds and the pass writes
//     no wake and no mark;
//   - the claim ready, its agent ready eleven minutes ago: the relaunched agent has been able to
//     take the task for more than the bound and no turn has confirmed it, so the arm holds and
//     the controller is woken;
//   - the claim ready, its agent ready nine minutes ago: that is inside the bound, so no arm holds.
func TestTheTookNothingArchitectWakeCountsATakenBackTaskFromTheRelaunchedAgentsReady(t *testing.T) {
	queued := fixedNow.Add(-40 * time.Minute)
	for _, tc := range []struct {
		name      string
		state     supervise.ClaimState
		lastReady time.Time
		want      []record.ArchitectStallArm
		why       string
	}{
		{
			name: "a claim the relaunch is still launching", state: supervise.StateLaunching, lastReady: queued.Add(-time.Minute),
			why: "the architect worked the task until the let-go, and the relaunched agent cannot take it yet",
		},
		{
			name: "a relaunched claim whose agent was ready past the bound", state: supervise.StateReady,
			lastReady: fixedNow.Add(-pastTheHandshake), want: []record.ArchitectStallArm{record.ArchitectTookNothing},
			why: "the relaunched agent could take the task for longer than the bound and no turn confirmed it",
		},
		{
			name: "a relaunched claim whose agent was ready inside the bound", state: supervise.StateReady,
			lastReady: fixedNow.Add(-insideTheHandshake),
			why:       "the relaunched agent has been able to take the task for less than the bound",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			registerController(t, pool)
			admission := quiet(t, 1)
			const root = "LEGION-829"
			fixture := architectWakeRoot(root)
			fixture.state, fixture.lastReady = tc.state, tc.lastReady
			fixture.raisedAt, fixture.queuedAt, fixture.confirmed, fixture.interrupted = queued, queued, false, true
			seedArchitectWakeRoot(t, pool, fixture)

			if arms := architectStallArmsOf(t, pool, admission, root); !slices.Equal(arms, tc.want) {
				t.Fatalf("the state's reading of %s names the arms %v, want %v: %s", root, arms, tc.want, tc.why)
			}
			applySweep(t, pool, admission, "taken-back-1")
			applySweep(t, pool, admission, "taken-back-2")

			assertArchitectWakes(t, pool, root, tc.want, tc.why)
			if marks := architectWakeMarks(t, pool, root, architectWakeGeneration); !slices.Equal(marks, tc.want) {
				t.Fatalf("the arms %s is marked told for = %v, want %v: %s", root, marks, tc.want, tc.why)
			}
			if len(tc.want) == 0 {
				return
			}
			if ready := tc.lastReady.UTC().Format(time.RFC3339); !strings.Contains(architectWakes(t, pool, root)[0], ready) {
				t.Fatalf("the wake of %s = %q, want it to name %s, the relaunched agent's ready the arm counted from",
					root, architectWakes(t, pool, root)[0], ready)
			}
		})
	}
}

// The first arm counts the task of an entry from when a task of that entry was first queued to the
// claim, as R7 says ("its entry's task was first queued more than 10 minutes ago",
// dispatch://INFRA-2614), which the claim's own handshake keeps (supervise.Claim.Handshake,
// record.RoleClaim.Handshake), and not from the queueing of the delivery the claim holds now. A
// task retired unconfirmed puts its entry back, and the sweep sends that entry again as a new
// delivery queued at that send, while the handshake keeps the first queueing through it. Each case
// is a root whose claim holds such a task of the entry its row names, queued again five minutes
// ago and confirmed by no turn, beside the handshake of that entry:
//   - first queued fifteen minutes ago: past the bound, so the arm names it, and its reason gives
//     that first moment;
//   - first queued nine minutes ago: inside the bound, so nothing is named.
func TestTheTookNothingArchitectWakeCountsFromTheEntrysFirstQueueing(t *testing.T) {
	resent := fixedNow.Add(-5 * time.Minute)
	for _, tc := range []struct {
		name  string
		first time.Time
		want  []record.ArchitectStallArm
		why   string
	}{
		{
			name: "an entry first queued fifteen minutes ago", first: fixedNow.Add(-15 * time.Minute),
			want: []record.ArchitectStallArm{record.ArchitectTookNothing},
			why:  "the entry's task was first queued past the bound and no turn has confirmed a task of it since",
		},
		{
			name: "an entry first queued nine minutes ago", first: fixedNow.Add(-9 * time.Minute),
			why: "the entry's task was first queued inside the bound",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			registerController(t, pool)
			admission := quiet(t, 1)
			const root = "LEGION-830"
			fixture := architectWakeRoot(root)
			fixture.queuedAt, fixture.confirmed, fixture.firstQueuedAt = resent, false, tc.first
			seedArchitectWakeRoot(t, pool, fixture)

			if arms := architectStallArmsOf(t, pool, admission, root); !slices.Equal(arms, tc.want) {
				t.Fatalf("the state's reading of %s names the arms %v, want %v: %s", root, arms, tc.want, tc.why)
			}
			applySweep(t, pool, admission, "first-queued-1")
			applySweep(t, pool, admission, "first-queued-2")
			assertArchitectWakes(t, pool, root, tc.want, tc.why)
			if starts := architectStartsOf(t, pool, root); len(starts) != 0 {
				t.Fatalf("the architect starts of %s = %+v, want none: its claim was given the entry its row names", root, starts)
			}
			if len(tc.want) == 0 {
				return
			}
			reason := architectWakes(t, pool, root)[0]
			if first := tc.first.UTC().Format(time.RFC3339); !strings.Contains(reason, "first queued to the architect's claim of "+root+" at "+first) {
				t.Fatalf("the wake of %s = %q, want it to name %s, when the entry's task was first queued", root, reason, first)
			}
		})
	}
}

// The second arm wakes the controller for a failed or a retired architect claim only while no
// start of the root's run is the daemon's next move for it (workflow.ArchitectRelaunchOf): a start
// a lane holds under a live lease is, and so is a start queued in the last ten minutes unless it
// has already run against the claim and a run of it ended without success. A lapsed lease, which a
// lane that died mid-run leaves on the row until the outbox's sweep runs the row again, holds
// nothing, as the store's own due predicate reads it (record.QueuedSupervise.Running). A queued
// start is the daemon's own next move: the outbox's start of a failed or a retired claim
// relaunches it, on its kept session, or a new one once supervise's Machine.forgetSession has
// dropped that (an architect's start asks for no new session), and a controller woken now would
// take `fresh_session`, which discards that session. Every re-admission of a closed tree makes this
// shape: the linger's end retires the architect's claim, and the promotion queues the architect's
// start in the same pass that reads the arms. A start in its run against the claim is still the
// next move: the outbox records the start on the claim (claims.last_start_row) before it
// relaunches the claim, so a pass that reads the record in that moment reads the claim failed or
// retired beside the start that is relaunching it, whatever an earlier run of that start that
// waited added to its attempts.
//
// workflow.Stranded is asked here too, of the claims the store holds for the very fixture the
// pass then reads, and the two readings answer different questions. Stranded asks whether nothing
// will start the claim again, and a start that has run against it and left it failed will, since
// the outbox runs it again on its backoff. The arm asks whether a start can still cure the claim,
// and that start cannot. So on these cases the two agree on every one but the failed claims whose
// own start has run against them and left them failed, under no lease or a lapsed one, which the
// arm wakes the controller for and Stranded leaves out. A start of an earlier run reaches no claim
// of this one, so it quiets neither.
//
// A root waiting on a person is in the workflow, so a failed or a retired claim of it with no
// start queued is a claim the arm names, and the reason says the root waits: the daemon queues
// the next start of its architect only when that wait is answered
// (workflow.Engine.ResumeAwaitedRoot). Stranded leaves such a root out, which is a different
// question again: the answer will start the claim, so something will.
func TestTheUnreachableClaimArchitectWakeIsRaisedOnlyWhereNoQueuedStartRelaunchesTheClaim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
		// queued is the generation of the architect start the outbox holds for the root when the
		// pass begins, and 0 for no start at all.
		queued uint64
		// ran records that start on the claim as the newest start run against it
		// (claims.last_start_row), and attempts as the runs of it the outbox finished, each one that
		// ended without success, a failure or a wait (record.Store.RetryOutbox).
		ran      bool
		attempts int
		// lease gives that start a lane's lease ending this long after now: a positive one is a
		// lane running it now, and a negative one is the lease a lane that died mid-run leaves,
		// already past, which holds nothing. 0 gives it no lease.
		lease time.Duration
		// awaiting puts the root in awaiting_person on that wait, whose answer resumes its
		// architect with a start of its own (workflow.Engine.ResumeAwaitedRoot).
		awaiting *record.Awaiting
		woken    bool
		// says is what the wake's reason states of the queue, where the arm wakes the controller.
		says     string
		stranded bool
		why      string
	}{
		{name: "a retired claim with a start of its run queued", state: supervise.StateRetired, queued: architectWakeGeneration,
			why: "the queued start relaunches the retired claim on its kept session"},
		{name: "a retired claim with no start queued", state: supervise.StateRetired, woken: true, stranded: true,
			says: "no start of its run queued in the last 10m0s waits to relaunch it",
			why:  "nothing the daemon has queued will relaunch the retired claim"},
		{name: "a retired claim with a start of an earlier run queued", state: supervise.StateRetired, queued: architectWakeGeneration - 1,
			woken: true, stranded: true, says: "no start of its run queued in the last 10m0s waits to relaunch it",
			why: "a start of the run before reaches no claim of this run"},
		{name: "a failed claim with a start of its run queued", state: supervise.StateFailed, queued: architectWakeGeneration,
			why: "the queued start relaunches the failed claim on its kept session"},
		{name: "a failed claim with no start queued", state: supervise.StateFailed, woken: true, stranded: true,
			says: "no start of its run queued in the last 10m0s waits to relaunch it",
			why:  "nothing the daemon has queued will relaunch the failed claim"},
		{name: "a retired claim of a root waiting on a person", state: supervise.StateRetired,
			awaiting: &record.Awaiting{Ask: "ask-q", Since: seqMark(10)}, woken: true,
			says: "The root waits on a person, and the daemon queues the next start of its architect when that wait is answered.",
			why: "the root is in the workflow and no start the daemon has queued relaunches the retired claim, while workflow.Stranded " +
				"leaves the claim out because the answer to the wait will start it"},
		{name: "a failed claim of a root waiting on a person", state: supervise.StateFailed,
			awaiting: &record.Awaiting{Ask: "ask-q", Since: seqMark(10)}, woken: true,
			says: "The root waits on a person, and the daemon queues the next start of its architect when that wait is answered.",
			why: "the root is in the workflow and no start the daemon has queued relaunches the failed claim, while workflow.Stranded " +
				"leaves the claim out because the answer to the wait will start it"},
		{name: "a retired claim whose start is in its first run against it", state: supervise.StateRetired,
			queued: architectWakeGeneration, ran: true, lease: time.Minute,
			why: "the outbox records the start on the claim before it relaunches the claim, so that start is still the daemon's next move"},
		{name: "a failed claim whose start is in its first run against it", state: supervise.StateFailed,
			queued: architectWakeGeneration, ran: true, lease: time.Minute,
			why: "the outbox records the start on the claim before it relaunches the claim, so that start is still the daemon's next move"},
		{name: "a failed claim whose start no run has ended yet, though it stands as the claim's last start", state: supervise.StateFailed,
			queued: architectWakeGeneration, ran: true,
			why: "no run of the start has ended without success, so nothing says it failed against the claim, and it was queued inside the bound"},
		{name: "a retired claim whose start the outbox is running now, after an earlier run of it waited", state: supervise.StateRetired,
			queued: architectWakeGeneration, ran: true, attempts: 1, lease: time.Minute,
			why: "a lane holds the start under a live lease, so the outbox is relaunching the claim now, whatever the earlier run that waited added to its attempts"},
		{name: "a failed claim whose own start has run against it and left it failed", state: supervise.StateFailed,
			queued: architectWakeGeneration, ran: true, attempts: 3, woken: true,
			says: "The start of its run has run against it and left it failed",
			why: "the outbox only runs that start again on its backoff, which cures nothing, so the arm wakes the controller, " +
				"while Stranded leaves the claim out because that start will run again"},
		{name: "a failed claim whose own start left it failed, under the lapsed lease of a lane that died", state: supervise.StateFailed,
			queued: architectWakeGeneration, ran: true, attempts: 3, lease: -5 * time.Minute, woken: true,
			says: "The start of its run has run against it and left it failed",
			why: "a lapsed lease holds nothing, as the outbox's own due predicate reads it, so that start is one the outbox only runs " +
				"again on its backoff, which cures nothing; Stranded leaves the claim out because that start will run again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := migratedPool(t)
			registerController(t, pool)
			admission := quiet(t, 1)
			const root = "LEGION-825"
			fixture := architectWakeRoot(root)
			fixture.state, fixture.task = tc.state, false
			if tc.awaiting != nil {
				fixture.stands, fixture.awaiting = phase.AwaitingPerson, tc.awaiting
			}
			seedArchitectWakeRoot(t, pool, fixture)
			token := architectToken(t, root)
			if tc.queued != 0 {
				inTx(t, pool, func(tx pgx.Tx) {
					row, err := record.NewOutboxRow(root, record.SuperviseRequest{Op: "start", Tree: root, Role: claim.RoleArchitect,
						Generation: tc.queued, Task: seededArchitectTask(root), Entry: seededEntry}, fixedNow)
					if err != nil {
						t.Fatalf("build the architect start of %s at generation %d: %v", root, tc.queued, err)
					}
					if err := record.NewStore().Enqueue(ctx, tx, row); err != nil {
						t.Fatalf("queue the architect start of %s at generation %d: %v", root, tc.queued, err)
					}
					if !tc.ran {
						return
					}
					var startRow int64
					if err := tx.QueryRow(ctx, `select max(id) from outbox where issue = $1 and kind = 'supervise'`, root).Scan(&startRow); err != nil {
						t.Fatalf("read the start row of %s: %v", root, err)
					}
					if _, err := tx.Exec(ctx, `update outbox set attempts = $1 where id = $2`, tc.attempts, startRow); err != nil {
						t.Fatalf("record %d unsuccessful runs of start row %d: %v", tc.attempts, startRow, err)
					}
					if tc.lease != 0 {
						if _, err := tx.Exec(ctx, `update outbox set lease_token = 'lane', lease_until = now() + make_interval(secs => $2) where id = $1`,
							startRow, tc.lease.Seconds()); err != nil {
							t.Fatalf("lease start row %d to a lane until %s from now: %v", startRow, tc.lease, err)
						}
					}
					if _, err := tx.Exec(ctx, `update claims set last_start_row = $1 where token = $2`, startRow, string(token)); err != nil {
						t.Fatalf("record start row %d as the last start that ran against %s: %v", startRow, token, err)
					}
				})
			}

			claims := storedClaims(t, pool)
			var named []supervise.Claim
			inTx(t, pool, func(tx pgx.Tx) {
				var err error
				named, err = workflow.Stranded(ctx, tx, record.NewStore(), testProject, nil, claims, nil, fixedNow)
				if err != nil {
					t.Fatalf("read the stranded claims: %v", err)
				}
			})
			stranded := len(named) == 1 && named[0].Token == token
			if stranded != tc.stranded || (!stranded && len(named) != 0) {
				t.Fatalf("the stranded claims = %+v, want the %s architect claim of %s named = %t: %s", named, tc.state, root, tc.stranded, tc.why)
			}
			var want []record.ArchitectStallArm
			if tc.woken {
				want = []record.ArchitectStallArm{record.ArchitectClaimUnreachable}
			}
			if arms := architectStallArmsOf(t, pool, admission, root); !slices.Equal(arms, want) {
				t.Fatalf("the state's reading of %s names the arms %v, want %v: %s", root, arms, want, tc.why)
			}

			applySweep(t, pool, admission, "unreachable-1")
			if !tc.woken {
				if reasons := architectWakes(t, pool, root); len(reasons) != 0 {
					t.Fatalf("the architect wakes of %s = %q, want none in the pass that reads the queued start: %s", root, reasons, tc.why)
				}
				if marks := architectWakeMarks(t, pool, root, architectWakeGeneration); len(marks) != 0 {
					t.Fatalf("the arms %s is marked told for = %v, want none: a mark spent now would keep a later failure of the "+
						"relaunched claim from waking anybody in this run", root, marks)
				}
				return
			}
			applySweep(t, pool, admission, "unreachable-2")
			assertArchitectWakes(t, pool, root, []record.ArchitectStallArm{record.ArchitectClaimUnreachable}, tc.why)
			if reasons := architectWakes(t, pool, root); !strings.Contains(reasons[0], tc.says) {
				t.Errorf("the wake of %s = %q, want it to say %q: %s", root, reasons[0], tc.says, tc.why)
			}
		})
	}
}

// A queued start no lane holds that has not run against the claim is the daemon's next move only
// for the bound the second arm reads (workflow.StrandedReleaseWake, ten minutes): a start of the
// run that has stood owed longer than that is no next move, and the arm names the failed claim,
// once (the architect's reading of R7 at dispatch://INFRA-2614). The control is the same start
// queued inside the bound, which the arm waits for. Each row is dated by its age in the store's own
// clock, the clock the arm reads it in (record.QueuedSupervise.Age), a minute either side of ten
// minutes, so a bound read wrong in either direction fails one of the two cases. The age is read
// from the outbox row's created_at, which the outbox keeps when it runs a row again in place; the
// start that has already run against the claim and left it failed is
// TestTheUnreachableClaimArchitectWakeIsRaisedForAClaimItsOwnStartKeepsFailingToLaunch's case.
func TestTheUnreachableClaimArchitectWakeIsRaisedForAStartQueuedPastTheBoundThatNeverRan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		age   time.Duration
		woken bool
	}{
		{name: "queued 11 minutes ago", age: 11 * time.Minute, woken: true},
		{name: "queued 9 minutes ago", age: 9 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := migratedPool(t)
			registerController(t, pool)
			admission := quiet(t, 1)
			const root = "LEGION-827"
			fixture := architectWakeRoot(root)
			fixture.state, fixture.task = supervise.StateFailed, false
			seedArchitectWakeRoot(t, pool, fixture)
			inTx(t, pool, func(tx pgx.Tx) {
				row, err := record.NewOutboxRow(root, record.SuperviseRequest{Op: "start", Tree: root, Role: claim.RoleArchitect,
					Generation: architectWakeGeneration, Task: seededArchitectTask(root), Entry: seededEntry}, fixedNow)
				if err != nil {
					t.Fatalf("build the architect start of %s: %v", root, err)
				}
				if err := record.NewStore().Enqueue(ctx, tx, row); err != nil {
					t.Fatalf("queue the architect start of %s: %v", root, err)
				}
				if _, err := tx.Exec(ctx, `update outbox set created_at = now() - make_interval(secs => $1) where issue = $2 and kind = 'supervise'`,
					tc.age.Seconds(), root); err != nil {
					t.Fatalf("date the architect start of %s %s before now: %v", root, tc.age, err)
				}
			})

			var want []record.ArchitectStallArm
			if tc.woken {
				want = []record.ArchitectStallArm{record.ArchitectClaimUnreachable}
			}
			if arms := architectStallArmsOf(t, pool, admission, root); !slices.Equal(arms, want) {
				t.Fatalf("the state's reading of %s names the arms %v with its start %s, want %v", root, arms, tc.name, want)
			}
			applySweep(t, pool, admission, "owed-1")
			applySweep(t, pool, admission, "owed-2")
			if !tc.woken {
				if reasons := architectWakes(t, pool, root); len(reasons) != 0 {
					t.Fatalf("the architect wakes of %s = %q, want none while a start queued inside the bound has still to run", root, reasons)
				}
				return
			}
			assertArchitectWakes(t, pool, root, want,
				"a start that has stood owed past the bound without running is no next move, so the failed claim is the arm's case")
		})
	}
}

// A re-admitted root starts its own bound, and is told again. The mark is keyed by the tree's
// generation, so the run that was told says nothing about the run after it; the bound the gate
// arm reads runs from the re-admission's own slot and entry, so a root admitted again is quiet
// until it has stood its own 90 minutes.
func TestTheArchitectWakeBoundStartsAgainWhenARootIsReadmitted(t *testing.T) {
	pool := migratedPool(t)
	registerController(t, pool)
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

// A root a hand-over carried in raises the gate arm not at all. It stands in a phase of the run
// that carried it, where its tree's work already runs under a phase worker, and that arm reads a
// root standing at admitted. The sweep's architect reach does serve such a root, so the quiet is
// the arm's own reading and not a root the pass never looked at.
func TestTheGateArchitectWakeIsNotRaisedForARootAHandoverCarriedIn(t *testing.T) {
	pool := migratedPool(t)
	registerController(t, pool)
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
		reached, err := workflow.ArchitectAtWork(context.Background(), tx, record.NewStore(), issue(t, pool, root),
			record.PhaseEntry{}, record.RoleRun{})
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
	registerController(t, pool)
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
// sends — its claim was given the entry its row names and holds that entry's task unconfirmed, so
// the pass writes no second start (workflow.OwedEntry) — so a gate that asked only whether a role
// is owed its entry's work would answer no, and in a quiet project the controller would hear of
// the stall only at the next fact. A root beside it that is moving along raises nothing, so it
// holds the gate open neither before the wake nor after it.
func TestTheArchitectWakeOpensTheEntrySweepsGateUntilItIsTold(t *testing.T) {
	pool := migratedPool(t)
	registerController(t, pool)
	admission := quiet(t, 2)
	const stalled, moving = "LEGION-840", "LEGION-841"
	fixture := architectWakeRoot(stalled)
	fixture.queuedAt, fixture.confirmed = fixedNow.Add(-pastTheHandshake), false
	seedArchitectWakeRoot(t, pool, fixture)
	seedArchitectWakeRoot(t, pool, architectWakeRoot(moving))

	if !entrySweepGateOpens(t, pool, admission) {
		t.Fatalf("the gate over %s, whose architect took nothing and whose arm no wake has told, answers no: the sweep would apply no fact for it", stalled)
	}
	applySweep(t, pool, admission, "gate-1")
	assertArchitectWakes(t, pool, stalled, []record.ArchitectStallArm{record.ArchitectTookNothing},
		"the pass the gate opened for reads the arm and tells the controller")
	if reasons := architectWakes(t, pool, moving); len(reasons) != 0 {
		t.Fatalf("the architect wakes of %s = %q, want none: its architect took the task it was sent", moving, reasons)
	}
	if entrySweepGateOpens(t, pool, admission) {
		t.Fatalf("the gate after the wake of %s was told answers yes: the sweep would apply a fact a minute for a stall already told", stalled)
	}
}

// An arm read while no controller is registered is marked for nobody and queued for nobody, as
// every wake of the daemon that keeps a mark is (admit's wakeController, tellStalledTrees). A
// controller that registers later reads its notices from the end of the stream, so a wake queued
// before it would reach nobody, and a mark written with it would keep the arm from being told
// again in that run. So the pass leaves the stall as it found it, and the sweep's gate answers no
// for a stall alone while nobody would hear of it: a gate that answered yes would apply an entry
// sweep's fact every minute for as long as no controller runs. The first pass after a controller
// registers tells it, once.
func TestAnArchitectWakeWaitsForARegisteredController(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	const root = "LEGION-842"
	fixture := architectWakeRoot(root)
	fixture.queuedAt, fixture.confirmed = fixedNow.Add(-pastTheHandshake), false
	seedArchitectWakeRoot(t, pool, fixture)

	if entrySweepGateOpens(t, pool, admission) {
		t.Fatalf("the gate over %s answers yes while no controller is registered: the sweep would apply a fact a minute for a "+
			"stall nobody would hear of", root)
	}
	applySweep(t, pool, admission, "unregistered-1")
	applySweep(t, pool, admission, "unregistered-2")
	if reasons := architectWakes(t, pool, root); len(reasons) != 0 {
		t.Fatalf("the architect wakes of %s = %q, want none: no controller is registered to read one", root, reasons)
	}
	if marks := architectWakeMarks(t, pool, root, architectWakeGeneration); len(marks) != 0 {
		t.Fatalf("the arms %s is marked told for = %v, want none: a mark written while no controller ran would keep the arm "+
			"from being told in this run", root, marks)
	}
	if arms := architectStallArmsOf(t, pool, admission, root); !slices.Equal(arms, []record.ArchitectStallArm{record.ArchitectTookNothing}) {
		t.Fatalf("the state's reading of %s names %v while no controller is registered, want the arm that holds: the reading "+
			"tells nobody, and names an arm whether or not anybody was told", root, arms)
	}

	registerController(t, pool)
	if !entrySweepGateOpens(t, pool, admission) {
		t.Fatalf("the gate over %s answers no once a controller is registered: the stall would wait for the project's next fact", root)
	}
	applySweep(t, pool, admission, "registered-1")
	applySweep(t, pool, admission, "registered-2")
	assertArchitectWakes(t, pool, root, []record.ArchitectStallArm{record.ArchitectTookNothing},
		"the first pass after the controller registered tells it, and the mark keeps it to one wake")
	if marks := architectWakeMarks(t, pool, root, architectWakeGeneration); !slices.Equal(marks, []record.ArchitectStallArm{record.ArchitectTookNothing}) {
		t.Fatalf("the arms %s is marked told for = %v, want the one arm that was told", root, marks)
	}
	if entrySweepGateOpens(t, pool, admission) {
		t.Fatalf("the gate after the wake of %s was told answers yes: the sweep would apply a fact a minute for a stall already told", root)
	}
}

// entrySweepGateOpens asks the entry sweep's gate in a read-only transaction of its own, as the
// daemon's sweep asks it before any fact (internal/daemon's applyEntrySweep).
func entrySweepGateOpens(t *testing.T, pool *pgxpool.Pool, admission *Admission) bool {
	t.Helper()
	ctx := context.Background()
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

// architectStallArmsOf is every arm of the stall reading that holds for root now, as the state
// route serves them (ArchitectStallArms), read in a read-only transaction of its own as the
// route's snapshot is.
func architectStallArmsOf(t *testing.T, pool *pgxpool.Pool, admission *Admission, root string) []record.ArchitectStallArm {
	t.Helper()
	ctx := context.Background()
	var arms []record.ArchitectStallArm
	if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		held, err := admission.ArchitectStallArms(ctx, tx)
		arms = held[root]
		return err
	}); err != nil {
		t.Fatalf("read the architect-stalled arms of %s: %v", root, err)
	}
	return arms
}
