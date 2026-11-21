package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// copyBundler is the runtime's bundler of an issue workspace as the cadence's tests drive it: it
// records every role it was asked and answers what the test set — a bundle, an error, or a hold
// that lasts until the test lets go or the copy's budget runs out. The release's own fake
// (releaseRuntime) is the other bundler of this package and is left as it is: the cadence reads
// its bundler from a field of its own (workflowRuntime.copyBundler), so the two paths can be
// driven apart.
type copyBundler struct {
	mu     sync.Mutex
	asked  []claim.Role
	bundle runtime.WorkspaceBundle
	err    error
	hold   func(context.Context) (runtime.WorkspaceBundle, error)
}

func (b *copyBundler) BundleWorkspace(ctx context.Context, _ string, role claim.Role) (runtime.WorkspaceBundle, error) {
	b.mu.Lock()
	b.asked = append(b.asked, role)
	hold, bundle, err := b.hold, b.bundle, b.err
	b.mu.Unlock()
	if hold != nil {
		return hold(ctx)
	}
	return bundle, err
}

// reads is every role the cadence has asked for a bundle, in order.
func (b *copyBundler) reads() []claim.Role {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]claim.Role(nil), b.asked...)
}

// answers makes the bundler answer bundle and err from now on, and holds nothing.
func (b *copyBundler) answers(bundle runtime.WorkspaceBundle, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bundle, b.err, b.hold = bundle, err, nil
}

// holds makes the bundler answer through hold, which the test's own goroutine keeps waiting.
func (b *copyBundler) holds(hold func(context.Context) (runtime.WorkspaceBundle, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hold = hold
}

// copyRig is the idle release's rig with the cadence wired onto it: the daemon's store, a bundler
// of the test's own, a log the test reads back as records, and the cadence's clock. Its issue is
// the rig's — one live implementer, idle with its phase's task pending, on a pod that has been
// quiet — so a test that wants a turn in flight starts one (turnStart).
type copyRig struct {
	*idleRig
	bundler *copyBundler
	conn    *fake.Conn
	logs    *testlog.Buffer
	// clock is the cadence's clock (runningCopies.now): the wall clock until a test moves it, and
	// held where the test put it from then on.
	clock *heldClock
}

// copyRigBundle is what the rig's bundler answers: a copy of the issue workspace's unpushed work,
// naming the pod it came off as the Kubernetes runtime's own bundler does (sandbox's
// Runtime.BundleWorkspace), which the store needs to tell that pod from the pod replacing it.
var copyRigBundle = runtime.WorkspaceBundle{Bundle: []byte("PACK the work of the turn"), Head: "c0ffee",
	Tips: []string{"c0ffee"}, PodUID: releasedPodUID}

func newCopyRig(t *testing.T) *copyRig {
	t.Helper()
	rig := newIdleRig(t)
	logs := &testlog.Buffer{}
	bundle := copyRigBundle
	bundle.At = time.Now().UTC()
	bundler := &copyBundler{bundle: bundle}
	rig.w.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rig.w.copyBundler, rig.w.copyStore = bundler, rig.st
	// A copy owed behind a release's save is started by the cadence's own start once the save gives
	// the place back, as attach wires it.
	rig.w.copies.startsOwedWith(rig.w.startOwedCopy)
	clock := &heldClock{}
	rig.w.copies.now = clock.Now
	// The copies run under the daemon's run, as attach and run wire them; the test's own end is
	// that run's.
	run, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	rig.w.copies.runUnder(run)
	conn := fake.NewConn()
	conns, ok := rig.sup.deps.Conns.(*fake.Conns)
	if !ok {
		t.Fatalf("the rig's connections are %T, want the package's fake", rig.sup.deps.Conns)
	}
	conns.Register(rig.token, conn)
	return &copyRig{idleRig: rig, bundler: bundler, conn: conn, logs: logs, clock: clock}
}

// turnStart puts the issue's implementer in a turn of its phase's task, as its agent's stream
// does: the start names the delivery, so the turn is that task's and its end retires it, which
// leaves the claim free to be given the next one.
func (r *copyRig) turnStart(t *testing.T) {
	t.Helper()
	start := supervise.StreamTurnStart{Claim: r.token}
	if pending := r.machine.Claim().Pending; pending != nil {
		start.DeliveryID = pending.ID
	}
	if err := r.machine.Handle(context.Background(), start); err != nil {
		t.Fatalf("start the implementer's turn: %v", err)
	}
	r.machine.Wait()
	if got := r.machine.Claim().State; got != supervise.StateWorking {
		t.Fatalf("the claim is %s after its turn started, want working", got)
	}
}

// turnEnd ends that turn, which is where the machine starts the copy the turn's end owes, through
// the supervisor, once a test has handed it the daemon's copy (copiesTurnEnds).
func (r *copyRig) turnEnd(t *testing.T) {
	t.Helper()
	if err := r.machine.Handle(context.Background(), supervise.StreamTurnEnd{Claim: r.token}); err != nil {
		t.Fatalf("end the implementer's turn: %v", err)
	}
	r.machine.Wait()
	if got := r.machine.Claim().State; got != supervise.StateIdle {
		t.Fatalf("the claim is %s after its turn ended, want idle", got)
	}
}

// setClaimState writes the state of the issue's implementer as the store holds it, which is what
// the cadence reads to choose the role it asks: a claim whose state holds no process is one no
// shim can be asked for a bundle.
func (r *copyRig) setClaimState(t *testing.T, state string) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), `update claims set state = $2 where token = $1`,
		string(r.token), state); err != nil {
		t.Fatalf("set the implementer's stored state to %s: %v", state, err)
	}
}

// storedImplementer is the rig's implementer as the store holds it, which a test hands the copy
// of a turn's end where the store records a process the machine's own claim does not
// (setLocatorPod writes the store alone).
func (r *copyRig) storedImplementer(t *testing.T) supervise.Claim {
	t.Helper()
	return r.storedClaim(t, r.token)
}

// heldBundler makes the rig's bundler hold every copy until the call it answers is made, and
// then answer copyRigBundle.
func (r *copyRig) heldBundler() func() {
	release := make(chan struct{})
	r.bundler.holds(func(ctx context.Context) (runtime.WorkspaceBundle, error) {
		select {
		case <-release:
			return copyRigBundle, nil
		case <-ctx.Done():
			return runtime.WorkspaceBundle{}, ctx.Err()
		}
	})
	return func() { close(release) }
}

// liveArchitect records an architect of the rig's issue as the store holds a live role: idle, on
// a process of its own. Its token sorts before the implementer's, so a copy that chooses its role
// from the claims (runningCopyClaim) asks it, and nothing of it waits for any copy.
func (r *copyRig) liveArchitect(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	token := mustClaimToken(t, r.issue.Key, claim.RoleArchitect)
	loc, err := r.rt.Spawn(ctx, runtime.SpawnSpec{Claim: token, Role: claim.RoleArchitect})
	if err != nil {
		t.Fatalf("mint the architect's locator: %v", err)
	}
	stored := supervise.Claim{Token: token, Project: "legion", Tree: r.issue.Tree, Issue: r.issue.Key,
		Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
		State: supervise.StateIdle, Locator: &loc, Session: "ses-architect", SessionFile: "/tmp/architect.jsonl",
		PluginContract: api.DaemonAPIVersion}
	if err := r.st.PutClaim(ctx, stored); err != nil {
		t.Fatalf("seed the live architect: %v", err)
	}
}

// holdStop holds a daemon's suspension of the implementer for the end of the turn it is in.
func (r *copyRig) holdStop(t *testing.T) {
	t.Helper()
	if err := r.machine.Handle(context.Background(), supervise.RequestSuspend{Claim: r.token, By: supervise.SuspendedByDaemon,
		Reason: "LEGION-208 left implementing"}); !errors.Is(err, supervise.ErrSuspendHeld) {
		t.Fatalf("the suspension of the implementer in a turn = %v, want it held for the turn's end", err)
	}
}

// waitsForTheCopy fails unless the implementer is idle, holding its stop, with its process not
// stopped: its held stop is waiting for a copy.
func (r *copyRig) waitsForTheCopy(t *testing.T) {
	t.Helper()
	if testwait.Holds(time.Second, testwait.Poll, func() bool { return len(r.rt.CallsOf("Suspend")) > 0 }) {
		t.Fatalf("the implementer's process was stopped while the copy of its turn's end was still being built; the daemon logged:\n%s", r.logs.String())
	}
	if c := r.machine.Claim(); c.State != supervise.StateIdle || !c.SuspensionHeld {
		t.Fatalf("the implementer is %s (held %v) while the copy of its turn's end is built, want idle holding its stop", c.State, c.SuspensionHeld)
	}
}

// stopsOnceCopied waits for the implementer's held stop to land, and fails unless it landed once.
func (r *copyRig) stopsOnceCopied(t *testing.T) {
	t.Helper()
	testwait.Eventually(t, "the implementer's held stop to land once the copy of its turn's end has ended", func() bool {
		return r.machine.Claim().State == supervise.StateSuspended
	})
	if stops := r.rt.CallsOf("Suspend"); len(stops) != 1 {
		t.Fatalf("the implementer's process was stopped %d times, want once", len(stops))
	}
}

// heldCopy is what an issue's running copy says of itself without its bytes, read off its row:
// the store serves no such read, since nothing in the daemon decides on it but the restore's own
// transaction (store.TakeWorkspaceCopyForRestore), and a test asks what the cadence stored.
type heldCopy struct {
	Held bool
	At   time.Time
	Size int
}

// runningCopyOf is issue's running copy as st holds it, and whether it holds one at all.
func runningCopyOf(t *testing.T, st *store.Store, issue string) (heldCopy, bool) {
	t.Helper()
	held := heldCopy{Held: true}
	err := st.Pool().QueryRow(context.Background(), `select at, size_bytes from workspace_running_copies where issue = $1`, issue).
		Scan(&held.At, &held.Size)
	if errors.Is(err, pgx.ErrNoRows) {
		return heldCopy{}, false
	}
	if err != nil {
		t.Fatalf("read the running copy of %s: %v", issue, err)
	}
	return held, true
}

// facts is what the issue's running copy says of itself, and whether it holds one at all.
func (r *copyRig) facts(t *testing.T) (heldCopy, bool) {
	t.Helper()
	return runningCopyOf(t, r.st, r.issue.Key)
}

// awaitCopy waits for the issue to hold a copy of size bytes, which is how a test reads the end
// of a copy the cadence took on a goroutine of its own.
func (r *copyRig) awaitCopy(t *testing.T, size int) heldCopy {
	t.Helper()
	var facts heldCopy
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		found, held := r.facts(t)
		facts = found
		return held && found.Size == size
	}) {
		t.Fatalf("the cadence stored no copy of %d bytes; it holds %+v and logged:\n%s", size, facts, r.logs.String())
	}
	return facts
}

// awaitAsks waits for the bundler to have been asked count times.
func (r *copyRig) awaitAsks(t *testing.T, count int) {
	t.Helper()
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		return len(r.bundler.reads()) >= count
	}) {
		t.Fatalf("the bundler was asked %v, want %d asks; the daemon logged:\n%s", r.bundler.reads(), count, r.logs.String())
	}
}

// awaitQuiet waits until no copy of the issue is in flight, so a test that drives the cadence's
// own memory next is not racing the goroutine that holds it.
func (r *copyRig) awaitQuiet(t *testing.T) {
	t.Helper()
	r.awaitPlace(t, func(c runningCopy) bool { return !c.inFlight() }, "a copy of "+r.issue.Key+" is still in flight")
}

// awaitCopyOwed waits for ask to be owed behind what holds the issue's place — behind, a copy in
// flight or a release's save — which is how a test reads that a copy refused the place is owed
// rather than dropped.
func (r *copyRig) awaitCopyOwed(t *testing.T, ask, behind string) {
	t.Helper()
	r.awaitPlace(t, func(c runningCopy) bool { return c.owed != nil }, ask+" was never owed behind "+behind)
}

// awaitPlace waits until ready holds of the cadence's memory of the issue, read under its lock,
// and fails with failure and the daemon's log when it never does.
func (r *copyRig) awaitPlace(t *testing.T, ready func(runningCopy) bool, failure string) {
	t.Helper()
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		r.w.copies.mu.Lock()
		defer r.w.copies.mu.Unlock()
		return ready(r.w.copies.issues[r.issue.Key])
	}) {
		t.Fatalf("%s; the daemon logged:\n%s", failure, r.logs.String())
	}
}

// wind moves the cadence's clock a whole period on, so the next pass of the cadence finds the
// issue due: the period is counted from the start of the issue's last copy, which the cadence
// read off that clock. It waits for the copy in flight to end first, so the move lands after it.
func (r *copyRig) wind(t *testing.T) {
	t.Helper()
	r.awaitQuiet(t)
	r.clock.advance(runningCopyPeriod)
}

// asksStay fails if the bundler is asked more than count times within a second, which is how a
// test reads that a pass of the cadence started no copy: a copy runs on a goroutine of its own.
func (r *copyRig) asksStay(t *testing.T, count int) {
	t.Helper()
	if testwait.Holds(time.Second, testwait.Poll, func() bool { return len(r.bundler.reads()) > count }) {
		t.Fatalf("the bundler was asked %v, want no more than %d asks; the daemon logged:\n%s", r.bundler.reads(), count, r.logs.String())
	}
}

// lines is every log record the daemon wrote under msg, as its attributes.
func (r *copyRig) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if line == "" {
			continue
		}
		record := map[string]any{}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("read the daemon's log line %q: %v", line, err)
		}
		if record["msg"] == msg {
			found = append(found, record)
		}
	}
	return found
}

// awaitLine waits for one record under msg and returns it.
func (r *copyRig) awaitLine(t *testing.T, msg string) map[string]any {
	t.Helper()
	var found []map[string]any
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		found = r.lines(t, msg)
		return len(found) > 0
	}) {
		t.Fatalf("the daemon never logged %q; it logged:\n%s", msg, r.logs.String())
	}
	return found[len(found)-1]
}

// copiesTurnEnds hands the rig's supervisor the daemon's copy of a turn's end, as attach does
// (supervisor.copyTurnEndsWith): the rig's machine was built before the rig's workflow runtime,
// and reads the copy through the supervisor all the same.
func (r *copyRig) copiesTurnEnds() {
	r.sup.copyTurnEndsWith(r.w.copyWorkspaceAtTurnEnd)
}

// A turn that ends stores a copy of the issue's workspace. The machine starts the copy from the
// turn's end itself (supervise's turnEnded, copyTurnEnd), handing it the claim whose turn ended,
// and the daemon asks that role for the bundle of its unpushed work and stores it as the issue's
// running copy. The line it writes carries the issue, the bytes, when the copy's take began by the
// store's clock, which is the copy's own time and stands close to the start of the cadence's
// period (runningCopy.started, the cadence's own clock), and how long the bundler took, which is
// what a deployment reads the cadence by.
func TestATurnThatEndsBundlesACopyOfTheIssueWorkspace(t *testing.T) {
	rig := newCopyRig(t)
	rig.copiesTurnEnds()
	// An architect an earlier release left suspended holds no process, and the copy is asked of
	// the implementer whose turn ended although the architect's token sorts first.
	rig.releasedArchitect(t)

	rig.turnStart(t)
	rig.turnEnd(t)

	facts := rig.awaitCopy(t, len(copyRigBundle.Bundle))
	if facts.At.IsZero() {
		t.Fatalf("the stored copy %+v carries no time", facts)
	}
	if asked := rig.bundler.reads(); len(asked) != 1 || asked[0] != claim.RoleImplementer {
		t.Fatalf("the bundle was asked of %v, want the role whose turn ended", asked)
	}
	line := rig.awaitLine(t, runningCopyStoredLine)
	if got := line["issue"]; got != rig.issue.Key {
		t.Fatalf("the line names issue %v, want %s", got, rig.issue.Key)
	}
	if got, ok := line["bytes"].(float64); !ok || int(got) != len(copyRigBundle.Bundle) {
		t.Fatalf("the line reads bytes %v, want the %d the copy holds", line["bytes"], len(copyRigBundle.Bundle))
	}
	if got, ok := line["took"].(string); !ok || got == "" {
		t.Fatalf("the line reads took %v, want how long the bundler took", line["took"])
	}
	stamp, _ := line["taken"].(string)
	taken, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || !taken.Equal(facts.At) {
		t.Fatalf("the line reads taken %v (%v), want the stored copy's own time %s, when its take began",
			line["taken"], err, facts.At)
	}
}

// Five minutes inside a turn the cadence takes a copy of its own: a turn that runs for an hour
// would otherwise have its work held by no copy until it ended, which is the whole of what an
// eviction takes. The period runs from the start of the copy before it, so a pass before it is
// due takes nothing and a pass after it takes another.
func TestFiveMinutesIntoATurnTheCadenceBundlesTheWorkspaceCopyAgain(t *testing.T) {
	rig := newCopyRig(t)
	ctx := context.Background()
	rig.turnStart(t)

	rig.w.copyWorkingWorkspaces(ctx)
	rig.awaitCopy(t, len(copyRigBundle.Bundle))
	rig.awaitQuiet(t)

	// A pass inside the period takes nothing: the issue was copied a moment ago.
	rig.w.copyWorkingWorkspaces(ctx)
	rig.w.copyWorkingWorkspaces(ctx)
	if asked := rig.bundler.reads(); len(asked) != 1 {
		t.Fatalf("the bundler was asked %v inside the period, want the one copy that started it", asked)
	}

	// The period passes, and the turn is still running: the cadence takes another.
	later := copyRigBundle
	later.Bundle, later.Head, later.Tips, later.At = []byte("PACK the work of the next five minutes"), "f00d", []string{"f00d"}, time.Now().UTC()
	rig.bundler.answers(later, nil)
	rig.wind(t)
	rig.w.copyWorkingWorkspaces(ctx)

	rig.awaitCopy(t, len(later.Bundle))
	if asked := rig.bundler.reads(); len(asked) != 2 {
		t.Fatalf("the bundler was asked %v, want the copy that started the period and the one the period's end took", asked)
	}
}

// No copy is taken between turns. The copy that ends each turn holds the work of that turn, and
// the workspace of a pod whose roles are all idle changes under nobody: a cadence that copied it
// anyway would store the same bundle again every period for as long as the pod stood.
func TestNoWorkspaceCopyIsBundledBetweenTurnsOfAnIdleIssuesPod(t *testing.T) {
	rig := newCopyRig(t)
	ctx := context.Background()

	rig.w.copyWorkingWorkspaces(ctx)

	if asked := rig.bundler.reads(); len(asked) != 0 {
		t.Fatalf("the bundler was asked %v for a pod with no turn in flight, want nothing", asked)
	}
	if _, held := rig.facts(t); held {
		t.Fatal("the cadence stored a copy of an issue that is between turns")
	}

	// The same pass, with the issue's role in a turn, takes one: what the pass reads is the
	// claims the daemon's own store holds.
	rig.turnStart(t)
	rig.w.copyWorkingWorkspaces(ctx)
	rig.awaitCopy(t, len(copyRigBundle.Bundle))
}

// A copy still running when another falls due starts no second bundle of the same workspace.
// One workspace belongs to one issue and its six roles share it, so the daemon holds one copy
// in flight per issue: two runs of the shim's bundler in one workspace share the ref prefix
// `refs/legion-bundle/`, and one run's delete would land inside the other's `git bundle
// create`.
//
// What moved: the copy that mark refuses used to be dropped, and this test pinned the one
// bundle as the whole of what the daemon took. It is owed now and taken when the copy in flight
// ends, so what is pinned here is the invariant itself — the bundler is never in two runs of
// one workspace at once — and that the refusals owe one copy between them rather than one copy
// each. That the owed copy is what holds the work a turn ended with is the next test's
// (TestATurnThatEndsWhileACopyIsInFlightIsCopiedAgainOnceThatCopyEnds).
func TestACopyStillRunningStartsNoSecondWorkspaceBundleAndOwesTheRefusedOne(t *testing.T) {
	rig := newCopyRig(t)
	ctx := context.Background()
	release := make(chan struct{})
	taken := copyRigBundle
	taken.At = time.Now().UTC()
	// live is how many bundlers of this workspace are running now, and overlapped counts every
	// time a second one started while one was still building, which is the thing the ref prefix
	// cannot carry and so the thing no owed copy may buy.
	var mu sync.Mutex
	var live, overlapped int
	rig.bundler.holds(func(ctx context.Context) (runtime.WorkspaceBundle, error) {
		mu.Lock()
		live++
		if live > 1 {
			overlapped++
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			live--
			mu.Unlock()
		}()
		select {
		case <-release:
			return taken, nil
		case <-ctx.Done():
			return runtime.WorkspaceBundle{}, ctx.Err()
		}
	})
	rig.turnStart(t)

	rig.w.copyWorkingWorkspaces(ctx)
	rig.awaitAsks(t, 1)

	// Two more passes while the first copy is still being built, and the issue's turn is still
	// in flight for each of them. A pass finds the issue not due while a copy of it runs, so
	// neither reaches the mark and neither owes anything: a copy the period skipped is taken by
	// the period that follows it.
	rig.w.copyWorkingWorkspaces(ctx)
	rig.w.copyWorkingWorkspaces(ctx)
	// The turn's end asks for one, which the mark refuses and the issue owes.
	rig.w.copyWorkspaceAtTurnEnd(rig.machine.Claim(), nil)
	if asked := rig.bundler.reads(); len(asked) != 1 {
		t.Fatalf("the bundler was asked %v while one copy was still running, want the one in flight", asked)
	}

	close(release)
	rig.awaitCopy(t, len(taken.Bundle))
	// The copy in flight ends and the owed one follows it, one and not one per refusal.
	rig.awaitAsks(t, 2)
	rig.awaitQuiet(t)
	if asked := rig.bundler.reads(); len(asked) != 2 {
		t.Fatalf("the bundler was asked %v in all, want the copy in flight and the one it owed after it", asked)
	}
	mu.Lock()
	defer mu.Unlock()
	if overlapped != 0 {
		t.Fatalf("a second bundler of the workspace started %d times while one was still building, want none", overlapped)
	}
}

// A bundler that blocks past its budget delays nothing of the turn: the machine hands the copy
// to a goroutine of the daemon's, so the turn's end returns, the claim's state write lands and the
// next task is sent while the bundler is still holding. The copy itself is abandoned at its
// budget and logged under that reason, rather than running into the next period.
func TestABlockingBundlerDelaysNoTurnEndAndItsWorkspaceCopyIsAbandonedAtItsBudget(t *testing.T) {
	rig := newCopyRig(t)
	rig.copiesTurnEnds()
	rig.w.copyBudget = 2 * time.Second
	held := make(chan struct{})
	rig.bundler.holds(func(ctx context.Context) (runtime.WorkspaceBundle, error) {
		defer close(held)
		<-ctx.Done()
		return runtime.WorkspaceBundle{}, ctx.Err()
	})
	rig.turnStart(t)

	started := time.Now()
	rig.turnEnd(t)
	ended := time.Since(started)

	if ended >= rig.w.copyBudget {
		t.Fatalf("the turn's end took %s, as long as the copy's whole budget (%s): the copy delayed it", ended, rig.w.copyBudget)
	}
	var state string
	if err := rig.pool.QueryRow(context.Background(), `select state from claims where token = $1`,
		string(rig.token)).Scan(&state); err != nil {
		t.Fatalf("read the claim's stored state: %v", err)
	}
	if state != "idle" {
		t.Fatalf("the stored claim is %s while the bundler holds, want the idle its turn's end wrote", state)
	}
	// The machine goes on deciding while the bundler holds: the task that follows the turn is
	// sent rather than waiting behind the copy.
	sent := time.Now()
	if err := rig.machine.Handle(context.Background(), supervise.RequestDeliver{Claim: rig.token, Task: "the next task"}); err != nil {
		t.Fatalf("deliver the task that follows the turn: %v", err)
	}
	rig.machine.Wait()
	if took := time.Since(sent); took >= rig.w.copyBudget {
		t.Fatalf("the next task's send took %s, as long as the copy's whole budget (%s)", took, rig.w.copyBudget)
	}
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		for _, prompt := range rig.conn.Prompts() {
			if prompt.Message == "the next task" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("the task that follows the turn was never sent while the bundler held: %v", rig.conn.Prompts())
	}

	<-held
	line := rig.awaitLine(t, runningCopyUnsavedLine)
	if got := line["reason"]; got != runningCopyBudgetSpent {
		t.Fatalf("the abandoned copy is logged under reason %v, want %s", got, runningCopyBudgetSpent)
	}
	if got := line["issue"]; got != rig.issue.Key {
		t.Fatalf("the line names issue %v, want %s", got, rig.issue.Key)
	}
	if _, stored := rig.facts(t); stored {
		t.Fatal("the copy abandoned at its budget stored something")
	}
	rig.awaitQuiet(t)
}

// Each way a copy can come to nothing is logged under its own reason, stores nothing, leaves the
// issue holding the copy it had, and leaves the issue free to be copied again: the cadence is a
// saving against an eviction, so a copy that failed costs the issue nothing but the period.
//
// They are driven through the one function the cadence and a push run (takeRunningCopy) rather
// than through the tick, because the tick reaches one of them only on its retry: an issue the
// tick copies inside a turn has a claim in a turn, which is a claim with a process. The role goes
// where the process does — a stop or a death between the push's record and its read of the claims
// — which is the push's path. A workspace over the bound a copy carries is refused by the shim
// with its size, and reaches the daemon as the fake connection answers it, as production's does.
func TestEachFailedWorkspaceCopyIsLoggedByItsReasonAndLeavesTheIdleIssuesCopyAsItWas(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		// bytes is the size the line names, where the case pins one.
		bytes int
		// fail makes the copy fail that way, and mend puts back whatever it took.
		fail func(t *testing.T, rig *copyRig)
		mend func(t *testing.T, rig *copyRig)
	}{
		{
			name:   "the bundler failed",
			reason: runningCopyBundlerFailed,
			fail: func(_ *testing.T, rig *copyRig) {
				rig.bundler.answers(runtime.WorkspaceBundle{}, errors.New("the shim answered nothing"))
			},
		},
		{
			name:   "the shim refuses the unpushed work as over the bound a copy carries",
			reason: runningCopyTooLarge,
			bytes:  runtime.MaxWorkspaceBundleBytes + 1,
			fail: func(_ *testing.T, rig *copyRig) {
				over := copyRigBundle
				over.Bundle = make([]byte, runtime.MaxWorkspaceBundleBytes+1)
				rig.conn.SetWorkspaceBundle(over)
				rig.bundler.holds(func(ctx context.Context) (runtime.WorkspaceBundle, error) {
					return shimBundle(ctx, rig.conn, rig.issue.Key)
				})
			},
		},
		{
			name:   "no live role holds a process to ask",
			reason: runningCopyNoRole,
			fail: func(t *testing.T, rig *copyRig) {
				rig.setClaimState(t, "suspended")
			},
			mend: func(t *testing.T, rig *copyRig) { rig.setClaimState(t, "idle") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCopyRig(t)
			ctx := context.Background()
			kept := rig.copyWorkspace(t)
			before, held := rig.facts(t)
			if !held {
				t.Fatal("the issue holds no copy to leave alone")
			}
			tc.fail(t, rig)

			rig.w.takeRunningCopy(ctx, rig.issue.Key, nil)

			line := rig.awaitLine(t, runningCopyUnsavedLine)
			if got := line["reason"]; got != tc.reason {
				t.Fatalf("the copy that was not stored is logged under reason %v, want %s", got, tc.reason)
			}
			if got := line["issue"]; got != rig.issue.Key {
				t.Fatalf("the line names issue %v, want %s", got, rig.issue.Key)
			}
			if got, ok := line["bytes"].(float64); tc.bytes != 0 && (!ok || int(got) != tc.bytes) {
				t.Fatalf("the line reads bytes %v, want the %d the shim's refusal named", line["bytes"], tc.bytes)
			}
			after, held := rig.facts(t)
			if !held || after.Size != before.Size || !after.At.Equal(before.At) {
				t.Fatalf("the issue holds %+v (held %v), want the %d bytes it held before the failure", after, held, len(kept.Bundle))
			}

			// The next period finds the issue free and the copy is taken again.
			if tc.mend != nil {
				tc.mend(t, rig)
			}
			taken := copyRigBundle
			taken.Bundle, taken.Head, taken.Tips, taken.At = []byte("PACK the work the next period copied"), "f00d", []string{"f00d"}, time.Now().UTC()
			rig.bundler.answers(taken, nil)
			rig.w.takeRunningCopy(ctx, rig.issue.Key, nil)
			if stored, held := rig.facts(t); !held || stored.Size != len(taken.Bundle) {
				t.Fatalf("the copy of the next period = %+v (held %v), want the %d bytes it took", stored, held, len(taken.Bundle))
			}
		})
	}
}

// A copy's time is the store's clock as its take begins (store.Store.BeginWorkspaceCopy), read
// before any shim is asked, so a copy whose take cannot read that clock is no copy: the daemon asks
// no shim, logs the copy under its own reason, and the issue keeps the copy it had. A copy taken
// with no such time would have only its write to stamp it, and a copy read before a push and
// written after it would then pass the push's rule. The store here is one whose pool is closed;
// the claims are the caller's, as the cadence hands them, so the clock is the first thing the copy
// reads of it.
func TestACopyWhoseTakeCannotReadTheStoresClockAsksNoShimAndKeepsTheIssuesCopy(t *testing.T) {
	rig := newCopyRig(t)
	ctx := context.Background()
	rig.copyWorkspace(t)
	before, held := rig.facts(t)
	if !held {
		t.Fatal("the issue holds no copy to leave alone")
	}
	claims, err := rig.st.Claims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := store.Open(ctx, rig.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	rig.w.copyStore = closed

	rig.w.takeRunningCopy(ctx, rig.issue.Key, claims)

	line := rig.awaitLine(t, runningCopyUnsavedLine)
	if got := line["reason"]; got != runningCopyClockUnread {
		t.Fatalf("the copy whose take could not read the store's clock is logged under reason %v, want %s", got, runningCopyClockUnread)
	}
	if asked := rig.bundler.reads(); len(asked) != 0 {
		t.Fatalf("the bundler was asked %v, want no shim asked for a copy with no time", asked)
	}
	rig.w.copyStore = rig.st
	if after, held := rig.facts(t); !held || after.Size != before.Size || !after.At.Equal(before.At) {
		t.Fatalf("the issue holds %+v (held %v), want the copy it held before, %+v", after, held, before)
	}
}

// A workspace with nothing unpushed bundles to nothing, and the store refuses an empty copy, so
// the cadence drops the issue's running copy instead: a copy taken minutes ago holds work that
// has since been pushed, and the next pod must never restore it over the branch it was pushed to.
// It is the rule the release bundle's own save follows (outbox.bundleIssueWorkspace).
func TestAWorkspaceThatBundlesToNothingDropsTheIssuesRunningCopy(t *testing.T) {
	rig := newCopyRig(t)
	ctx := context.Background()
	rig.copyWorkspace(t)
	if _, held := rig.facts(t); !held {
		t.Fatal("the issue holds no copy for the empty bundle to drop")
	}
	empty := copyRigBundle
	empty.Bundle, empty.Head, empty.Tips, empty.At = nil, "", nil, time.Now().UTC()
	rig.bundler.answers(empty, nil)

	rig.w.takeRunningCopy(ctx, rig.issue.Key, nil)

	if facts, held := rig.facts(t); held {
		t.Fatalf("the issue still holds %+v, want the copy of work that has since been pushed dropped", facts)
	}
	line := rig.awaitLine(t, runningCopyDroppedLine)
	if got := line["issue"]; got != rig.issue.Key {
		t.Fatalf("the line names issue %v, want %s", got, rig.issue.Key)
	}
}

// The cadence is no part of the idle release: a deployment that releases no pod
// (`idle_minutes: 0`) still has pods the cluster can end under it, and the work of those pods is
// what the copies hold. So the pass takes its copy with release off, which is what wiring it in
// attach rather than attachIdleRelease buys.
func TestTheWorkspaceCopyIsBundledWithIdleReleaseOff(t *testing.T) {
	rig := newCopyRig(t)
	rig.w.idlePeriod, rig.w.idleRuntime = 0, nil
	rig.turnStart(t)

	rig.w.copyWorkingWorkspaces(context.Background())

	rig.awaitCopy(t, len(copyRigBundle.Bundle))
}

// The Envoy listener's liveness gate is the idle release's and not the cadence's. A deployment
// whose listener has not reported inside the window releases no pod, because a released pod is
// woken by nothing; its pods go on working all the same, and the work of those pods is what a
// copy holds against an eviction. So the pass takes its copy with the listener silent — the gate
// would otherwise cost every in-turn copy of such a deployment while the copy each turn's end
// takes went on running.
func TestTheWorkspaceCopyIsBundledWhileTheEnvoyListenerIsSilent(t *testing.T) {
	rig := newCopyRig(t)
	recordListenerReady(t, rig.pool, time.Now().UTC().Add(-listenerReadyWindow-time.Minute))
	if rig.w.listenerLive(context.Background()) {
		t.Fatal("the listener still reads live; this case is about a deployment whose listener has gone quiet")
	}
	rig.turnStart(t)

	rig.w.copyWorkingWorkspaces(context.Background())

	rig.awaitCopy(t, len(copyRigBundle.Bundle))
}

// The cadence's own memory holds one entry per issue it has copied, and an entry whose copy is
// not in flight, that owes no retry and whose period has passed answers due exactly as a missing
// one does. Such an entry is dropped where the map is written, so a daemon that runs for weeks
// holds entries for the issues it is copying rather than for every issue it has ever copied. An
// entry whose last copy came to nothing is kept whatever its age: dropped, it would owe no retry,
// and an issue between turns would never be copied again.
func TestTheCadenceForgetsAnIssueWhoseWorkspaceCopyPeriodHasPassed(t *testing.T) {
	clock := &heldClock{}
	clock.advance(0)
	copies := runningCopies{now: clock.Now}
	for _, issue := range []string{"INFRA-1", "INFRA-2", "INFRA-4"} {
		if !copies.begin(copyAsk{of: supervise.Claim{Issue: issue}}) {
			t.Fatalf("the cadence refused the first copy of %s", issue)
		}
	}
	copies.done("INFRA-1", false)
	copies.done("INFRA-2", false)
	copies.done("INFRA-4", true)
	clock.advance(2 * runningCopyPeriod)
	if !copies.begin(copyAsk{of: supervise.Claim{Issue: "INFRA-3"}}) {
		t.Fatal("the cadence refused the first copy of INFRA-3")
	}

	copies.done("INFRA-3", false)

	copies.mu.Lock()
	held := slices.Sorted(maps.Keys(copies.issues))
	copies.mu.Unlock()
	if want := []string{"INFRA-3", "INFRA-4"}; !slices.Equal(held, want) {
		t.Fatalf("the cadence remembers %v, want %v: an entry whose period has passed is dropped where the map is written, unless it owes a retry", held, want)
	}
	// The issues it forgot are due at once, which is what the entries it dropped said too.
	now := clock.Now()
	for _, issue := range []string{"INFRA-1", "INFRA-2"} {
		if !copies.due(issue, now, idleTickInterval) {
			t.Fatalf("%s is not due though the cadence forgot it; a dropped entry must answer as a missing one", issue)
		}
	}
	if copies.due("INFRA-3", now, idleTickInterval) {
		t.Fatal("the copy that just ended is due again at once")
	}
	if owed := copies.retries(); !slices.Equal(owed, []string{"INFRA-4"}) {
		t.Fatalf("the cadence owes retries of %v, want the one issue whose copy came to nothing", owed)
	}
}

// What does turn the cadence off is having nothing to ask or nowhere to put the answer: a runtime
// that bundles no workspace, which is `runtime: tmux`, and a daemon with no store. Either leaves
// the tick running no ticker at all, as release's own tick does where release is off.
func TestTheWorkspaceCopyTickDoesNothingWithoutABundlerOrAStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    *workflowRuntime
	}{
		{name: "the runtime bundles no workspace", w: &workflowRuntime{copyStore: &store.Store{}}},
		{name: "no store", w: &workflowRuntime{copyBundler: &copyBundler{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			go func() { defer close(done); tc.w.tickWorkspaceCopies(context.Background()) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("tickWorkspaceCopies kept running with no bundler or no store")
			}
		})
	}
}

// A turn that ends while the cadence's copy of its issue is still being built is copied again
// once that copy ends. The copy in flight was read before the turn's last work was written, and
// the cadence takes no copy between turns of an issue whose last copy was stored, so without a
// copy after it the newest copy of a pod whose roles are all idle is not the one its last turn
// ended with, which is what the daemon holds itself to (dispatch://INFRA-1117/spec#b-r1): an
// eviction of that idle pod then loses the end of the turn. One copy of an issue is still in
// flight at a time; the turn's end is owed, not dropped.
func TestATurnThatEndsWhileACopyIsInFlightIsCopiedAgainOnceThatCopyEnds(t *testing.T) {
	rig := newCopyRig(t)
	rig.copiesTurnEnds()
	ctx := context.Background()
	midTurn := copyRigBundle
	midTurn.At = time.Now().UTC()
	ended := copyRigBundle
	ended.Bundle, ended.Head, ended.Tips = []byte("PACK the work the turn ended with, written after the copy in flight read the workspace"), "beef", []string{"beef"}
	release := make(chan struct{})
	rig.bundler.holds(func(ctx context.Context) (runtime.WorkspaceBundle, error) {
		select {
		case <-release:
			// The workspace from now on holds the work the turn ended with.
			later := ended
			later.At = time.Now().UTC()
			rig.bundler.answers(later, nil)
			return midTurn, nil
		case <-ctx.Done():
			return runtime.WorkspaceBundle{}, ctx.Err()
		}
	})
	rig.turnStart(t)

	// The cadence's copy inside the turn starts, and is still being built when the turn ends.
	rig.w.copyWorkingWorkspaces(ctx)
	rig.awaitAsks(t, 1)
	rig.turnEnd(t)

	close(release)
	// The issue is between turns now and the copy in flight is stored, so no pass of the cadence
	// would copy it again: the copy the turn's end owed is the only one that can hold that work.
	rig.awaitCopy(t, len(ended.Bundle))
	if asked := rig.bundler.reads(); len(asked) != 2 {
		t.Fatalf("the bundler was asked %v, want the copy in flight and the one the turn's end owed after it", asked)
	}
}

// Inside a turn the cadence starts its copies of one issue at most a period apart when the
// daemon's tick arrives on time, and never less than a period less one tick apart
// (runningCopyPeriod). The period is counted from the start of the copy before it: a copy holds
// the workspace as it was when it started, so an eviction loses the work since that start, and a
// period counted from the copy's end would let a copy that took two minutes put the next start
// seven minutes after its own. Here the first copy takes two minutes of the cadence's clock, and
// the tick comes every minute, on the minute.
func TestTheCadenceStartsTheCopiesOfOneTurnAtMostAPeriodApart(t *testing.T) {
	rig := newCopyRig(t)
	ctx := context.Background()
	tick := rig.w.copyTick()
	rig.clock.advance(0)
	// starts is the cadence's clock as the bundler is asked for each copy, which is when that copy
	// took the issue's place: the clock does not move while a copy is being started.
	var mu sync.Mutex
	var starts []time.Time
	release := make(chan struct{})
	rig.bundler.holds(func(ctx context.Context) (runtime.WorkspaceBundle, error) {
		mu.Lock()
		starts = append(starts, rig.clock.Now())
		first := len(starts) == 1
		mu.Unlock()
		if first {
			select {
			case <-release:
			case <-ctx.Done():
				return runtime.WorkspaceBundle{}, ctx.Err()
			}
		}
		taken := copyRigBundle
		taken.At = time.Now().UTC()
		return taken, nil
	})
	started := func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(starts)
	}
	// pass is one tick of the cadence at the clock's time. A copy the tick starts reads the clock
	// as it takes the issue's place, so the test moves the clock on only once that copy holds the
	// place, or the tick found the issue not due.
	pass := func() {
		t.Helper()
		rig.w.copyWorkingWorkspaces(ctx)
		if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
			return !rig.w.copies.due(rig.issue.Key, rig.clock.Now(), tick)
		}) {
			t.Fatalf("the copy the tick at %s found due never started; the daemon logged:\n%s", rig.clock.Now(), rig.logs.String())
		}
	}
	rig.turnStart(t)

	pass()
	rig.awaitAsks(t, 1)
	// The first copy is still being built at the next two ticks, and ends after the second.
	for range 2 {
		rig.clock.advance(tick)
		pass()
	}
	close(release)
	rig.awaitQuiet(t)
	for minute := 3; len(started()) < 2 && minute <= 10; minute++ {
		rig.clock.advance(tick)
		pass()
		rig.awaitQuiet(t)
	}

	got := started()
	if len(got) < 2 {
		t.Fatalf("the cadence started %d copies in ten minutes of one turn, want a second one; the daemon logged:\n%s", len(got), rig.logs.String())
	}
	if apart := got[1].Sub(got[0]); apart > runningCopyPeriod || apart < runningCopyPeriod-tick {
		t.Fatalf("the turn's second copy started %s after its first, want at most %s and no less than %s", apart, runningCopyPeriod, runningCopyPeriod-tick)
	}
}
