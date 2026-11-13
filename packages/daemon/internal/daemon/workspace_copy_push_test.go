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
	sent, take, err := takeCopyOf(ctx, rig.st, runtime.WorkspaceCopyRunning, rig.issue.Key, "pod-that-replaces-it", time.Now().UTC().Add(time.Hour))
	if err != nil || take != runtime.WorkspaceBundleSend || string(sent.Bundle) != string(atPush.Bundle) {
		t.Fatalf("the pod that replaces this one was answered %q (%v, %v), want the copy taken after the push", sent.Bundle, take, err)
	}
}

// A release's save takes the same per-issue mark a running copy takes, so it never runs the
// shim's bundler beside a copy in the one workspace: the two share the ref prefix
// `refs/legion-bundle/`, and only timing kept them apart before — a turn's end and the copy it
// was owed can take two copy budgets, more than a short `idle_minutes`. The save waits for the
// copy in flight, then bundles, and its stored bundle drops the copy that finished just before it.
func TestAReleaseSaveWaitsForTheRunningCopyBundlerInFlightBeforeItBundles(t *testing.T) {
	rig := newCopyRig(t)
	rig.runner.copies = &rig.w.copies
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
