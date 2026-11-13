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
// of the test's own and a log the test reads back as records. Its issue is the rig's — one live
// implementer, idle with its phase's task pending, on a pod that has been quiet — so a test that
// wants a turn in flight starts one (turnStart).
type copyRig struct {
	*idleRig
	bundler *copyBundler
	conn    *fake.Conn
	logs    *testlog.Buffer
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
	return &copyRig{idleRig: rig, bundler: bundler, conn: conn, logs: logs}
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

// turnEnd ends that turn, which is where the daemon is told the claim is durably idle.
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
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		r.w.copies.mu.Lock()
		defer r.w.copies.mu.Unlock()
		return !r.w.copies.issues[r.issue.Key].inFlight()
	}) {
		t.Fatalf("a copy of %s is still in flight; the daemon logged:\n%s", r.issue.Key, r.logs.String())
	}
}

// wind puts the issue's last copy a whole period in the past, so the next pass of the cadence
// finds it due. It is the one thing a test cannot drive with the clock: the period is measured
// from the end of the copy before it, which this process recorded.
func (r *copyRig) wind(t *testing.T) {
	t.Helper()
	r.awaitQuiet(t)
	r.w.copies.done(r.issue.Key, time.Now().Add(-runningCopyPeriod))
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

// A turn that ends stores a copy of the issue's workspace. The claim's idle is reported through
// the daemon's one durable-state callback (supervise's turnEnded, claimDurableState), the daemon
// asks the first live role of the issue with a process for the bundle of its unpushed work, and
// stores it as the issue's running copy. The line it writes carries the issue, the bytes and how
// long the bundler took, which is what a deployment reads the cadence by.
func TestATurnThatEndsBundlesACopyOfTheIssueWorkspace(t *testing.T) {
	rig := newCopyRig(t)
	rig.sup.OnDurableState(rig.w.claimDurableState)
	// An architect an earlier release left suspended holds no process, so the copy is asked of
	// the implementer although the architect's token sorts first.
	rig.releasedArchitect(t)

	rig.turnStart(t)
	rig.turnEnd(t)

	facts := rig.awaitCopy(t, len(copyRigBundle.Bundle))
	if facts.At.IsZero() {
		t.Fatalf("the stored copy %+v carries no time", facts)
	}
	if asked := rig.bundler.reads(); len(asked) != 1 || asked[0] != claim.RoleImplementer {
		t.Fatalf("the bundle was asked of %v, want the issue's one live role with a process", asked)
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
}

// Five minutes inside a turn the cadence takes a copy of its own: a turn that runs for an hour
// would otherwise have its work held by no copy until it ended, which is the whole of what an
// eviction takes. The period runs from the end of the copy before it, so a pass before it has
// passed takes nothing and a pass after it takes another.
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
	rig.w.copyWorkspaceAtTurnEnd(rig.machine.Claim(), supervise.StateIdle)
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

// A bundler that blocks past its budget delays nothing of the turn: the callback hands the copy
// to a goroutine of its own, so the turn's end returns, the claim's state write lands and the
// next task is sent while the bundler is still holding. The copy itself is abandoned at its
// budget and logged under that reason, rather than running into the next period.
func TestABlockingBundlerDelaysNoTurnEndAndItsWorkspaceCopyIsAbandonedAtItsBudget(t *testing.T) {
	rig := newCopyRig(t)
	rig.sup.OnDurableState(rig.w.claimDurableState)
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
// The three are driven through the one function every path runs (takeRunningCopy) rather than
// through the tick, because one of them cannot be reached from a tick at all: an issue the tick
// copies is an issue with a turn in flight, and a claim in a turn is a claim with a process. The
// role goes where the process does — a death between the report of the turn's end and the read of
// the claims — which is the turn's end path.
func TestEachFailedWorkspaceCopyIsLoggedByItsReasonAndLeavesTheIdleIssuesCopyAsItWas(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
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
			name:   "the unpushed work is over the bound a copy carries",
			reason: runningCopyTooLarge,
			fail: func(_ *testing.T, rig *copyRig) {
				over := copyRigBundle
				over.Bundle, over.At = make([]byte, runtime.MaxWorkspaceBundleBytes+1), time.Now().UTC()
				rig.bundler.answers(over, nil)
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

			rig.w.takeRunningCopy(ctx, rig.issue.Key, nil, slog.LevelWarn)

			line := rig.awaitLine(t, runningCopyUnsavedLine)
			if got := line["reason"]; got != tc.reason {
				t.Fatalf("the copy that was not stored is logged under reason %v, want %s", got, tc.reason)
			}
			if got := line["issue"]; got != rig.issue.Key {
				t.Fatalf("the line names issue %v, want %s", got, rig.issue.Key)
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
			rig.w.takeRunningCopy(ctx, rig.issue.Key, nil, slog.LevelWarn)
			if stored, held := rig.facts(t); !held || stored.Size != len(taken.Bundle) {
				t.Fatalf("the copy of the next period = %+v (held %v), want the %d bytes it took", stored, held, len(taken.Bundle))
			}
		})
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

	rig.w.takeRunningCopy(ctx, rig.issue.Key, nil, slog.LevelWarn)

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
// not in flight and whose period has passed answers due exactly as a missing one does. Such an
// entry is dropped where the map is written, so a daemon that runs for weeks holds entries for
// the issues it is copying rather than for every issue it has ever copied.
func TestTheCadenceForgetsAnIssueWhoseWorkspaceCopyPeriodHasPassed(t *testing.T) {
	var copies runningCopies
	now := time.Now()
	for _, issue := range []string{"INFRA-1", "INFRA-2"} {
		if !copies.begin(issue) {
			t.Fatalf("the cadence refused the first copy of %s", issue)
		}
		copies.done(issue, now.Add(-2*runningCopyPeriod))
	}
	if !copies.begin("INFRA-3") {
		t.Fatal("the cadence refused the first copy of INFRA-3")
	}

	copies.done("INFRA-3", now)

	copies.mu.Lock()
	held := slices.Sorted(maps.Keys(copies.issues))
	copies.mu.Unlock()
	if want := []string{"INFRA-3"}; !slices.Equal(held, want) {
		t.Fatalf("the cadence remembers %v, want %v: an entry whose period has passed is dropped where the map is written", held, want)
	}
	// The issues it forgot are due at once, which is what the entries it dropped said too.
	for _, issue := range []string{"INFRA-1", "INFRA-2"} {
		if !copies.due(issue, now) {
			t.Fatalf("%s is not due though the cadence forgot it; a dropped entry must answer as a missing one", issue)
		}
	}
	if copies.due("INFRA-3", now) {
		t.Fatal("the copy that just ended is due again at once")
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
// the cadence takes no copy of an issue between turns, so without a copy after it the newest
// copy of a pod whose roles are all idle is not the one its last turn ended with, which is what
// the daemon holds itself to (dispatch://INFRA-1117/spec#b-r1): an eviction of that idle pod
// then loses the end of the turn. One copy of an issue is still in flight at a time; the turn's
// end is owed, not dropped.
func TestATurnThatEndsWhileACopyIsInFlightIsCopiedAgainOnceThatCopyEnds(t *testing.T) {
	rig := newCopyRig(t)
	rig.sup.OnDurableState(rig.w.claimDurableState)
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
	// The issue is between turns now, so no pass of the cadence would copy it again: the copy
	// the turn's end owed is the only one that can hold that work.
	rig.awaitCopy(t, len(ended.Bundle))
	if asked := rig.bundler.reads(); len(asked) != 2 {
		t.Fatalf("the bundler was asked %v, want the copy in flight and the one the turn's end owed after it", asked)
	}
}
