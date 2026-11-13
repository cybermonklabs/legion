package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// concurrency counts the bundler runs in flight at once and the most there ever were, which is
// what says two of them never shared the workspace.
type concurrency struct {
	now, most atomic.Int32
}

// enter counts one run in, and answers the call that counts it out.
func (c *concurrency) enter() func() {
	n := c.now.Add(1)
	for {
		most := c.most.Load()
		if n <= most || c.most.CompareAndSwap(most, n) {
			break
		}
	}
	return func() { c.now.Add(-1) }
}

// A push of the issue's branch that the daemon records takes a copy of the issue's workspace once
// the push fact has committed, through the cadence's own single flight, so the copy the next pod
// is sent is one taken after the push and holding it.
//
// The push arrives here while a copy is already being built — the cadence's copy inside a turn —
// which is the case the single flight is for: the push's copy waits behind it and never runs
// beside it in the one workspace, and the copy that was in flight is not stored, since it read the
// workspace before the push and may hold none of what was pushed. The copy the push asked for
// reads the push's own record when it runs, which is what says it started after the commit. The
// issue has no pull request: a branch is pushed before its pull request opens, and the record is
// the push's own.
func TestAPushTheDaemonRecordsBundlesACopyAfterItsCommitThroughTheSingleFlight(t *testing.T) {
	rig := newCopyRig(t)
	ctx := context.Background()
	engine := workflow.New(rig.issues, workflow.Config{Project: "LEGION", Pushes: rig.st, PushRecorded: rig.w.copyWorkspaceAtPush}, quietLogger())
	beforePush := copyRigBundle
	beforePush.Bundle = []byte("PACK the workspace as it was read before the push")
	atPush := copyRigBundle
	atPush.Bundle, atPush.Head, atPush.Tips = []byte("PACK the workspace after the push"), "c2c0ffee", []string{"c2c0ffee"}
	var runs concurrency
	release := make(chan struct{})
	pushRead := make(chan error, 1)
	rig.bundler.holds(func(ctx context.Context) (runtime.WorkspaceBundle, error) {
		defer runs.enter()()
		select {
		case <-release:
		case <-ctx.Done():
			return runtime.WorkspaceBundle{}, ctx.Err()
		}
		// The copy owed behind this one reads the workspace after the push.
		rig.bundler.holds(func(ctx context.Context) (runtime.WorkspaceBundle, error) {
			defer runs.enter()()
			var pushed time.Time
			pushRead <- rig.pool.QueryRow(ctx, `select pushed_at from issue_pushes where issue = $1`, rig.issue.Key).Scan(&pushed)
			taken := atPush
			taken.At = time.Now().UTC()
			return taken, nil
		})
		taken := beforePush
		taken.At = time.Now().UTC()
		return taken, nil
	})
	rig.turnStart(t)
	rig.w.copyWorkingWorkspaces(ctx)
	rig.awaitAsks(t, 1)

	push := intake.Push{Repo: rig.issue.GitHubRepo, Branch: "legion/" + rig.issue.Key, Before: "c1", After: "c2"}
	if _, err := intake.ApplyFact(ctx, rig.pool, "github", "push-of-the-issue-branch", push, engine); err != nil {
		t.Fatalf("apply the push: %v", err)
	}
	var pushedAt time.Time
	var head string
	if err := rig.pool.QueryRow(ctx, `select pushed_at, head from issue_pushes where issue = $1`, rig.issue.Key).Scan(&pushedAt, &head); err != nil || head != "c2" {
		t.Fatalf("the push of an issue with no pull request recorded head %q (%v), want c2", head, err)
	}
	close(release)

	stored := rig.awaitCopy(t, len(atPush.Bundle))
	select {
	case err := <-pushRead:
		if err != nil {
			t.Fatalf("the copy the push asked for could not read the push's record when it ran: %v", err)
		}
	default:
		t.Fatal("the copy the push asked for was never taken")
	}
	if asked := rig.bundler.reads(); len(asked) != 2 {
		t.Fatalf("the bundler was asked %v, want the copy in flight and the push's copy after it", asked)
	}
	if most := runs.most.Load(); most != 1 {
		t.Fatalf("%d copies were built in the workspace at once, want one at a time", most)
	}
	if !stored.At.After(pushedAt) {
		t.Fatalf("the stored copy was taken at %s, want it after the push recorded at %s", stored.At, pushedAt)
	}
	rig.awaitLine(t, runningCopyOvertakenLine)
	if lines := rig.lines(t, runningCopyStoredLine); len(lines) != 1 {
		t.Fatalf("the cadence stored %d copies, want only the one taken after the push: the copy read before it is not stored", len(lines))
	}
	// It is the copy the pod that replaces this one is sent: it is not older than the push.
	restore, err := rig.st.TakeWorkspaceCopyForRestore(ctx, rig.issue.Key, "pod-that-replaces-it", time.Now().UTC().Add(time.Hour))
	if sent := restore.Running; err != nil || sent.Take != runtime.WorkspaceBundleSend || string(sent.Bundle.Bundle) != string(atPush.Bundle) {
		t.Fatalf("the pod that replaces this one was answered %q (%v, %v), want the copy taken after the push", sent.Bundle.Bundle, sent.Take, err)
	}
}

// A release's save takes the same per-issue mark a running copy takes, so it never runs the
// shim's bundler beside a copy in the one workspace: the two share the ref prefix
// `refs/legion-bundle/`, and only timing kept them apart before — a turn's end and the copy it
// was owed can take two copy budgets, more than a short `idle_minutes`. The save waits for the
// copy in flight, then bundles, and its stored bundle drops the copy that finished just before it.
func TestAReleaseSaveWaitsForTheRunningCopyBundlerInFlightBeforeItBundles(t *testing.T) {
	rig := newCopyRig(t)
	ctx := context.Background()
	release := make(chan struct{})
	rig.bundler.holds(func(ctx context.Context) (runtime.WorkspaceBundle, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return runtime.WorkspaceBundle{}, ctx.Err()
		}
		taken := copyRigBundle
		taken.At = time.Now().UTC()
		return taken, nil
	})
	// The turn that ended last left its copy being built.
	rig.w.copyWorkspaceAtTurnEnd(rig.machine.Claim(), supervise.StateIdle)
	rig.awaitAsks(t, 1)

	rig.w.releaseIdlePods(ctx)
	drained := make(chan error, 1)
	go func() {
		for range 6 {
			if err := rig.runner.RunOnce(context.Background()); err != nil {
				drained <- err
				return
			}
		}
		drained <- nil
	}()
	if testwait.Holds(2*time.Second, testwait.Poll, func() bool {
		bundled, _ := rig.rt.reads()
		return len(bundled) > 0
	}) {
		t.Fatal("the release's save ran the bundler while a running copy was being built in the same workspace")
	}
	close(release)
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("run the outbox: %v", err)
		}
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
		t.Fatal("the release's save never ran once the running copy ended")
	}
	if bundled, _ := rig.rt.reads(); len(bundled) != 1 {
		t.Fatalf("the release's save asked its bundler %d times, want once, after the copy ended", len(bundled))
	}
	if !rig.bundled(t) {
		t.Fatal("the release stored no bundle")
	}
	if rig.copied(t) {
		t.Fatal("the running copy that finished before the save outlived the bundle that supersedes it")
	}
}

// The copies a turn's end and a push start are given no context by the callbacks that start them,
// so they run under the daemon's run: its end ends a copy in flight at once rather than at the
// copy's budget, and the copy is not reported as a failed save against a store that is closing.
// Once the run has ended, no copy is started at all.
func TestTheWorkspaceCopyBundlerEndsWithTheDaemonsRun(t *testing.T) {
	rig := newCopyRig(t)
	run, stop := context.WithCancel(context.Background())
	rig.w.copies.runUnder(run)
	ended := make(chan error, 1)
	rig.bundler.holds(func(ctx context.Context) (runtime.WorkspaceBundle, error) {
		<-ctx.Done()
		ended <- ctx.Err()
		return runtime.WorkspaceBundle{}, ctx.Err()
	})
	rig.w.copyWorkspaceAtTurnEnd(rig.machine.Claim(), supervise.StateIdle)
	rig.awaitAsks(t, 1)

	stop()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the copy in flight ended with %v, want the run's own end", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the copy in flight outlived the run; its budget is %s", rig.w.runningCopyBudget())
	}
	rig.awaitLine(t, runningCopyStoppedLine)
	rig.awaitQuiet(t)
	if lines := rig.lines(t, runningCopyUnsavedLine); len(lines) != 0 {
		t.Fatalf("the daemon's stop was logged as %d failed copies: %v", len(lines), lines)
	}
	// A turn's end and a push after the run has ended start nothing.
	rig.w.copyWorkspaceAtTurnEnd(rig.machine.Claim(), supervise.StateIdle)
	rig.w.copyWorkspaceAtPush(rig.issue.Key)
	if testwait.Holds(time.Second, testwait.Poll, func() bool { return len(rig.bundler.reads()) > 1 }) {
		t.Fatalf("the bundler was asked %v after the run ended, want nothing more", rig.bundler.reads())
	}
}

// The daemon applies every push at its own clock, so a push it applies again — the per-issue
// read's rebuild of a push whose webhook it already applied, under an event id of its own — and a
// webhook that arrives after a newer push are each applied later than the push they repeat or that
// replaced them. Neither moves the record of the issue's newest push back, in its head or its
// time: a copy taken after the newest push holds it, and the pod that replaces this one is sent
// that copy. Were the record's time moved on to the later application, the copy would be refused.
//
// The pushes are driven through the workflow's push handler as the daemon wires it, with the
// daemon's store as its record (openWorkflow), and the record is read back by the store's restore,
// which is the one reader of it.
func TestAPushAppliedAgainOrLateNeverMovesTheNewestRecordedPushBack(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	clock := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	engine := workflow.New(rig.issues, workflow.Config{Project: "LEGION", Clock: func() time.Time { return clock }, Pushes: rig.st}, quietLogger())
	apply := func(eventID, before, after string, at time.Time) {
		t.Helper()
		clock = at
		push := intake.Push{Repo: rig.issue.GitHubRepo, Branch: "legion/" + rig.issue.Key, Before: before, After: after}
		if _, err := intake.ApplyFact(ctx, rig.pool, "github", eventID, push, engine); err != nil {
			t.Fatalf("apply the push %s..%s as %s: %v", before, after, eventID, err)
		}
	}
	// sent stores a running copy of the issue's workspace the cadence took at taken, and answers
	// how the pod that replaces this one is answered it, with the push the restore read.
	sent := func(name string, taken time.Time) (runtime.WorkspaceBundleTake, runtime.WorkspacePush) {
		t.Helper()
		held := runtime.WorkspaceBundle{Bundle: []byte("PACK the " + name), Head: name, Tips: []string{name}, At: taken, PodUID: releasedPodUID}
		if err := rig.st.PutRunningWorkspaceCopy(ctx, rig.issue.Key, held); err != nil {
			t.Fatalf("take the %s: %v", name, err)
		}
		restore, err := rig.st.TakeWorkspaceCopyForRestore(ctx, rig.issue.Key, "pod-after-the-"+name, taken.Add(time.Hour))
		if err != nil {
			t.Fatalf("restore the %s: %v", name, err)
		}
		return restore.Running.Take, restore.Push
	}

	read := clock
	webhook := read.Add(time.Minute)
	apply("pull-request-read:c1..c2", "c1", "c2", read)
	apply("push-webhook:c2..c3", "c2", "c3", webhook)

	// The read rebuilds c2..c3 from the branch's activity after its webhook was applied.
	apply("pull-request-read:c2..c3", "c2", "c3", webhook.Add(2*time.Minute))
	if take, push := sent("copy after the push of c3", webhook.Add(30*time.Second)); take != runtime.WorkspaceBundleSend || push.Head != "c3" || !push.At.Equal(webhook) {
		t.Fatalf("after c2..c3 was applied again, the copy taken after it was answered %v against the push %+v; want it sent against c3 at %s", take, push, webhook)
	}

	// c1..c2's own webhook arrives an hour on, behind c2..c3.
	late := webhook.Add(time.Hour)
	apply("push-webhook:c1..c2", "c1", "c2", late)
	if take, push := sent("copy before the late webhook", late.Add(-time.Minute)); take != runtime.WorkspaceBundleSend || push.Head != "c3" || !push.At.Equal(webhook) {
		t.Fatalf("after the late webhook of c1..c2, the copy taken after c3 was answered %v against the push %+v; want it sent against c3 at %s", take, push, webhook)
	}
}

// Every push of an issue's branch asks for a copy of the issue's workspace, whatever the issue is
// doing, so a push of an issue whose roles were released, or whose branch was deleted after its
// merge, finds no live role to ask. That copy is logged at Debug: the Warn of a copy that found no
// role is what a deployment counts of the cadence and the turn's end, and a Warn per push of every
// released issue would bury it. The same holds for the copy such a push is owed behind a copy in
// flight, as the per-issue read's burst of rebuilt pushes meets: it is the push's copy all the
// same. A turn's end that finds no role is still a Warn, since that copy holds the end of a turn.
func TestAPushOfAnIssueWithNoLiveRoleLogsItsCopyAtDebugAndNeverAtWarn(t *testing.T) {
	rig := newCopyRig(t)
	ctx := context.Background()
	engine := workflow.New(rig.issues, workflow.Config{Project: "LEGION", Pushes: rig.st, PushRecorded: rig.w.copyWorkspaceAtPush}, quietLogger())
	apply := func(eventID, before, after string) {
		t.Helper()
		push := intake.Push{Repo: rig.issue.GitHubRepo, Branch: "legion/" + rig.issue.Key, Before: before, After: after}
		if _, err := intake.ApplyFact(ctx, rig.pool, "github", eventID, push, engine); err != nil {
			t.Fatalf("apply the push %s..%s: %v", before, after, err)
		}
	}
	// noRole waits for count lines of a copy that found no live role, and fails on any at Warn.
	noRole := func(count int) {
		t.Helper()
		var lines []map[string]any
		if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
			lines = rig.lines(t, runningCopyUnsavedLine)
			return len(lines) >= count
		}) {
			t.Fatalf("the daemon logged %d copies that found no role, want %d; it logged:\n%s", len(lines), count, rig.logs.String())
		}
		rig.awaitQuiet(t)
		for _, line := range rig.lines(t, runningCopyUnsavedLine) {
			if line["reason"] != runningCopyNoRole || line["level"] != "DEBUG" {
				t.Fatalf("a push's copy of an issue with no live role was logged as %v, want reason %s at DEBUG", line, runningCopyNoRole)
			}
		}
	}

	rig.setClaimState(t, "suspended")
	apply("push-of-a-released-issue", "c1", "c2")
	noRole(1)
	if asked := rig.bundler.reads(); len(asked) != 0 {
		t.Fatalf("the bundler was asked %v for an issue with no live role", asked)
	}

	// A copy is in flight when the role goes and the push arrives, so the push's copy is owed
	// behind it and runs once it ends.
	rig.setClaimState(t, "idle")
	release := make(chan struct{})
	rig.bundler.holds(func(ctx context.Context) (runtime.WorkspaceBundle, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return runtime.WorkspaceBundle{}, ctx.Err()
		}
		taken := copyRigBundle
		taken.At = time.Now().UTC()
		return taken, nil
	})
	rig.w.copyWorkspaceAtTurnEnd(rig.machine.Claim(), supervise.StateIdle)
	rig.awaitAsks(t, 1)
	rig.setClaimState(t, "suspended")
	apply("push-behind-a-copy-in-flight", "c2", "c3")
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		rig.w.copies.mu.Lock()
		defer rig.w.copies.mu.Unlock()
		return rig.w.copies.issues[rig.issue.Key].owed
	}) {
		t.Fatalf("the push's copy was never owed behind the copy in flight; the daemon logged:\n%s", rig.logs.String())
	}
	close(release)
	noRole(2)

	rig.w.copyWorkspaceAtTurnEnd(rig.machine.Claim(), supervise.StateIdle)
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		for _, line := range rig.lines(t, runningCopyUnsavedLine) {
			if line["reason"] == runningCopyNoRole && line["level"] == "WARN" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("a turn's end that found no live role was not logged at WARN; the daemon logged:\n%s", rig.logs.String())
	}
}
