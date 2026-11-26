package daemon

// A delivery the project release held reaches its claim after the claim's start
// (dispatch://INFRA-3583 R8), and a row the release holds spends no attempt. A release ends with
// its process, so the tests of a delivery stand in a later process: the claims they read are ones
// that process's boot found stopped by the release (releaseStoppedClaims), read from the store as
// openSupervision reads them, and the set, the outbox and the supervisor's durable-state callback
// are wired as attach wires them.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// releaseDeliveries is the three rows that publish to a role's own topic and that a role nobody
// holds does not reach: each names its recipient and builds its row for that recipient's claim.
// kept is a phase-ended message's and a hook prompt's row, which keeps its message for the role
// (publishKeptToRole) where a role message's row ends (nobodyHolds says each rule).
var releaseDeliveries = []struct {
	name    string
	role    claim.Role
	kept    bool
	payload func(claim.Token) record.OutboxPayload
}{
	{"a role message", claim.RoleImplementer, false, func(token claim.Token) record.OutboxPayload {
		return record.RoleMessage{Role: string(token), Body: "CI settled at the head"}
	}},
	{"a phase-ended message", claim.RoleTester, true, func(claim.Token) record.OutboxPayload { return endedByTheWait }},
	{"a hook prompt", claim.RoleImplementer, true, func(claim.Token) record.OutboxPayload { return firedHook }},
}

// releaseDeliveryRig is an outbox over an isolated database holding LEGION-208 in implementing,
// with a listener that answers each role topic as held or unheld, and one clock the test moves.
type releaseDeliveryRig struct {
	pool      *store.Pool
	st        *store.Store
	records   record.Store
	sup       *supervisor
	runner    *outbox
	publisher *holderPublisher
	// clock is the rig's one clock: the outbox runs its rows by it, and the supervisor stamps a
	// claim's ready by it (supervise.Claim.LastReady), so the first-turn bound the outbox counts
	// from that ready is counted on the time the test moves.
	clock *heldClock
}

// newReleaseDeliveryRig wires the rig and boots it over boot, the claims the boot read from the
// store (releaseDeliveryRig.boot).
func newReleaseDeliveryRig(t *testing.T, boot ...supervise.Claim) *releaseDeliveryRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	rig := &releaseDeliveryRig{pool: pool, st: outboxTreeStore(t, pool), records: records, publisher: &holderPublisher{},
		clock: microClock()}
	rig.boot(t, boot...)
	return rig
}

// boot wires the rig as a boot of the daemon wires it, over claims, the claims that boot read from
// the store: a supervisor of its own on the rig's clock, the set openSupervision reads from those
// claims and from the newest project release the database holds (readReleaseStoppedClaims), and
// the outbox and the supervisor's durable-state callback, the daemon's own, as attach wires them.
// A second call is a restart over the same database: the outbox runs on the new supervisor, and
// the test supervises each claim again as the store held it.
func (r *releaseDeliveryRig) boot(t *testing.T, claims ...supervise.Claim) {
	t.Helper()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Clock = r.clock
	stopped, err := readReleaseStoppedClaims(context.Background(), r.st, "legion", claims, quietLogger())
	if err != nil {
		t.Fatalf("read the claims the project release stopped: %v", err)
	}
	sup.OnDurableState((&workflowRuntime{pool: r.pool, records: r.records, log: quietLogger(), releaseStopped: stopped}).claimDurableState)
	r.sup = sup
	// The tree store is the one a kept role message reads a role's open resume from (wakeComing).
	r.runner = &outbox{dispatchProject: "LEGION", pool: r.pool, records: r.records, supervisor: sup, project: "legion",
		notices: r.publisher, log: quietLogger(), now: r.clock.Now, releaseStopped: stopped, trees: r.st}
}

// recordEndedRelease writes a project release that stopped each of stopped's claims as the
// release and the boot that ended it write it (store.StartProjectRelease, FinishProjectRelease,
// EndProjectReleasesAtBoot): its result names each claim it stopped, and the boot ended it at
// endedAt.
func (r *releaseDeliveryRig) recordEndedRelease(t *testing.T, endedAt time.Time, stopped ...supervise.Claim) {
	t.Helper()
	ctx := context.Background()
	startedAt := endedAt.Add(-time.Hour)
	release, _, _, err := r.st.StartProjectRelease(ctx, "legion", func() time.Time { return startedAt }, 30*time.Minute)
	if err != nil {
		t.Fatalf("record the project release: %v", err)
	}
	result := api.ProjectReleaseResult{Sandboxes: []api.ReleasedSandbox{}, Claims: []api.ReleasedClaim{}}
	for _, c := range stopped {
		result.Claims = append(result.Claims, api.ReleasedClaim{Token: c.Token, Issue: c.Issue, Role: c.Role, Session: c.Session,
			Ended: api.ReleasedClaimEnd(supervise.SuspendedByProjectRelease), BootStarts: true})
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode the release's result: %v", err)
	}
	if err := r.st.FinishProjectRelease(ctx, release.ID, encoded, startedAt.Add(10*time.Minute)); err != nil {
		t.Fatalf("finish the project release: %v", err)
	}
	if _, err := r.st.EndProjectReleasesAtBoot(ctx, "legion", endedAt); err != nil {
		t.Fatalf("end the project release at the boot: %v", err)
	}
}

// stoppedByRelease is role's claim on LEGION-208 as a project release leaves it in the store:
// suspended with the source `project_release`, on the session it ran in.
func stoppedByRelease(t *testing.T, role claim.Role) supervise.Claim {
	t.Helper()
	return supervise.Claim{Token: mustClaimToken(t, "LEGION-208", role), Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
		Role: role, Generation: 1, ServingGeneration: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByProjectRelease,
		Session: "ses-" + string(role), SessionFile: "/tmp/" + string(role) + ".jsonl"}
}

// supervise gives held its machine, as the boot's restore gives each stored claim one.
func (r *releaseDeliveryRig) supervise(t *testing.T, held supervise.Claim) *supervise.Machine {
	t.Helper()
	machine, _, err := r.sup.Create(context.Background(), held, "")
	if err != nil {
		t.Fatalf("supervise %s: %v", held.Token, err)
	}
	return machine
}

// enqueue writes payload's row for LEGION-208, due at the rig's clock.
func (r *releaseDeliveryRig) enqueue(t *testing.T, payload record.OutboxPayload) {
	t.Helper()
	enqueueOutbox(t, r.pool, r.records, mustOutboxRow(t, "LEGION-208", payload, r.clock.Now()))
}

// run moves the clock past any backoff and runs every row due then, as a lane runs each.
func (r *releaseDeliveryRig) run(t *testing.T) {
	t.Helper()
	r.runAfter(t, 5*time.Minute)
}

// runAfter moves the clock on by d and runs every row due then, as a lane runs each.
func (r *releaseDeliveryRig) runAfter(t *testing.T, d time.Duration) {
	t.Helper()
	r.clock.advance(d)
	runDue(t, r.runner, r.clock.Now())
}

// waits runs the row and requires that it waited: nothing was published or tried, and the row is
// still in the outbox with a wait as its last error.
func (r *releaseDeliveryRig) waits(t *testing.T, when string) {
	t.Helper()
	r.run(t)
	if attempts, delivered := r.publisher.snapshot(); attempts != 0 || len(delivered) != 0 {
		t.Fatalf("%s: publish attempts = %d with %d delivered, want none: the row waits for the claim's start", when, attempts, len(delivered))
	}
	if _, lastError, present := outboxRowState(t, r.pool, "LEGION-208"); !present || !strings.Contains(lastError, "waits for") {
		t.Fatalf("%s: the row is present=%t with %q, want it waiting in the outbox", when, present, lastError)
	}
}

// endsUndelivered runs the row and requires the outbox's rule for a role nobody holds: one
// refused publish, nothing delivered beyond what was, and the row finished.
func (r *releaseDeliveryRig) endsUndelivered(t *testing.T, when string, attemptsBefore, deliveredBefore int) {
	t.Helper()
	r.run(t)
	if attempts, delivered := r.publisher.snapshot(); attempts != attemptsBefore+1 || len(delivered) != deliveredBefore {
		t.Fatalf("%s: publish attempts = %d with %d delivered, want %d and %d: one refused publish to a role nobody holds",
			when, attempts, len(delivered), attemptsBefore+1, deliveredBefore)
	}
	if left := outboxRows(t, r.pool); left != 0 {
		t.Fatalf("%s: outbox rows left = %d, want the row finished undelivered", when, left)
	}
}

// nobodyHolds runs the row and requires the outbox's rule for a role nobody holds, which is the
// row's kind's: a role message is published once, refused, and finished undelivered
// (endsUndelivered). A phase-ended message or a hook prompt (kept) is its own row's to keep
// (publishKeptToRole): published once and refused, it waits for a session to hold the role within
// its hour, but for a role whose claim failed or retired (ended), which no session holds again, so
// its row ends before any publish (keptRoleMessageEnded).
func (r *releaseDeliveryRig) nobodyHolds(t *testing.T, kept, ended bool, when string, attemptsBefore, deliveredBefore int) {
	t.Helper()
	if !kept {
		r.endsUndelivered(t, when, attemptsBefore, deliveredBefore)
		return
	}
	r.run(t)
	published := attemptsBefore + 1
	if ended {
		published = attemptsBefore
	}
	if attempts, delivered := r.publisher.snapshot(); attempts != published || len(delivered) != deliveredBefore {
		t.Fatalf("%s: publish attempts = %d with %d delivered, want %d and %d", when, attempts, len(delivered), published, deliveredBefore)
	}
	var held int
	var lastError string
	if err := r.pool.QueryRow(context.Background(), `select count(*), coalesce(max(last_error), '') from outbox
		where issue = 'LEGION-208' and kind in ('phase_ended', 'hook_prompt')`).Scan(&held, &lastError); err != nil {
		t.Fatalf("%s: read the kept message's row: %v", when, err)
	}
	switch {
	case ended && held != 0:
		t.Fatalf("%s: %d kept message rows left, want the row ended: no session of a failed or retired claim holds the role again", when, held)
	case !ended && (held != 1 || !strings.Contains(lastError, "waits for a session to hold")):
		t.Fatalf("%s: %d kept message rows left with %q, want the row waiting for a session to hold the role", when, held, lastError)
	}
}

// A role message, a phase-ended message and a hook prompt for a claim the boot found stopped by
// the project release each wait while that claim has not come back: before the boot's start of it
// has run, and while the start's process is launching, connected and registered, when no session
// holds the role's topic and a publish would be refused. Once the claim has stood ready past the
// first-turn bound its session holds the role, and the row is published to it. The claim is then
// out of the set, so when its process dies later and it is relaunching, a new row meets the rule
// for a role nobody holds, as it does for any other claim (nobodyHolds).
func TestADeliveryHeldThroughAProjectReleaseReachesItsClaimAfterItsStart(t *testing.T) {
	t.Parallel()
	for _, tc := range releaseDeliveries {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			stored := stoppedByRelease(t, tc.role)
			rig := newReleaseDeliveryRig(t, stored)
			machine := rig.supervise(t, stored)
			topic := notify.RoleTopicPrefix + string(stored.Token)
			rig.publisher.setAbsent(topic)
			rig.enqueue(t, tc.payload(stored.Token))

			rig.waits(t, "before the boot's start of the claim has run")

			rig.start(t, machine, stored)
			generation := machine.Claim().Generation
			for _, step := range []struct {
				state supervise.ClaimState
				event supervise.Event
			}{
				{supervise.StateLaunching, nil},
				{supervise.StateShimConnected, supervise.StreamHello{Claim: stored.Token, Generation: generation}},
				{supervise.StateRegistered, supervise.RequestRegister{Claim: stored.Token, Generation: generation, Session: stored.Session, SessionFile: stored.SessionFile}},
			} {
				if step.event != nil {
					if err := machine.Handle(ctx, step.event); err != nil {
						t.Fatalf("handle %T: %v", step.event, err)
					}
				}
				if got := machine.Claim().State; got != step.state {
					t.Fatalf("the claim is %s, want %s", got, step.state)
				}
				rig.waits(t, "while the claim is "+string(step.state))
			}

			if err := machine.Handle(ctx, supervise.RequestReady{Claim: stored.Token, Generation: generation, Session: stored.Session}); err != nil {
				t.Fatalf("make the claim ready: %v", err)
			}
			rig.publisher.setAbsent()
			rig.run(t)
			attempts, delivered := rig.publisher.snapshot()
			if attempts != 1 || len(delivered) != 1 || delivered[0].topic != topic {
				t.Fatalf("once the claim is ready: %d publish attempts delivered %+v, want the one publish to %s", attempts, delivered, topic)
			}
			if left := outboxRows(t, rig.pool); left != 0 {
				t.Fatalf("once the claim is ready: outbox rows left = %d, want the row finished", left)
			}

			if err := machine.Handle(ctx, supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *machine.Claim().Locator, Kind: runtime.Gone}}); err != nil {
				t.Fatalf("report the claim's process gone: %v", err)
			}
			if got := machine.Claim().State; got != supervise.StateLaunching {
				t.Fatalf("after its process died the claim is %s, want it relaunching", got)
			}
			rig.publisher.setAbsent(topic)
			rig.enqueue(t, tc.payload(stored.Token))
			rig.nobodyHolds(t, tc.kept, false, "while the claim that came back relaunches", attempts, len(delivered))
		})
	}
}

// start starts held's claim as the boot's start does, a resume by rule, with a connection
// registered for its process to come up on.
func (r *releaseDeliveryRig) start(t *testing.T, machine *supervise.Machine, held supervise.Claim) {
	t.Helper()
	r.sup.deps.Conns.(*fake.Conns).Register(held.Token, fake.NewConn())
	if err := machine.Handle(context.Background(), supervise.RequestRuleResume{Claim: held.Token, Reason: "the boot starts a claim the project release stopped"}); err != nil {
		t.Fatalf("start the claim: %v", err)
	}
}

// comeUp starts held's claim as the boot's start does (start) and brings its process up to
// registered: launched, its shim connected and its agent registered, with no ready yet.
func (r *releaseDeliveryRig) comeUp(t *testing.T, machine *supervise.Machine, held supervise.Claim) {
	t.Helper()
	r.start(t, machine, held)
	r.register(t, machine, held)
}

// register brings machine's launching claim up to registered: its shim connects and its agent
// registers on held's session.
func (r *releaseDeliveryRig) register(t *testing.T, machine *supervise.Machine, held supervise.Claim) {
	t.Helper()
	generation := machine.Claim().Generation
	for _, event := range []supervise.Event{
		supervise.StreamHello{Claim: held.Token, Generation: generation},
		supervise.RequestRegister{Claim: held.Token, Generation: generation, Session: held.Session, SessionFile: held.SessionFile},
	} {
		if err := machine.Handle(context.Background(), event); err != nil {
			t.Fatalf("handle %T: %v", event, err)
		}
	}
	if got := machine.Claim().State; got != supervise.StateRegistered {
		t.Fatalf("the claim is %s, want it registered", got)
	}
}

// ready reports machine's claim ready, as its agent does before its session takes its role.
func (r *releaseDeliveryRig) ready(t *testing.T, machine *supervise.Machine, held supervise.Claim) {
	t.Helper()
	if err := machine.Handle(context.Background(), supervise.RequestReady{Claim: held.Token, Generation: machine.Claim().Generation, Session: held.Session}); err != nil {
		t.Fatalf("make the claim ready: %v", err)
	}
}

// A claim the boot started after the project release stands ready before its session holds its
// role: the plugin takes the role at the session's first turn, or at the first-turn bound when no
// task starts a turn (supervise.SaidReady). A delivery that runs inside that window publishes
// nothing, where the publish would be refused and end the row undelivered, and waits until the
// bound and its margin have passed since the ready. The session takes its role some seconds after
// the ready, and the row, run again at the window's end, reaches it.
func TestADeliveryHeldThroughAProjectReleaseWaitsForTheRoleAfterTheClaimIsReady(t *testing.T) {
	t.Parallel()
	for _, tc := range releaseDeliveries {
		t.Run(tc.name, func(t *testing.T) {
			stored := stoppedByRelease(t, tc.role)
			rig := newReleaseDeliveryRig(t, stored)
			machine := rig.supervise(t, stored)
			topic := notify.RoleTopicPrefix + string(stored.Token)
			rig.publisher.setAbsent(topic)
			rig.comeUp(t, machine, stored)
			rig.ready(t, machine, stored)
			roleBy := machine.Claim().LastReady.Add(config.ClaimFirstTurnBound + roleTakenMargin)
			rig.enqueue(t, tc.payload(stored.Token))

			rig.runAfter(t, 5*time.Second)
			if attempts, _ := rig.publisher.snapshot(); attempts != 0 {
				t.Fatalf("5 s after the ready: %d publish attempts, want none: the session has not taken its role yet", attempts)
			}
			row, present := readHeldRow(t, rig.pool, "LEGION-208")
			if !present || !row.nextAt.Equal(roleBy) || !strings.Contains(row.lastError, "waits for") {
				t.Fatalf("5 s after the ready the row is present=%t with %s, want it waiting until %s, the first-turn bound and its margin after the ready",
					present, row, roleBy.Format(time.RFC3339Nano))
			}
			// The wait says what it waits for, the claim's start after the project release, and
			// not an idle release's pod coming back, which no wake here brings.
			if !strings.Contains(row.lastError, "after the start the daemon's boot gives a claim the project release stopped") ||
				strings.Contains(row.lastError, "released pod") {
				t.Fatalf("5 s after the ready the row waits with %q, want the wait for the start the boot gives after the project release", row.lastError)
			}

			// The session takes its role 20 seconds after its ready, at no turn of its own.
			rig.clock.advance(15 * time.Second)
			rig.publisher.setAbsent()
			rig.runAfter(t, roleBy.Sub(rig.clock.Now()))
			attempts, delivered := rig.publisher.snapshot()
			if attempts != 1 || len(delivered) != 1 || delivered[0].topic != topic {
				t.Fatalf("at the first-turn bound's end: %d publish attempts delivered %+v, want the one publish to %s", attempts, delivered, topic)
			}
			if left := outboxRows(t, rig.pool); left != 0 {
				t.Fatalf("at the first-turn bound's end: outbox rows left = %d, want the row finished", left)
			}
		})
	}
}

// A merge forward's message (record.MergeForwardMessage) is held for its role as a merge-queue
// removal is (deliverHeld): while nobody holds the role the row waits rather than ending. For a
// claim the boot found stopped by the project release it also publishes nothing until that claim
// has come back, as the role message, the phase-ended message and the hook prompt above do
// (wakeForDelivery's awaitsBootStart): before the boot's start of it has run, and while the
// start's process is launching and registered. Once the claim has stood ready past the first-turn
// bound its session holds the role, and the ask is published to it once, under its row's key. The
// negative control is deliverHeld without that wait: each of those attempts then publishes to the
// topic nobody holds yet, and the row counts publishes the test wants none of.
func TestAMergeForwardMessageHeldThroughAProjectReleaseWaitsForTheBootsStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stored := stoppedByRelease(t, claim.RoleImplementer)
	rig := newReleaseDeliveryRig(t, stored)
	pull := record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "acme/widgets",
		Number: 86, Branch: "legion/LEGION-208", HeadSHA: "sha-1", HeadUpdatedAt: rig.clock.Now(), Failing: []string{},
		CheckRuns: []record.AttemptRun{}, Base: "main", MergeForward: &record.MergeForward{Head: "sha-1", Base: "main",
			Phase: phase.Implementing, Why: record.ConflictCheckNotGreen, AskedAt: rig.clock.Now()}}
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		return rig.records.PutPullRequest(ctx, tx, pull)
	}); err != nil {
		t.Fatalf("seed the pull request and the merge forward it holds: %v", err)
	}
	ask := record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: "sha-1", Base: "main",
		Phase: phase.Implementing, PullRequest: 86, Why: record.ConflictCheckNotGreen}
	machine := rig.supervise(t, stored)
	topic := notify.RoleTopicPrefix + string(stored.Token)
	rig.publisher.setAbsent(topic)
	rig.enqueue(t, ask)
	id := noticeRowID(t, rig.pool)

	rig.waits(t, "before the boot's start of the claim has run")
	rig.start(t, machine, stored)
	rig.waits(t, "while the claim launches")
	rig.register(t, machine, stored)
	rig.waits(t, "while the claim is registered")

	rig.ready(t, machine, stored)
	rig.publisher.setAbsent()
	rig.run(t)
	attempts, delivered := rig.publisher.snapshot()
	if attempts != 1 || len(delivered) != 1 || delivered[0].topic != topic || delivered[0].keep ||
		delivered[0].key != record.OutboxKey(id) || delivered[0].message != ask.Message("LEGION-208", pull) {
		t.Fatalf("once the claim holds its role: %d publish attempts delivered %+v, want the ask published once to %s under its row's key, unkept",
			attempts, delivered, topic)
	}
	if left := outboxRows(t, rig.pool); left != 0 {
		t.Fatalf("once the claim holds its role: outbox rows left = %d, want the row finished", left)
	}
}

// The wake pacing can hold the boot's start of a claim the project release stopped for longer than
// an hour over a wave, the claim launching with no process all that time. A role message, a
// phase-ended message and a hook prompt held for it each wait for that start as a delivery waits
// for any launch the pacing parked (launchParked): the row runs again at the start's turn or
// parkedWakeBound on, whichever comes first, on a routine wait, rather than climbing its backoff
// to 64 s and warning at every attempt from the seventh for as long as the park lasts. The
// negative control is awaitsBootStart without its read of the park: each delivery, which reads
// the park of such a claim through awaitsBootStart alone, then waits on its backoff with the
// start's own sentence.
func TestADeliveryHeldThroughAProjectReleaseWaitsOutThePacingOfItsStart(t *testing.T) {
	t.Parallel()
	for _, tc := range releaseDeliveries {
		t.Run(tc.name, func(t *testing.T) {
			stored := stoppedByRelease(t, tc.role)
			rig := newReleaseDeliveryRig(t, stored)
			// A machine takes its deps where it is built, so the pacing goes in before the claim
			// is supervised: it lets the first paced launch out parkedTurn after the rig's now
			// (parkedFor), and the rig's clock fires that let-out's timer only when a test fires it.
			rig.sup.deps.WakePace = parkedFor(rig.clock, parkedTurn)
			machine := rig.supervise(t, stored)
			rig.publisher.setAbsent(notify.RoleTopicPrefix + string(stored.Token))
			rig.start(t, machine, stored)
			if held := machine.View(); held.State != supervise.StateLaunching || held.Locator != nil ||
				held.WakeWaiting == nil || held.WakeWaiting.Kind != supervise.WakeLaunch {
				t.Fatalf("after the boot's start the claim is %s recording %+v with wake %+v, want it launching with no process under the start the pacing parked",
					held.State, held.Locator, held.WakeWaiting)
			}
			rig.enqueue(t, tc.payload(stored.Token))

			rig.waits(t, "while the wake pacing holds the claim's start")
			row, present := readHeldRow(t, rig.pool, "LEGION-208")
			if due := rig.clock.Now().Add(parkedWakeBound); !present || !row.nextAt.Equal(due) ||
				!strings.Contains(row.lastError, "launch, which the wake pacing holds until") {
				t.Fatalf("while the wake pacing holds the claim's start the row is present=%t with %s, want it waiting for the parked launch until %s, parkedWakeBound on",
					present, row, due.Format(time.RFC3339Nano))
			}
		})
	}
}

// Each boot builds the set again from the store (readReleaseStoppedClaims): the claims the newest
// release's record names, read with what each claim's record says since that release ended. So a
// claim the first boot after the release started, whose session did not hold its role yet when the
// daemon restarted, is still waited for in the second boot: a delivery held for it through the
// release and through the restart waits while the claim comes up again, and reaches it once its
// session holds the role.
func TestADeliveryHeldThroughAProjectReleaseWaitsForItsClaimAcrossARestart(t *testing.T) {
	t.Parallel()
	for _, tc := range releaseDeliveries {
		t.Run(tc.name, func(t *testing.T) {
			stored := stoppedByRelease(t, tc.role)
			rig := newReleaseDeliveryRig(t)
			rig.recordEndedRelease(t, rig.clock.Now(), stored)
			rig.boot(t, stored)
			machine := rig.supervise(t, stored)
			topic := notify.RoleTopicPrefix + string(stored.Token)
			rig.publisher.setAbsent(topic)
			rig.enqueue(t, tc.payload(stored.Token))

			// The first boot's start of the claim: the claim is written launching.
			rig.start(t, machine, stored)
			if got := machine.Claim().State; got != supervise.StateLaunching {
				t.Fatalf("after the first boot's start the claim is %s, want it launching", got)
			}
			rig.waits(t, "in the first boot, while the claim it started is launching")

			// The daemon restarts before the claim's session holds its role, and the second boot
			// reads the claim launching, as the store holds it.
			launching := machine.Claim()
			rig.boot(t, launching)
			machine = rig.supervise(t, launching)
			rig.waits(t, "in the second boot, while the claim the first boot started is launching")
			rig.sup.deps.Conns.(*fake.Conns).Register(stored.Token, fake.NewConn())
			rig.register(t, machine, stored)
			rig.waits(t, "in the second boot, while the claim the first boot started is registered")

			rig.ready(t, machine, stored)
			rig.publisher.setAbsent()
			rig.run(t)
			attempts, delivered := rig.publisher.snapshot()
			if attempts != 1 || len(delivered) != 1 || delivered[0].topic != topic {
				t.Fatalf("once the session holds its role: %d publish attempts delivered %+v, want the one publish to %s", attempts, delivered, topic)
			}
			if left := outboxRows(t, rig.pool); left != 0 {
				t.Fatalf("once the session holds its role: outbox rows left = %d, want the row finished", left)
			}
		})
	}
}

// A claim the newest release stopped whose session has held its role since that release ended is
// not waited for after a restart: one that had a turn since and is relaunching now, and one whose
// ready the store holds from longer ago than the first-turn bound. Each delivery for them meets the
// rule for a role nobody holds (nobodyHolds).
func TestADeliveryForAClaimThatHeldItsRoleSinceTheReleaseEndsAfterARestart(t *testing.T) {
	t.Parallel()
	for _, tc := range releaseDeliveries {
		for _, since := range []struct {
			name  string
			state supervise.ClaimState
			// turn and ready are how long after the release ended the claim last began a turn
			// and last reported ready, zero for never.
			turn, ready time.Duration
		}{
			{"had a turn and relaunches", supervise.StateLaunching, 2 * time.Minute, time.Minute},
			{"stood ready past the first-turn bound", supervise.StateReady, 0, time.Minute},
		} {
			t.Run(tc.name+" for a claim that "+since.name, func(t *testing.T) {
				held := stoppedByRelease(t, tc.role)
				rig := newReleaseDeliveryRig(t)
				endedAt := rig.clock.Now().Add(-10 * time.Minute)
				rig.recordEndedRelease(t, endedAt, held)
				held.State, held.SuspendedBy = since.state, ""
				if since.turn > 0 {
					held.LastTurnStart = endedAt.Add(since.turn)
				}
				held.LastReady = endedAt.Add(since.ready)
				rig.boot(t, held)
				rig.supervise(t, held)
				rig.publisher.setAbsent(notify.RoleTopicPrefix + string(held.Token))
				rig.enqueue(t, tc.payload(held.Token))

				rig.nobodyHolds(t, tc.kept, false, "for a claim that "+since.name, 0, 0)
			})
		}
	}
}

// The boot's start of a phase owner the project release stopped names the owner's phase, and the
// issue can leave that phase, or the role's phases row move to a later entry, before the start
// runs: a red CI moves the issue back while the start waits its turn. That start is the claim's one
// start, so the outbox starts the claim anyway, resumed by rule and with no task, as the boot starts
// a role of an earlier phase, rather than finish the row and leave the claim suspended by the
// release with every delivery for it waiting. The same start of a claim no release stopped still
// finishes without acting, and so does a start queued for news alone of a claim the release
// stopped, which is never that claim's start (workflow.StartsWork): the boot writes the claim's
// own start beside it.
func TestABootStartOfAStoppedPhaseOwnerStartsItAfterItsIssueLeftThePhase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		role  claim.Role
		by    supervise.SuspendSource
		start record.SuperviseRequest
		// entry is the entry the role's phases row names, 0 for no row.
		entry   int64
		started bool
	}{
		{"a start of a phase the issue has left", claim.RoleTester, supervise.SuspendedByProjectRelease,
			record.SuperviseRequest{Phase: phase.Testing, Task: "carry the testing phase on", ResumeTask: true}, 0, true},
		{"a start of an entry its role has left", claim.RoleImplementer, supervise.SuspendedByProjectRelease,
			record.SuperviseRequest{Phase: phase.Implementing, Entry: 1, Task: "the work of entry 1"}, 2, true},
		{"a start of a phase the issue has left, for a claim the daemon suspended", claim.RoleTester, supervise.SuspendedByDaemon,
			record.SuperviseRequest{Phase: phase.Testing, Task: "carry the testing phase on", ResumeTask: true}, 0, false},
		{"a start queued for news alone of a phase the issue has left", claim.RoleTester, supervise.SuspendedByProjectRelease,
			record.SuperviseRequest{Phase: phase.Testing, News: true}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			stored := stoppedByRelease(t, tc.role)
			stored.SuspendedBy = tc.by
			rig := newReleaseDeliveryRig(t, stored)
			machine := rig.supervise(t, stored)
			rig.sup.deps.Conns.(*fake.Conns).Register(stored.Token, fake.NewConn())
			if tc.entry > 0 {
				if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
					return rig.records.PutPhase(ctx, tx, record.PhaseRow{Issue: "LEGION-208", Role: tc.role, Claim: stored.Token,
						Entry: tc.entry, EntryTask: "the work of entry 2", EntryRaisedAt: rig.clock.Now()})
				}); err != nil {
					t.Fatalf("move the role's phases row to entry %d: %v", tc.entry, err)
				}
			}
			start := tc.start
			start.Op, start.Tree, start.Role, start.Generation = "start", "LEGION-208", tc.role, 1
			// A start queued for news alone is never resumed by rule (record.NewOutboxRow).
			start.RuleResume = !start.News
			rig.enqueue(t, start)

			rig.run(t)
			if left := outboxRows(t, rig.pool); left != 0 {
				t.Fatalf("outbox rows left = %d, want the start finished", left)
			}
			held := machine.Claim()
			if !tc.started {
				if held.State != supervise.StateSuspended || held.SuspendedBy != tc.by {
					t.Fatalf("the claim is %s by %q, want it left suspended by %s: the fence finishes this start", held.State, held.SuspendedBy, tc.by)
				}
				return
			}
			if held.State != supervise.StateLaunching {
				t.Fatalf("the claim is %s by %q, want it started and launching", held.State, held.SuspendedBy)
			}
			if held.Pending != nil {
				t.Fatalf("the started claim holds the task %q, want none: the start of a phase or entry the issue has left carries no task", held.Pending.Task)
			}
		})
	}
}

// The boot writes no start for a phase owner a queued start already covers, and that start can be
// taken out before a lane runs it, by a later start of another role written in its place: a
// tester's fail moves its issue back to implementing, and the implementer's start takes out the
// tester's start the release held (workflow.StartOrOwe, record.Store.EnqueueStart).
// That start was the stopped tester's one start, so the same write leaves the tester a start of
// its own, and the claim launches, resumed by rule and with no task, as the outbox's phase fence
// would have started it, while the implementer's start waits for the issue's slot.
func TestAStartTakenOutByAnotherRolesStartStillStartsTheClaimTheProjectReleaseStopped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stored := stoppedByRelease(t, claim.RoleTester)
	rig := newReleaseDeliveryRig(t, stored)
	machine := rig.supervise(t, stored)
	rig.sup.deps.Conns.(*fake.Conns).Register(stored.Token, fake.NewConn())
	if err := rig.st.PutClaim(ctx, stored); err != nil {
		t.Fatalf("store the tester's claim as the release left it: %v", err)
	}
	// The record stands in implementing, where the tester's fail moved it. The tester's start was
	// written when the issue entered testing and held through the release; the implementer's is
	// the move back.
	for _, start := range []record.SuperviseRequest{
		{Op: "start", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 1, Phase: phase.Testing,
			Task: "Test the change at its head.", Entry: 1},
		{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing,
			Task: "Fix what the tester found.", Entry: 2},
	} {
		if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			return workflow.StartOrOwe(ctx, tx, rig.records, "LEGION-208", start, rig.clock.Now())
		}); err != nil {
			t.Fatalf("write the %s's start: %v", start.Role, err)
		}
	}

	rig.run(t)
	held := machine.Claim()
	if held.State != supervise.StateLaunching {
		t.Fatalf("the tester's claim is %s by %q, want it started and launching: the start the implementer's took out was its one start", held.State, held.SuspendedBy)
	}
	if held.Pending != nil {
		t.Fatalf("the started tester holds the task %q, want none: the issue has left testing", held.Pending.Task)
	}
	rows, err := rig.pool.Query(ctx, `select payload->>'role' || ': ' || coalesce(payload->>'task', '') from outbox where issue = 'LEGION-208' order by id`)
	if err != nil {
		t.Fatalf("list the rows left: %v", err)
	}
	left, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the rows left: %v", err)
	}
	if want := []string{"implementer: Fix what the tester found."}; !slices.Equal(left, want) {
		t.Fatalf("the rows left = %q, want %q: the implementer's start waits for the issue's slot", left, want)
	}
}

// A claim no project release stopped keeps the outbox's rule for a role nobody holds while it
// comes up: the boot read it launching or registered, so it is not in the set, and its row is
// published once, refused, and handled as its kind handles a role nobody holds (nobodyHolds).
func TestADeliveryForAClaimNoReleaseStoppedEndsWhenNobodyHoldsItsRole(t *testing.T) {
	t.Parallel()
	for _, tc := range releaseDeliveries {
		for _, state := range []supervise.ClaimState{supervise.StateLaunching, supervise.StateRegistered} {
			t.Run(tc.name+" for a claim that is "+string(state), func(t *testing.T) {
				// The claim as the boot read it: coming up, and stopped by no release.
				held := stoppedByRelease(t, tc.role)
				held.State, held.SuspendedBy = state, ""
				rig := newReleaseDeliveryRig(t, held)
				rig.supervise(t, held)
				rig.publisher.setAbsent(notify.RoleTopicPrefix + string(held.Token))
				rig.enqueue(t, tc.payload(held.Token))

				rig.nobodyHolds(t, tc.kept, false, "for a claim no release stopped", 0, 0)
			})
		}
	}
}

// A claim the boot found stopped by the project release that will not come back holds no row: one
// that has failed, one that has retired, and one an operator or the daemon has suspended since
// each leave the row to the rule for a role nobody holds (nobodyHolds), under which a kept
// message to a failed or retired claim ends before any publish.
func TestADeliveryStopsWaitingForAStoppedClaimThatWillNotComeBack(t *testing.T) {
	t.Parallel()
	for _, tc := range releaseDeliveries {
		for _, now := range []struct {
			name  string
			state supervise.ClaimState
			by    supervise.SuspendSource
		}{
			{"has failed", supervise.StateFailed, ""},
			{"has retired", supervise.StateRetired, ""},
			{"an operator has suspended", supervise.StateSuspended, supervise.SuspendedByOperator},
			{"the daemon has suspended", supervise.StateSuspended, supervise.SuspendedByDaemon},
		} {
			t.Run(tc.name+" for a stopped claim that "+now.name, func(t *testing.T) {
				stored := stoppedByRelease(t, tc.role)
				rig := newReleaseDeliveryRig(t, stored)
				held := stored
				held.State, held.SuspendedBy = now.state, now.by
				rig.supervise(t, held)
				rig.publisher.setAbsent(notify.RoleTopicPrefix + string(held.Token))
				rig.enqueue(t, tc.payload(held.Token))

				ended := now.state == supervise.StateFailed || now.state == supervise.StateRetired
				rig.nobodyHolds(t, tc.kept, ended, "for a stopped claim that "+now.name, 0, 0)
			})
		}
	}
}

// heldRow is the one outbox row of issue as the store holds it.
type heldRow struct {
	attempts  int
	nextAt    time.Time
	lastError string
	leased    bool
}

func (r heldRow) String() string {
	return fmt.Sprintf("%d attempts, leased=%t, due at %s, last error %q", r.attempts, r.leased, r.nextAt.UTC().Format(time.RFC3339Nano), r.lastError)
}

func readHeldRow(t *testing.T, pool *store.Pool, issue string) (heldRow, bool) {
	t.Helper()
	var row heldRow
	err := pool.QueryRow(context.Background(), `select attempts, next_at, last_error, lease_token <> '' or lease_until is not null
		from outbox where issue = $1`, issue).Scan(&row.attempts, &row.nextAt, &row.lastError, &row.leased)
	if errors.Is(err, pgx.ErrNoRows) {
		return heldRow{}, false
	}
	if err != nil {
		t.Fatalf("read the outbox row of %s: %v", issue, err)
	}
	return row, true
}

// A row the project release holds is put back unrun as often as its lane reads it, and none of
// those reads is an attempt: the row keeps the attempts it had, its lease is cleared, and it is
// due again one fixed spacing later. A merge-queue read shows what that keeps: it bounds its reads
// of GitHub by the row's attempts (mergeStateLadderReads), so after a release that held it more
// often than that bound, the next process's first failed read is the first rung of the ladder and
// the row reads again, where a counted hold would have ended the ladder and told the merger that
// no read succeeded. The hold is logged once at info and at debug from then on.
func TestARowTheProjectReleaseHoldsSpendsNoAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Merging, Generation: 1, Status: "in_progress", Rank: "U"}
	putOutboxIssue(t, pool, records, issue)
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: issue.Key, Repo: "acme/widgets",
			Number: 86, Branch: "legion/LEGION-208", HeadSHA: "sha-1", HeadUpdatedAt: time.Now(), Failing: []string{},
			CheckRuns: []record.AttemptRun{}, Base: "main"})
	}); err != nil {
		t.Fatalf("record the pull request: %v", err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	reads := 0
	var logged bytes.Buffer
	runner := func(release *projectRelease) *outbox {
		return &outbox{pool: pool, dispatchProject: "LEGION", records: records, project: "legion", release: release,
			log: slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
			now: func() time.Time { return at },
			readPullRequest: func(context.Context, record.PullRequest) (githubrest.Client, githubPullRequest, error) {
				reads++
				return githubrest.Client{}, githubPullRequest{}, errors.New("GitHub answered 502")
			},
			// Every read fails, so the row applies no answer of GitHub's.
			applyMergeability: func(context.Context, githubrest.Client, record.PullRequest, githubPullRequest, time.Time, string) (intake.PullRequestMergeability, error) {
				return intake.PullRequestMergeability{}, errors.New("a failed read applies no mergeability")
			},
			mergeStateWait: func() time.Duration { return time.Second }}
	}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, record.MergeQueueRead{Number: 86, Head: "sha-1"}, at))

	released := runner(releasedMark(t))
	const holds = mergeStateLadderReads + 1
	for hold := 1; hold <= holds; hold++ {
		if ran := runDue(t, released, at); ran != 1 {
			t.Fatalf("hold %d: rows due = %d, want the held row due again", hold, ran)
		}
		row, present := readHeldRow(t, pool, issue.Key)
		if !present || row.attempts != 0 || row.leased || !row.nextAt.Equal(at.Add(releaseHoldSpacing)) ||
			!strings.Contains(row.lastError, api.ProjectReleaseHeldSentence) {
			t.Fatalf("hold %d: the row is present=%t with %s; want it unleased with no attempt, due at %s on the release's wait",
				hold, present, row, at.Add(releaseHoldSpacing).Format(time.RFC3339Nano))
		}
		at = row.nextAt
	}
	if reads != 0 {
		t.Fatalf("reads of GitHub while released = %d, want none", reads)
	}
	var levels []string
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		var entry struct{ Level, Msg string }
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode the log line %q: %v", line, err)
		}
		if entry.Msg == "outbox row waits" {
			levels = append(levels, entry.Level)
		}
	}
	if want := append([]string{"INFO"}, slices.Repeat([]string{"DEBUG"}, holds-1)...); !slices.Equal(levels, want) {
		t.Fatalf("the held row's wait was logged at %v, want %v", levels, want)
	}

	if ran := runDue(t, runner(&projectRelease{}), at); ran != 1 {
		t.Fatalf("rows run by the next process = %d, want the read", ran)
	}
	row, present := readHeldRow(t, pool, issue.Key)
	if reads != 1 || !present || row.attempts != 1 ||
		!strings.Contains(row.lastError, fmt.Sprintf("after the read of attempt 1 of %d failed", mergeStateLadderReads)) {
		t.Fatalf("after the next process's first failed read: %d reads, the row present=%t with %s; want one read and the row waiting to read again as attempt 1 of %d",
			reads, present, row, mergeStateLadderReads)
	}
}
