// The restore to the pod after a second end, and of a copy written after a pod's first ask: a pod
// ended from outside before any turn of its own, after it restored a running copy or a release
// bundle, leaves that work for the pod Agent Sandbox makes in its place, whatever the API server's
// clock stamps that pod and whatever push the daemon recorded before the restore; and a copy
// written after a pod first asked is sent to no ask of that pod, only to the pod after it. The
// fixtures these tests share with the rest of the restore's tests are in workspace_restore_test.go.
package sandbox

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The pod after the one that restored a copy is sent the kept copy whatever the API server's clock
// says of it. Here that clock agrees with the database's until pod-three, and then runs seconds
// behind it: pod-two restores pod-one's running copy and its launcher confirms the restore, which
// keeps the copy at the database's clock, and pod-three, made by a second end of pod-two before
// any turn of it, is stamped created seconds before that time. The restore reads no
// creationTimestamp — every time it compares is the store's, and pod-three first asks after the
// confirmation — so pod-three is sent the work. A restore that compared the kept copy's time with
// pod-three's creationTimestamp would send pod-three nothing; the rule before this one also spent
// the copy's one attempt on that refusal, and the work went with pod-two.
func TestARestoreSendsTheKeptCopyToAPodTheAPIServerStampedSecondsBeforeIt(t *testing.T) {
	st := bundleStore(t)
	taken := runningWorkspaceCopy("pod-one")
	saveRunningCopy(t, st, taken)
	g := newRig(t, nil, withSavedWorkspace(st))
	// The API server stamps pod-two by a clock that agrees with the database's.
	g.now.Store(new(time.Now().UTC()))
	spec := workerSpec(t)
	g.spawn(spec)
	two := string(g.pod(SandboxName(spec.Claim)).UID)
	if read, _ := bundleBody(t, g.sent(spec.Claim)); !bytes.Equal(read.Bundle, taken.Bundle) || read.Head != taken.Head {
		t.Fatalf("pod-two was sent %d bytes at head %s, want the %d of pod-one's copy at %s", len(read.Bundle), read.Head, len(taken.Bundle), taken.Head)
	}
	if source := runningCopySource(t, st, testTree); source != two {
		t.Fatalf("the running copy names pod %q, want pod-two's %q: its launcher's report keeps it as pod-two's own", source, two)
	}
	kept := storedTime(t, st, runningCopyTaken, testTree)

	three := secondEnd(t, g, spec, kept.Add(-2*time.Second))
	if created := g.pod(SandboxName(spec.Claim)).CreationTimestamp.Time; !created.Before(kept) {
		t.Fatalf("pod-three was stamped created at %s, want it before the kept copy's %s, as an API server seconds behind the database stamps it", created, kept)
	}
	read := sentTo(t, g, spec, three)
	if want := sentBookmarks(taken); !bytes.Equal(read.Bundle, taken.Bundle) || read.Head != taken.Head ||
		!slices.Equal(read.Tips, taken.Tips) || !maps.Equal(read.Bookmarks, want) {
		t.Fatalf("pod-three was sent %d bytes at head %s with tips %v and bookmarks %v, want pod-one's %d at %s with %v and %v",
			len(read.Bundle), read.Head, read.Tips, read.Bookmarks, len(taken.Bundle), taken.Head, taken.Tips, want)
	}
}

// A copy written after a pod first asked for a restore is sent to no ask of that pod, and nothing
// is recorded on it. Here the take of the last copy of the pod before this one began, and its
// bundler read that pod's workspace, before this pod's first role start asked while the issue
// held nothing; the copy was written after that ask. The pod's next role start — its roles
// container restarting — is sent nothing of it: the pod has been working since its first ask, and
// the copy would `jj edit` its workspace back over that work. The ask is weighed against the
// copy's write and not its take's begin. The daemon says so at Info, naming the pod's first ask
// and the copy's write, both by the store's clock, and the copy stays unspent, so the pod after
// this one is sent the work.
func TestARestoreLeavesACopyWrittenAfterThePodFirstAskedForThePodAfterIt(t *testing.T) {
	st := bundleStore(t)
	logs := &logBuffer{}
	g := newRig(t, nil, withLog(logs), withSavedWorkspace(st))
	// The take of the pod before this one begins, and its bundler reads that pod's workspace.
	lateBegun, err := st.BeginWorkspaceCopy(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t)
	g.spawn(spec)
	pod := string(g.pod(SandboxName(spec.Claim)).UID)
	if frames := g.sent(spec.Claim); countOf(frames, shimwire.TypeLauncherBundle) != 0 {
		t.Fatalf("the pod's first role start was sent %v, want no slice: the issue held nothing", frameTypes(frames))
	}
	late := runningWorkspaceCopy("pod-before-this-one")
	if err := st.PutRunningWorkspaceCopy(g.ctx, testProject, testTree, lateBegun, late); err != nil {
		t.Fatalf("write the late copy of the pod before this one: %v", err)
	}

	before := len(g.sent(spec.Claim))
	g.connect(spec.Claim)
	g.r.launchers.restoreWorkspace(g.ctx, spec.Claim, pod)
	for _, frame := range g.sent(spec.Claim)[before:] {
		if slice, ok := frame.(shimwire.LauncherBundle); ok {
			t.Fatalf("the pod that first asked before the copy was written was sent slice %d of %d of copy %d, want nothing",
				slice.Index, slice.Count, slice.Release)
		}
	}
	asked := storedTime(t, st, firstAsked, testTree, pod)
	if taken := storedTime(t, st, runningCopyTaken, testTree); !taken.Before(asked) {
		t.Fatalf("the late copy was taken at %s and the pod first asked at %s, want the take before the ask", taken, asked)
	}
	written := storedTime(t, st, runningCopyWritten, testTree)
	line := restoreLine(t, logs, leftForNextPodLine)
	for _, want := range []string{"level=INFO", "issue=" + testTree, "pod=" + pod, `copy="running copy"`,
		"firstAsked=" + loggedAs(asked), "written=" + loggedAs(written)} {
		if !strings.Contains(line, want) {
			t.Fatalf("the copy left for the next pod logged %q, want it to carry %s", line, want)
		}
	}
	var unspent bool
	if err := st.Pool().QueryRow(g.ctx, `select attempt_pod_uid is null from workspace_running_copies where issue = $1`, testTree).Scan(&unspent); err != nil || !unspent {
		t.Fatalf("the late copy's attempt is spent = %t (%v), want nothing recorded on it", !unspent, err)
	}

	next := secondEnd(t, g, spec, time.Now().UTC())
	read := sentTo(t, g, spec, next)
	if want := sentBookmarks(late); !bytes.Equal(read.Bundle, late.Bundle) || read.Head != late.Head ||
		!slices.Equal(read.Tips, late.Tips) || !maps.Equal(read.Bookmarks, want) {
		t.Fatalf("the pod after this one was sent %d bytes at head %s with tips %v and bookmarks %v, want the late copy's %d at %s with %v and %v",
			len(read.Bundle), read.Head, read.Tips, read.Bookmarks, len(late.Bundle), late.Head, late.Tips, want)
	}
}

// A release bundle older than the newest push of its branch the daemon recorded is restored like
// any other: it is the released pod's last copy, taken while the roles were idle and before the
// release stopped them, so a push recorded after it — GitHub's "Update branch", a person's push,
// a webhook the daemon applied late — holds none of its work, and the bundle is the one place that
// work is. It crosses whole but for the issue's own branch and its rescue branch, which no
// restore sets (runtime.RestorableBookmarks), and the launcher's report moves it out of the bundle
// table, so it holds the issue's pod no longer.
//
// The report keeps it as the restoring pod's running copy, and the push recorded before the
// confirmation refuses that copy no more than it refused the bundle: the kept copy is stamped at
// the confirmation, when the restoring pod's copy of the work begins
// (store.KeepRestoredWorkspaceBundle). A second end of that pod before any turn of it brings a pod
// that is sent the same work, where a copy stamped at the bundle's own time would be refused as
// older than the push and the work lost with the pod that restored it.
func TestARestoreSendsAReleaseBundleOlderThanTheIssuesNewestPushAndThePodAfterASecondEndIsSentItToo(t *testing.T) {
	st := bundleStore(t)
	ctx := context.Background()
	saved := savedWorkspaceBundle()
	saveReleaseBundle(t, st, saved)
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		_, err := st.RecordIssuePush(ctx, tx, testTree, "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1", "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2")
		return err
	}); err != nil {
		t.Fatalf("record the push of the issue's branch: %v", err)
	}
	g := newRig(t, nil, withSavedWorkspace(st))
	spec := workerSpec(t)
	g.spawn(spec)

	read, _ := bundleBody(t, g.sent(spec.Claim))
	if want := sentBookmarks(saved); !bytes.Equal(read.Bundle, saved.Bundle) || read.Head != saved.Head || !maps.Equal(read.Bookmarks, want) {
		t.Fatalf("the pod was sent %d bytes at head %s with bookmarks %v, want the %d of the release bundle at %s with %v",
			len(read.Bundle), read.Head, read.Bookmarks, len(saved.Bundle), saved.Head, want)
	}
	if _, held, err := st.WorkspaceBundleSize(g.ctx, testTree); err != nil || held {
		t.Fatalf("after the launcher reported its restore the store holds the bundle = %t (%v), want it gone", held, err)
	}

	third := secondEnd(t, g, spec, time.Now().UTC())
	again := sentTo(t, g, spec, third)
	if want := sentBookmarks(saved); !bytes.Equal(again.Bundle, saved.Bundle) || again.Head != saved.Head ||
		!slices.Equal(again.Tips, saved.Tips) || !maps.Equal(again.Bookmarks, want) {
		t.Fatalf("the pod after the second end was sent %d bytes at head %s with tips %v and bookmarks %v, want the %d restored at %s with %v and %v",
			len(again.Bundle), again.Head, again.Tips, again.Bookmarks, len(saved.Bundle), saved.Head, saved.Tips, want)
	}
}

// secondEnd ends the pod of spec's claim from outside, before any turn of it — an eviction, a
// node drain — and answers the uid of the pod Agent Sandbox makes in its place once that pod's
// launcher has connected. The rig's clock is moved to created first, which is the instant the API
// server stamps the new pod created; the restore reads none of it, every time it compares being
// the store's, so a test names it only where the API server's clock is the point.
func secondEnd(t *testing.T, g *rig, spec runtime.SpawnSpec, created time.Time) string {
	t.Helper()
	ended := g.pod(SandboxName(spec.Claim))
	g.now.Store(new(created))
	if err := g.kube.Tracker().Delete(podsGVR, testNamespace, SandboxName(spec.Claim)); err != nil {
		t.Fatalf("end the pod: %v", err)
	}
	var uid string
	g.eventually("the pod after the second end and its launcher", func() bool {
		pod := g.pod(SandboxName(spec.Claim))
		if pod == nil || pod.UID == ended.UID {
			return false
		}
		g.r.launchers.mu.Lock()
		defer g.r.launchers.mu.Unlock()
		session := g.r.launchers.sessions[spec.Claim]
		uid = string(pod.UID)
		return session != nil && session.podUID == uid
	})
	return uid
}

// sentTo is the saved copy the restore of pod's first role start sends that pod's launcher, parsed
// back out of the slices; it fails the test when the restore sends nothing.
func sentTo(t *testing.T, g *rig, spec runtime.SpawnSpec, pod string) shimwire.WorkspaceBundle {
	t.Helper()
	before := len(g.sent(spec.Claim))
	g.r.launchers.restoreWorkspace(g.ctx, spec.Claim, pod)
	read, _ := bundleBody(t, g.sent(spec.Claim)[before:])
	return read
}

// A pod that restored a running copy and was ended from outside before any turn of its own took
// a copy still leaves the work for the pod after it. The cluster ended the pod the copy came off;
// the next pod was sent the copy and its launcher's report confirmed the restore; that pod's
// agents came up Ready, so neither a turn's end nor the cadence copies it, and nothing pushed. Its
// disk is then the one place the work is, and the confirmation kept the copy as that pod's own
// (store.KeepRestoredRunningWorkspaceCopy): a second end brings a third pod, which is sent the same
// work.
func TestARestoredRunningCopyIsSentToThePodAfterASecondEndBeforeAnyTurn(t *testing.T) {
	st := bundleStore(t)
	taken := runningWorkspaceCopy("pod-one")
	saveRunningCopy(t, st, taken)
	g := newRig(t, nil, withSavedWorkspace(st))
	spec := workerSpec(t)
	g.spawn(spec)
	two := string(g.pod(SandboxName(spec.Claim)).UID)
	if read, _ := bundleBody(t, g.sent(spec.Claim)); !bytes.Equal(read.Bundle, taken.Bundle) || read.Head != taken.Head {
		t.Fatalf("pod-two was sent %d bytes at head %s, want the %d of pod-one's copy at %s", len(read.Bundle), read.Head, len(taken.Bundle), taken.Head)
	}
	// pod-two took no copy of its own, since no turn of it ran, and its roles container restarting
	// is answered nothing: its own workspace holds the work.
	restartIsSentNothing(t, g, spec, two)

	three := secondEnd(t, g, spec, time.Now().UTC())
	read := sentTo(t, g, spec, three)
	if want := sentBookmarks(taken); !bytes.Equal(read.Bundle, taken.Bundle) || read.Head != taken.Head ||
		!slices.Equal(read.Tips, taken.Tips) || !maps.Equal(read.Bookmarks, want) {
		t.Fatalf("pod-three was sent %d bytes at head %s with tips %v and bookmarks %v, want pod-one's %d at %s with %v and %v",
			len(read.Bundle), read.Head, read.Tips, read.Bookmarks, len(taken.Bundle), taken.Head, taken.Tips, want)
	}
}

// The same holds of a release bundle. A release saved pod-one's work and took that pod away;
// pod-two was sent the bundle and its launcher's report confirmed the restore, which lifts the
// hold the bundle's row puts on the issue's pod; pod-two's agents came up Ready and took no copy.
// The confirmation moved the work into the running copy, named off pod-two
// (store.KeepRestoredWorkspaceBundle), so a second end of pod-two before any turn of it brings a
// third pod that is sent the release's work.
func TestARestoredReleaseBundleIsSentToThePodAfterASecondEndBeforeAnyTurn(t *testing.T) {
	st := bundleStore(t)
	saved := savedWorkspaceBundle()
	saved.PodUID = "pod-one"
	saveReleaseBundle(t, st, saved)
	g := newRig(t, nil, withSavedWorkspace(st))
	spec := workerSpec(t)
	g.spawn(spec)
	two := string(g.pod(SandboxName(spec.Claim)).UID)
	if read, _ := bundleBody(t, g.sent(spec.Claim)); !bytes.Equal(read.Bundle, saved.Bundle) || read.Head != saved.Head {
		t.Fatalf("pod-two was sent %d bytes at head %s, want the %d of the release bundle at %s", len(read.Bundle), read.Head, len(saved.Bundle), saved.Head)
	}
	// The hold lifts: the bundle's row is gone.
	if _, held, err := st.WorkspaceBundleSize(g.ctx, testTree); err != nil || held {
		t.Fatalf("after pod-two's restore the store holds the release bundle = %t (%v), want it gone, which lifts the hold on the issue's pod", held, err)
	}
	restartIsSentNothing(t, g, spec, two)

	three := secondEnd(t, g, spec, time.Now().UTC())
	read := sentTo(t, g, spec, three)
	if want := sentBookmarks(saved); !bytes.Equal(read.Bundle, saved.Bundle) || read.Head != saved.Head ||
		!slices.Equal(read.Tips, saved.Tips) || !maps.Equal(read.Bookmarks, want) {
		t.Fatalf("pod-three was sent %d bytes at head %s with tips %v and bookmarks %v, want the release's %d at %s with %v and %v",
			len(read.Bundle), read.Head, read.Tips, read.Bookmarks, len(saved.Bundle), saved.Head, saved.Tips, want)
	}
}

// restartIsSentNothing restarts the roles container of pod, a launcher process of its own on the
// pod's own disk, and fails the test if the restore of its next role start sends it any copy:
// that pod restored the work, and its workspace holds it.
func restartIsSentNothing(t *testing.T, g *rig, spec runtime.SpawnSpec, pod string) {
	t.Helper()
	before := len(g.sent(spec.Claim))
	g.connect(spec.Claim)
	g.r.launchers.restoreWorkspace(g.ctx, spec.Claim, pod)
	for _, frame := range g.sent(spec.Claim)[before:] {
		if slice, ok := frame.(shimwire.LauncherBundle); ok {
			t.Fatalf("the pod that restored the work was sent slice %d of %d of copy %d on a restart, want nothing: its own disk holds the work",
				slice.Index, slice.Count, slice.Release)
		}
	}
}
