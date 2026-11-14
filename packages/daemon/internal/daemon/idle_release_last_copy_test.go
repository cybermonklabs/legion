package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// saveRelease queues the release of the rig's idle pod and runs its save alone, which opens the
// release's window: the bundle is stored, and the role stops queued behind it have not run, so
// the pod's roles are still live.
func (r *copyRig) saveRelease(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	r.w.releaseIdlePods(ctx)
	due, err := r.runner.listDue(ctx, "", nil, r.runner.now().UTC())
	if err != nil {
		t.Fatalf("list the release's rows: %v", err)
	}
	if len(due) == 0 || due[0].Kind != record.OutboxKindIssueBundle {
		t.Fatalf("the release's first due row = %+v, want its save", due)
	}
	if err := r.runner.runRow(ctx, due[0]); err != nil {
		t.Fatalf("run the release's save: %v", err)
	}
	if !r.bundled(t) {
		t.Fatal("the release's save stored no bundle")
	}
}

// pushEngine is the workflow's push handler as the daemon wires it, with the daemon's store as
// its record and the copy each recorded push asks for, and the call that applies one push of the
// issue's branch from before to after at the daemon's clock at.
func (r *copyRig) pushEngine(t *testing.T) func(before, after string, at time.Time) {
	clock := time.Now().UTC()
	engine := workflow.New(r.issues, workflow.Config{Project: "LEGION", Clock: func() time.Time { return clock },
		Pushes: r.st, PushRecorded: r.w.copyWorkspaceAtPush}, quietLogger())
	return func(before, after string, at time.Time) {
		t.Helper()
		clock = at
		push := intake.Push{Repo: r.issue.GitHubRepo, Branch: "legion/" + r.issue.Key, Before: before, After: after}
		if _, err := intake.ApplyFact(context.Background(), r.pool, "github", "push-webhook:"+before+".."+after, push, engine); err != nil {
			t.Fatalf("apply the push of %s..%s: %v", before, after, err)
		}
	}
}

// unsavedFor is every line of a copy that was not stored for reason.
func (r *copyRig) unsavedFor(t *testing.T, reason string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range r.lines(t, runningCopyUnsavedLine) {
		if line["reason"] == reason {
			found = append(found, line)
		}
	}
	return found
}

// awaitUnsaved waits for count lines of a copy that was not stored for reason, and answers them.
func (r *copyRig) awaitUnsaved(t *testing.T, reason string, count int) []map[string]any {
	t.Helper()
	var found []map[string]any
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		found = r.unsavedFor(t, reason)
		return len(found) >= count
	}) {
		t.Fatalf("the daemon logged %d copies not stored for %s, want %d; it logged:\n%s", len(found), reason, count, r.logs.String())
	}
	return found
}

// noWarnedCopy fails on any line of a copy not stored that was written at Warn: a copy the
// release's bundle refused is no fault, and nothing else here fails.
func (r *copyRig) noWarnedCopy(t *testing.T) {
	t.Helper()
	for _, line := range r.lines(t, runningCopyUnsavedLine) {
		if line["level"] == "WARN" {
			t.Fatalf("a copy was logged as a failure: %v", line)
		}
	}
}

// setLocatorPod records the issue's implementer as a process of pod, as the Kubernetes runtime
// records a role's process (runtime.SandboxLocator). That record is what the cadence reads the pod
// a copy would be taken off from, and the runtime's bundler names the copy by the same one.
func (r *copyRig) setLocatorPod(t *testing.T, pod string) {
	t.Helper()
	loc := runtime.Locator{Runtime: runtime.RuntimeSandbox, Claim: r.token, Incarnation: runtime.SandboxIncarnation(pod, 1),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: r.rt.sandboxName, PodUID: pod,
			Container: runtime.IssuePodContainer, Generation: 1}}
	encoded, err := json.Marshal(loc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), `update claims set locator = $2 where token = $1`,
		string(r.token), encoded); err != nil {
		t.Fatalf("record the implementer as a process of pod %s: %v", pod, err)
	}
}

// Once an idle release has saved its bundle, that bundle is the pod's last copy. A push the daemon
// records in the release's window — after the save, with the roles still live before their stops
// — takes no copy, and a second push after the stops and the teardown finds no live role and takes
// none either. The pod that replaces the released one is then sent the bundle, older than both
// pushes, and the confirmation of its restore lifts the hold the bundle's row puts on the issue's
// pod. Were the window's copy stored, it would be the later of the two by the clock and the bundle
// would be answered superseded; the second push would then refuse that copy as older than it, the
// next pod would be sent nothing at all, and the bundle's row would hold the issue's pod for good.
//
// The refusal is logged at Info under a reason of its own, never as a failure a deployment counts.
func TestASecondPushAfterAnIdleReleaseStillLeavesTheNextPodItsReleaseBundle(t *testing.T) {
	rig := newCopyRig(t)
	ctx := context.Background()
	// Whole seconds, which the store keeps exactly.
	saved := time.Now().UTC().Truncate(time.Second).Add(-30 * time.Minute)
	rig.rt.bundle.At = saved
	copied := copyRigBundle
	copied.At = saved.Add(2 * time.Minute)
	rig.bundler.answers(copied, nil)
	apply := rig.pushEngine(t)

	rig.saveRelease(t)

	// A push in the window, with the pod's roles still live.
	apply("c0", "c2", saved.Add(time.Minute))
	refused := rig.awaitUnsaved(t, runningCopyReleaseBundled, 1)
	rig.awaitQuiet(t)
	if line := refused[0]; line["level"] != "INFO" || line["issue"] != rig.issue.Key {
		t.Fatalf("the refused copy was logged as %v, want it at INFO naming %s", line, rig.issue.Key)
	}
	if rig.copied(t) {
		t.Fatal("a copy taken off the released pod after its bundle was saved was stored")
	}

	// The role stops and the teardown.
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() {
		t.Fatalf("the implementer is %s by %s after the release, want it released", got.State, got.SuspendedBy)
	}
	if _, suspended := rig.rt.reads(); suspended != 1 {
		t.Fatalf("Sandbox teardowns = %d, want the release's one", suspended)
	}

	// A second push, after the release: no role is live, and nothing is asked.
	asked := len(rig.bundler.reads())
	apply("c2", "c3", saved.Add(5*time.Minute))
	rig.awaitUnsaved(t, runningCopyNoRole, 1)
	rig.awaitQuiet(t)
	if got := rig.bundler.reads(); len(got) != asked {
		t.Fatalf("the bundler was asked %v after the release, want nothing more than the %d asks before it", got, asked)
	}
	if rig.copied(t) {
		t.Fatal("the push after the release stored a copy")
	}
	rig.noWarnedCopy(t)

	// The pod that replaces the released one is sent the bundle against the second push, and its
	// launcher's report of the restore moves the row out of the bundle table.
	restore, err := rig.st.TakeWorkspaceCopyForRestore(ctx, rig.issue.Key, "pod-that-replaces-it", saved.Add(20*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if restore.Push.Head != "c3" || !restore.Push.At.Equal(saved.Add(5*time.Minute)) {
		t.Fatalf("the restore read the push %+v, want the second push of c3", restore.Push)
	}
	sent := restore.ReleaseBundle
	if sent.Take != runtime.WorkspaceBundleSend || restore.Offered != runtime.WorkspaceCopyReleaseBundle ||
		string(sent.Bundle.Bundle) != string(rig.rt.bundle.Bundle) {
		t.Fatalf("the release bundle was answered %v with %q (%q offered), want it offered and sent whole", sent.Take, sent.Bundle.Bundle, restore.Offered)
	}
	if restore.Running.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("the running copy was answered %v, want none held", restore.Running.Take)
	}
	if err := rig.st.KeepRestoredWorkspaceCopy(ctx, rig.issue.Key, "pod-that-replaces-it", sent.Bundle.Release); err != nil {
		t.Fatalf("confirm the restore: %v", err)
	}
	if rig.bundled(t) {
		t.Fatalf("after the next pod restored the release bundle, its row still holds the issue's pod (%s)", workflow.IdleHoldBundleUnrestored)
	}
}

// No shim is asked for a copy of the pod an idle release has saved its bundle off. The copy reads
// the pod its role's process is recorded in — the pod the runtime's bundler names the copy by —
// before it takes the issue's place in flight, so no bundle is built in a workspace whose release
// is about to stop its roles. Every way a copy is asked for meets that read: a push, a turn's end
// and the cadence inside a turn, each logged at Info and none as a failure. A pod the bundle did
// not come off is asked as before: the pod that replaces the released one, once it has asked for
// the bundle, is copied while it works.
func TestNoShimIsAskedForACopyOfThePodAnIdleReleaseSavedItsBundleOff(t *testing.T) {
	rig := newCopyRig(t)
	ctx := context.Background()
	apply := rig.pushEngine(t)
	rig.saveRelease(t)
	rig.setLocatorPod(t, releasedPodUID)

	apply("c0", "c2", time.Now().UTC())
	rig.awaitUnsaved(t, runningCopyReleaseBundled, 1)
	rig.w.copyWorkspaceAtTurnEnd(rig.machine.Claim(), supervise.StateIdle)
	rig.awaitUnsaved(t, runningCopyReleaseBundled, 2)
	// The release's save started the issue's period as it gave the issue's place back, so the
	// cadence finds the issue due only once that period has passed.
	rig.setClaimState(t, "working")
	rig.wind(t)
	rig.w.copyWorkingWorkspaces(ctx)
	refused := rig.awaitUnsaved(t, runningCopyReleaseBundled, 3)
	rig.awaitQuiet(t)
	if asked := rig.bundler.reads(); len(asked) != 0 {
		t.Fatalf("the bundler was asked %v for a copy of the released pod", asked)
	}
	for _, line := range refused {
		if line["level"] != "INFO" || line["issue"] != rig.issue.Key || line["pod"] != releasedPodUID {
			t.Fatalf("a refused copy was logged as %v, want it at INFO naming %s and the pod %s", line, rig.issue.Key, releasedPodUID)
		}
	}
	rig.noWarnedCopy(t)
	if rig.copied(t) {
		t.Fatal("a copy of the released pod was stored")
	}

	// The pod that replaces the released one asks for the bundle and works on.
	restore, err := rig.st.TakeWorkspaceCopyForRestore(ctx, rig.issue.Key, "pod-that-replaces-it", time.Now().UTC().Add(time.Hour))
	if err != nil || restore.ReleaseBundle.Take != runtime.WorkspaceBundleSend {
		t.Fatalf("the next pod was answered %+v (%v), want the release bundle sent", restore, err)
	}
	rig.setLocatorPod(t, "pod-that-replaces-it")
	next := copyRigBundle
	next.Bundle, next.At, next.PodUID = []byte("PACK the next pod's work"), time.Now().UTC(), "pod-that-replaces-it"
	rig.bundler.answers(next, nil)
	rig.w.copyWorkspaceAtTurnEnd(rig.machine.Claim(), supervise.StateIdle)
	rig.awaitCopy(t, len(next.Bundle))
	if asked := rig.bundler.reads(); len(asked) != 1 {
		t.Fatalf("the bundler was asked %v, want the one copy of the next pod", asked)
	}
}
