package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// testRelease is the id of the idle release whose save the fixture's bundle carries: the daemon
// deletes the bundle of the release a launcher's restore report names, so a stored bundle has one.
const testRelease int64 = 11

// savedWorkspaceBundle is a release's saved work as the store holds it: more than one
// shimwire.LauncherBundlePayloadBytes of git bundle, so the hop to the launcher is a sequence of
// slices and not one frame, with the head, the tips and the bookmark a restore needs beside it.
// It was taken before the rig's clock starts, so every pod the rig makes is one created after
// the save — the only kind a bundle is sent to
// (store.TakeWorkspaceBundleForRestore). Its source pod, the pod the work came off, is a pod of
// an earlier life of the issue that the rig never makes, so no pod of the rig is refused by uid;
// a test of the pod the work came off names that pod itself.
func savedWorkspaceBundle() runtime.WorkspaceBundle {
	return runtime.WorkspaceBundle{
		Bundle:    bytes.Repeat([]byte("PACKjj\x00\x02"), (600<<10)/8),
		Head:      "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips:      []string{"da4f9f492e9f8349389c243743e0a000cd1c676f"},
		Bookmarks: map[string]string{"legion/" + testTree: "dffe88ae2f793e13436cef3921adc988433fbd4e"},
		At:        rigNow.Add(-time.Minute),
		Release:   testRelease,
		PodUID:    "pod-the-release-took-the-work-off",
	}
}

// bundleStore is a migrated store whose issues table records testTree under the rig's project,
// as the release that saved a bundle of it leaves it: the store answers a take only for an issue
// the asking project records (store.Store.TakeWorkspaceBundleForRestore).
func bundleStore(t *testing.T) *store.Store {
	t.Helper()
	st := migratedStore(t)
	if _, err := st.Pool().Exec(context.Background(), `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ($1, $1, 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1)`, testTree); err != nil {
		t.Fatalf("record %s: %v", testTree, err)
	}
	return st
}

// takeBundle is st's take of a release's saved work for a pod of the rig's project, as the daemon
// wires it into the runtime (internal/daemon's savedWorkspaceBundle).
func takeBundle(st *store.Store) runtime.SavedWorkspaceBundle {
	return func(ctx context.Context, issue, podUID string, podCreated time.Time) (runtime.WorkspaceBundle, runtime.WorkspaceBundleTake, error) {
		return st.TakeWorkspaceBundleForRestore(ctx, testProject, issue, podUID, podCreated)
	}
}

// dropHeldBundle is st's drop of a release's saved work that a kept volume of a pod of the rig's
// project holds, as the daemon wires it into the runtime (internal/daemon's heldWorkspaceBundle).
func dropHeldBundle(st *store.Store) runtime.HeldWorkspaceBundleDrop {
	return func(ctx context.Context, issue, podUID string, keptSince time.Time) (runtime.WorkspaceBundle, bool, error) {
		return st.DropHeldWorkspaceBundle(ctx, testProject, issue, podUID, keptSince)
	}
}

// frameTypes names what a launcher was sent, for a failure an operator reads.
func frameTypes(frames []shimwire.Frame) []string {
	types := make([]string, 0, len(frames))
	for _, frame := range frames {
		types = append(types, frame.FrameType())
	}
	return types
}

// A released issue's pod comes back with the work the release took off its disk: the daemon reads
// the bundle its store kept, sends it to the new pod's launcher in launcher_bundle slices, and
// drops its own copy when that launcher reports the restore it ran — all of the hop before the
// launcher is told to start any role, since the launcher restores the bundle into the workspace
// ahead of the role's command (internal/launcher).
func TestASavedWorkspaceBundleReachesThePodsLauncherBeforeItStartsAnyRole(t *testing.T) {
	st := bundleStore(t)
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.SavedWorkspaceBundle, o.WorkspaceBundleRestored = takeBundle(st), st.DeleteRestoredWorkspaceBundle
	}))
	spec := workerSpec(t)
	g.spawn(spec)

	frames := g.sent(spec.Claim)
	start := slices.IndexFunc(frames, func(f shimwire.Frame) bool { _, ok := f.(shimwire.LauncherStart); return ok })
	if start < 1 {
		t.Fatalf("the launcher was sent %v, want the bundle's slices and then the role's start", frameTypes(frames))
	}
	for _, frame := range frames[start+1:] {
		if _, ok := frame.(shimwire.LauncherBundle); ok {
			t.Fatalf("the launcher was sent %v: a slice crossed after the role's start", frameTypes(frames))
		}
	}
	var body []byte
	var sequence string
	for i, frame := range frames[:start] {
		slice, ok := frame.(shimwire.LauncherBundle)
		if !ok {
			t.Fatalf("the launcher was sent %v, want the bundle's slices before the role's start", frameTypes(frames))
		}
		if i == 0 {
			sequence = slice.ID
		}
		if slice.ID != sequence || slice.Index != i || slice.Count != start {
			t.Fatalf("slice %d of %d under %q arrived at position %d of the %d sent before the start",
				slice.Index, slice.Count, slice.ID, i, start)
		}
		body = append(body, slice.Payload...)
	}
	read, err := shimwire.ParseWorkspaceBundle(body)
	if err != nil {
		t.Fatalf("the %d slices rebuilt no workspace bundle: %v", start, err)
	}
	if !bytes.Equal(read.Bundle, saved.Bundle) {
		t.Fatalf("the bundle crossed as %d bytes, want the %d saved", len(read.Bundle), len(saved.Bundle))
	}
	if read.Head != saved.Head || !slices.Equal(read.Tips, saved.Tips) || !maps.Equal(read.Bookmarks, saved.Bookmarks) {
		t.Fatalf("the bundle crossed as head %s, tips %v and bookmarks %v, want %s, %v and %v",
			read.Head, read.Tips, read.Bookmarks, saved.Head, saved.Tips, saved.Bookmarks)
	}
	if !read.At.Equal(saved.At) {
		t.Fatalf("the bundle crossed as taken at %s, want %s", read.At, saved.At)
	}
	// The launcher restored it and said so, so the daemon's copy has done its work.
	if _, held, err := st.WorkspaceBundleSize(g.ctx, testTree); err != nil || held {
		t.Fatalf("after the launcher reported its restore the store holds the bundle = %t (%v), want it deleted", held, err)
	}
}

// Held says the launcher holds the body, not that the restore ran: the restore runs later, at the
// first role start, and it can fail — a prerequisite commit the bundle needs that the new pod's
// clone does not have, a jj step that will not run, the restore's own budget. The work is kept
// where the launcher's notice says it is, in the daemon's database, and it is sent to no pod
// again: that send was the bundle's one restore attempt.
//
// The pod it was sent to is the case the rule is for. Its roles container restarts, which is a
// launcher process of its own, holding no record of the restore that already ran, on the pod's
// own disk, whose workspace the roles have worked in since. The bundle sent there a second time
// would `jj edit` that workspace back to the saved head and move each bookmark
// `--allow-backwards` over all of it.
func TestAFailedRestoreKeepsTheSavedWorkAndSendsItToNoPodAgain(t *testing.T) {
	st := bundleStore(t)
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	const refusal = "git bundle unbundle: object not found"
	g := newRig(t, nil, withFailedRestore(refusal), withOptions(func(o *Options) {
		o.SavedWorkspaceBundle, o.WorkspaceBundleRestored = takeBundle(st), st.DeleteRestoredWorkspaceBundle
	}))
	spec := workerSpec(t)
	loc := g.spawn(spec)
	if loc.Sandbox.Generation != spec.Generation {
		t.Fatalf("the failed restore left the launch at %+v, want generation %d", loc, spec.Generation)
	}

	sent := g.sent(spec.Claim)
	if start := slices.IndexFunc(sent, func(f shimwire.Frame) bool { _, ok := f.(shimwire.LauncherStart); return ok }); start < 1 {
		t.Fatalf("the launcher was sent %v, want the bundle's slices and then the role's start", frameTypes(sent))
	}
	size, held, err := st.WorkspaceBundleSize(g.ctx, testTree)
	if err != nil {
		t.Fatal(err)
	}
	if !held || size != len(saved.Bundle) {
		t.Fatalf("after a restore the launcher could not run the store holds %d bytes (held=%t), want the %d saved kept",
			size, held, len(saved.Bundle))
	}

	// The roles container restarts in the same pod, and the start of the next role is what
	// would send the bundle again.
	before := len(sent)
	g.connect(spec.Claim)
	g.r.launchers.restoreWorkspace(g.ctx, spec.Claim, string(g.pod(SandboxName(spec.Claim)).UID))
	for _, frame := range g.sent(spec.Claim)[before:] {
		if slice, ok := frame.(shimwire.LauncherBundle); ok {
			t.Fatalf("the pod's second launcher was sent slice %d of %d of release %d, want nothing: the bundle has had its one restore attempt",
				slice.Index, slice.Count, slice.Release)
		}
	}
	if size, held, err := st.WorkspaceBundleSize(g.ctx, testTree); err != nil || !held || size != len(saved.Bundle) {
		t.Fatalf("after the restart the store holds %d bytes (held=%t, %v), want the %d saved kept", size, held, err, len(saved.Bundle))
	}
}

// A release stops the roles of the pod it is releasing, and a role relaunched in that pod before
// the teardown deletes it starts like any other: it asks the store for the issue's saved work.
// That pod is the one the work was taken off — its workspace holds every commit of it — so it
// must be sent nothing, and it must not spend the bundle's one restore attempt either: the pod
// the wake makes next is the one with an empty workspace, and an attempt spent here would leave
// it sent nothing while the row stayed and held the issue's pod
// (workflow.IdleHoldBundleUnrestored). The store knows that pod by the uid the save recorded,
// so it is the pod's identity and not its clock that leaves the attempt where it belongs.
func TestARelaunchInThePodBeingReleasedLeavesTheAttemptForItsReplacement(t *testing.T) {
	st := bundleStore(t)
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.SavedWorkspaceBundle, o.WorkspaceBundleRestored = takeBundle(st), st.DeleteRestoredWorkspaceBundle
	}))
	spec := workerSpec(t)
	g.spawn(spec)
	released := g.pod(SandboxName(spec.Claim))

	// The release takes the workspace's unpushed work off that pod and saves it, while the pod
	// and its roles are still there: the save names that pod, as the runtime's own does
	// (Runtime.BundleWorkspace).
	saved := savedWorkspaceBundle()
	saved.At = rigNow.Add(time.Minute)
	saved.PodUID = string(released.UID)
	if err := st.PutWorkspaceBundle(g.ctx, testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	g.advance(2 * time.Minute)

	// A role of the pod being released is relaunched — a crash, a stale-plugin pair, the
	// release's own stop answered by the supervisor — before the teardown deletes the pod.
	before := len(g.sent(spec.Claim))
	g.connect(spec.Claim)
	g.r.launchers.restoreWorkspace(g.ctx, spec.Claim, string(released.UID))
	for _, frame := range g.sent(spec.Claim)[before:] {
		if slice, ok := frame.(shimwire.LauncherBundle); ok {
			t.Fatalf("the pod being released was sent slice %d of %d of release %d, want nothing: its own workspace is where the work came from",
				slice.Index, slice.Count, slice.Release)
		}
	}

	// The teardown takes the pod, the wake brings its replacement, and that pod — created after
	// the save — is answered the whole of the work.
	if err := g.kube.Tracker().Delete(podsGVR, testNamespace, SandboxName(spec.Claim)); err != nil {
		t.Fatalf("delete the released pod: %v", err)
	}
	g.advance(time.Hour)
	g.eventually("the replacement pod's launcher", func() bool {
		pod := g.pod(SandboxName(spec.Claim))
		if pod == nil || pod.UID == released.UID {
			return false
		}
		g.r.launchers.mu.Lock()
		defer g.r.launchers.mu.Unlock()
		session := g.r.launchers.sessions[spec.Claim]
		return session != nil && session.podUID == string(pod.UID)
	})
	replacement := g.pod(SandboxName(spec.Claim))
	if !replacement.CreationTimestamp.Time.After(saved.At) {
		t.Fatalf("the replacement pod was created %s, want it after the save at %s", replacement.CreationTimestamp, saved.At)
	}
	before = len(g.sent(spec.Claim))
	g.r.launchers.restoreWorkspace(g.ctx, spec.Claim, string(replacement.UID))
	var body []byte
	for _, frame := range g.sent(spec.Claim)[before:] {
		slice, ok := frame.(shimwire.LauncherBundle)
		if !ok {
			t.Fatalf("the replacement pod's launcher was sent %v, want the bundle's slices", frameTypes(g.sent(spec.Claim)[before:]))
		}
		body = append(body, slice.Payload...)
	}
	read, err := shimwire.ParseWorkspaceBundle(body)
	if err != nil {
		t.Fatalf("the slices the replacement pod was sent rebuilt no workspace bundle: %v", err)
	}
	if !bytes.Equal(read.Bundle, saved.Bundle) || read.Head != saved.Head ||
		!slices.Equal(read.Tips, saved.Tips) || !maps.Equal(read.Bookmarks, saved.Bookmarks) {
		t.Fatalf("the replacement pod was sent %d bytes at head %s with tips %v and bookmarks %v, want the %d saved at %s, %v and %v",
			len(read.Bundle), read.Head, read.Tips, read.Bookmarks, len(saved.Bundle), saved.Head, saved.Tips, saved.Bookmarks)
	}

	// That send was the attempt. The replacement pod's own roles container restarting is the
	// case the rule is for, and it is sent nothing.
	before = len(g.sent(spec.Claim))
	g.connect(spec.Claim)
	g.r.launchers.restoreWorkspace(g.ctx, spec.Claim, string(replacement.UID))
	for _, frame := range g.sent(spec.Claim)[before:] {
		if slice, ok := frame.(shimwire.LauncherBundle); ok {
			t.Fatalf("the replacement pod's second launcher was sent slice %d of %d of release %d, want nothing: the bundle has had its one restore attempt",
				slice.Index, slice.Count, slice.Release)
		}
	}
	if size, held, err := st.WorkspaceBundleSize(g.ctx, testTree); err != nil || !held || size != len(saved.Bundle) {
		t.Fatalf("the store holds %d bytes (held=%t, %v), want the %d saved kept until a launcher reports the restore", size, held, err, len(saved.Bundle))
	}
}

// A launcher keeps the report of its restore until some connection carries it, so one can reach
// the daemon after the issue has been released again and that release has saved a bundle of its
// own. The report names the release it restored, and the delete reads it: the newer bundle stays,
// which is the whole of the work, since the teardown of that newer release took the disk it came
// off.
func TestALateRestoreReportLeavesTheBundleOfANewerRelease(t *testing.T) {
	st := bundleStore(t)
	ctx := context.Background()
	first := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(ctx, testTree, first.Release, first); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	// The issue woke, worked and was released again while the first pod's launcher still held
	// its report.
	second := savedWorkspaceBundle()
	second.Bundle, second.Head, second.Release = []byte("PACK the work of the second release"), "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9", first.Release+1
	if err := st.PutWorkspaceBundle(ctx, testTree, second.Release, second); err != nil {
		t.Fatalf("save the second release's bundle: %v", err)
	}

	d := newLaunchers(nil, takeBundle(st), st.DeleteRestoredWorkspaceBundle, dropHeldBundle(st), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	spec := workerSpec(t)
	session := d.accept(spec.Claim, testTree, rigNow, time.Time{}, shimwire.LauncherHello{LauncherID: "late", PodUID: "pod-uid"})
	frames, err := bundleFrames(testTree, first)
	if err != nil {
		t.Fatal(err)
	}
	d.restoreReported(session, shimwire.LauncherRestored{ID: frames[0].ID, Release: first.Release, OK: true})

	kept, take, err := st.TakeWorkspaceBundleForRestore(ctx, testProject, testTree, "pod-uid", rigNow)
	if err != nil {
		t.Fatal(err)
	}
	if take != runtime.WorkspaceBundleSend || !bytes.Equal(kept.Bundle, second.Bundle) || kept.Release != second.Release {
		t.Fatalf("after the first release's report the store holds %q of release %d (take=%v), want the %q release %d saved since",
			kept.Bundle, kept.Release, take, second.Bundle, second.Release)
	}
	// The pod of the second release restores it, and that report drops it.
	later, err := bundleFrames(testTree, second)
	if err != nil {
		t.Fatal(err)
	}
	d.restoreReported(session, shimwire.LauncherRestored{ID: later[0].ID, Release: second.Release, OK: true})
	if _, held, err := st.WorkspaceBundleSize(ctx, testTree); err != nil || held {
		t.Fatalf("after the second release's report the store holds the bundle = %t (%v), want it deleted", held, err)
	}
}

// A launcher that refuses a slice costs the restore and nothing else: the role starts on the
// workspace the pod's own fetch provisioned, the launcher writes the agent the notice of what was
// lost, and the saved work stays in the daemon's database. A pod that never starts would be the
// worse mistake. The refusal costs the bundle its one attempt all the same: the daemon records
// the attempt before it sends a slice, because a send it could not finish leaves it knowing
// nothing of what that pod's workspace now holds.
func TestARefusedBundleSliceStillStartsTheRoleAndKeepsTheSavedWork(t *testing.T) {
	st := bundleStore(t)
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	const refusal = "launcher_bundle sequence rebuilt to another digest than its slices declared"
	g := newRig(t, nil, withRefusedBundle(refusal), withOptions(func(o *Options) {
		o.SavedWorkspaceBundle, o.WorkspaceBundleRestored = takeBundle(st), st.DeleteRestoredWorkspaceBundle
	}))
	spec := workerSpec(t)
	loc := g.spawn(spec)
	if loc.Sandbox.Generation != spec.Generation {
		t.Fatalf("the refused bundle left the launch at %+v, want generation %d", loc, spec.Generation)
	}

	frames := g.sent(spec.Claim)
	bundleSlices, starts := 0, 0
	for _, frame := range frames {
		switch frame.(type) {
		case shimwire.LauncherBundle:
			bundleSlices++
		case shimwire.LauncherStart:
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("the launcher was sent %v, want the role's start all the same", frameTypes(frames))
	}
	// A refusal ends the sequence: the rest of a bundle the launcher will not take is bytes no
	// pod reads.
	if bundleSlices != 1 {
		t.Fatalf("the launcher was sent %v, want one slice and no more after its refusal", frameTypes(frames))
	}
	size, held, err := st.WorkspaceBundleSize(g.ctx, testTree)
	if err != nil {
		t.Fatal(err)
	}
	if !held || size != len(saved.Bundle) {
		t.Fatalf("after the refusal the store holds %d bytes (held=%t), want the %d saved kept", size, held, len(saved.Bundle))
	}
}

// The replacement pod refused because its node's clock ran ahead of the API server's is sent
// nothing, and so is every pod after it.
//
// A save is stamped by the clock of the node the released pod ran on, and a pod's
// creationTimestamp by the API server's; a node minutes ahead stamps the save in the
// replacement pod's own future, so that pod reads as older than the work it was made to carry
// and is refused. Refusing is the direction to fail in — the work stays in the database and the
// pod starts on what its fetch provisioned — but the refusal has to close the bundle. That pod
// stands for hours with its roles working and pushing to the issue's branch, and the pod made
// after it, by an eviction or an update round or a park and unpark, has a workspace provisioned
// from everything they pushed: sending it the bundle would run `jj edit` and
// `jj bookmark set --allow-backwards` back over all of it.
func TestAReplacementPodRefusedByClockSkewLeavesNothingForThePodAfterIt(t *testing.T) {
	st := bundleStore(t)
	saved := savedWorkspaceBundle()
	// The node that took the bundle is a minute ahead of the API server that stamps the pods.
	saved.At = rigNow.Add(time.Minute)
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.SavedWorkspaceBundle, o.WorkspaceBundleRestored = takeBundle(st), st.DeleteRestoredWorkspaceBundle
	}))
	spec := workerSpec(t)
	g.spawn(spec)
	refused := g.pod(SandboxName(spec.Claim))
	if refused.CreationTimestamp.Time.After(saved.At) {
		t.Fatalf("the replacement pod was created %s, want the skewed save at %s to be later", refused.CreationTimestamp, saved.At)
	}
	for _, frame := range g.sent(spec.Claim) {
		if slice, ok := frame.(shimwire.LauncherBundle); ok {
			t.Fatalf("the refused pod was sent slice %d of %d of release %d, want nothing: it reads as older than the save",
				slice.Index, slice.Count, slice.Release)
		}
	}
	// Its roles ran for an hour and pushed what they did; then the pod went and another came.
	if err := g.kube.Tracker().Delete(podsGVR, testNamespace, SandboxName(spec.Claim)); err != nil {
		t.Fatalf("delete the refused pod: %v", err)
	}
	g.advance(time.Hour)
	g.eventually("the pod after the refused one", func() bool {
		pod := g.pod(SandboxName(spec.Claim))
		if pod == nil || pod.UID == refused.UID {
			return false
		}
		g.r.launchers.mu.Lock()
		defer g.r.launchers.mu.Unlock()
		session := g.r.launchers.sessions[spec.Claim]
		return session != nil && session.podUID == string(pod.UID)
	})
	after := g.pod(SandboxName(spec.Claim))
	if !after.CreationTimestamp.Time.After(saved.At) {
		t.Fatalf("the pod after the refused one was created %s, want it after the save at %s: the save's own clock no longer refuses it",
			after.CreationTimestamp, saved.At)
	}
	before := len(g.sent(spec.Claim))
	g.r.launchers.restoreWorkspace(g.ctx, spec.Claim, string(after.UID))
	for _, frame := range g.sent(spec.Claim)[before:] {
		if slice, ok := frame.(shimwire.LauncherBundle); ok {
			t.Fatalf("the pod after the refused one was sent slice %d of %d of release %d, want nothing: the refusal was the bundle's one attempt",
				slice.Index, slice.Count, slice.Release)
		}
	}
	// The work is kept whole, for a person to carry out of the database, and it holds the
	// issue's pod until the issue's work is over (workflow.IdleHoldBundleUnrestored).
	if size, held, err := st.WorkspaceBundleSize(g.ctx, testTree); err != nil || !held || size != len(saved.Bundle) {
		t.Fatalf("after the refusal the store holds %d bytes (held=%t, %v), want the %d saved kept", size, held, err, len(saved.Bundle))
	}
}

// A pod whose own kept volume holds the work a release saved is sent none of it (R5). The release
// keeps the Sandbox that owns the issue's kept volume, Suspended, and the pod that the issue's
// wake brings next mounts that volume, with the workspace the save was taken from. The pod's
// launcher would refuse a bundle sent there, because the workspace is older than the save
// (internal/launcher's restoreWorkspace). It would log a notice that the work was lost, and the
// bundle would hold the issue's pod (workflow.IdleHoldBundleUnrestored). So the daemon sends that
// pod no slice and drops the bundle of that release, and the store then holds no bundle for the
// issue.
//
// A pod on a volume newer than the save restores the work, as at main: the pod of a kept Sandbox
// made after the save, which is the Sandbox that a reset makes, and the pod of a Sandbox with an
// ephemeral volume, which its own pod made. That ephemeral Sandbox was made before the save, as
// an eviction leaves it. So a rule that reads the Sandbox's age and not its volume fails the
// test, and so does a rule that never sends a bundle. A rule that sends every bundle fails the
// first case.
func TestAKeptVolumeTakesNoSavedWorkItHolds(t *testing.T) {
	// drop is one bundle the store dropped, as the drop answered it, beside the creation of the
	// kept Sandbox that the runtime asked it for.
	type drop struct {
		release   int64
		keptSince time.Time
	}
	// wired is the store's three bundle calls as the daemon hands them to the runtime (daemon's
	// sandboxRuntime), and the drops the store made, for the test to read.
	wired := func(st *store.Store) (rigOption, func() []drop) {
		var mu sync.Mutex
		var drops []drop
		held := dropHeldBundle(st)
		option := withOptions(func(o *Options) {
			o.SavedWorkspaceBundle, o.WorkspaceBundleRestored = takeBundle(st), st.DeleteRestoredWorkspaceBundle
			o.HeldWorkspaceBundle = func(ctx context.Context, issue, podUID string, keptSince time.Time) (runtime.WorkspaceBundle, bool, error) {
				bundle, dropped, err := held(ctx, issue, podUID, keptSince)
				if dropped {
					mu.Lock()
					drops = append(drops, drop{release: bundle.Release, keptSince: keptSince})
					mu.Unlock()
				}
				return bundle, dropped, err
			}
		})
		return option, func() []drop {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(drops)
		}
	}
	// sentBundle is the bundle that the slices among frames rebuild, and false where frames hold
	// no slice.
	sentBundle := func(t *testing.T, frames []shimwire.Frame) (shimwire.WorkspaceBundle, bool) {
		t.Helper()
		var body []byte
		for _, frame := range frames {
			if slice, ok := frame.(shimwire.LauncherBundle); ok {
				body = append(body, slice.Payload...)
			}
		}
		if body == nil {
			return shimwire.WorkspaceBundle{}, false
		}
		read, err := shimwire.ParseWorkspaceBundle(body)
		if err != nil {
			t.Fatalf("the slices the launcher was sent rebuilt no workspace bundle: %v", err)
		}
		return read, true
	}
	// accepted is the Sandbox creation that the resolver hands the session of the tester's
	// launcher on the pod of loc, read off the informer's Sandbox as for any hello.
	accepted := func(t *testing.T, g *rig, loc runtime.Locator) time.Time {
		t.Helper()
		handler, _, reason := g.r.LauncherResolver()(rigLauncherHello(g, loc.Sandbox.Name, loc.Sandbox.PodUID, claim.RoleTester))
		session, ok := handler.(*launcherSession)
		if !ok {
			t.Fatalf("the tester's launcher on the pod %s was refused: %s", loc.Sandbox.PodUID, reason)
		}
		return session.keptSince
	}

	t.Run("a kept Sandbox made before the save is sent nothing and the bundle is dropped", func(t *testing.T) {
		st := bundleStore(t)
		option, drops := wired(st)
		var logs logBuffer
		g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)), option, withLog(&logs))
		spec := workerSpec(t)
		loc := g.spawn(spec)
		s := g.sandbox(loc.Sandbox.Name)
		if s == nil || !ownsKeptVolume(s) {
			t.Fatalf("the issue's Sandbox = %+v, want one that owns the kept volume, or this case proves nothing", s)
		}

		// The idle release takes the work off the pod, saves it, and keeps the Sandbox and its
		// volume.
		saved := savedWorkspaceBundle()
		saved.At = rigNow.Add(10 * time.Minute)
		saved.PodUID = loc.Sandbox.PodUID
		if err := st.PutWorkspaceBundle(g.ctx, testTree, saved.Release, saved); err != nil {
			t.Fatalf("save the released workspace's bundle: %v", err)
		}
		if !s.CreationTimestamp.Time.Before(saved.At) {
			t.Fatalf("the kept Sandbox was made %s, want it before the save at %s", s.CreationTimestamp, saved.At)
		}
		g.advance(11 * time.Minute)
		if err := g.r.ReleaseIssue(g.ctx, testTree, testTree, authorized); err != nil {
			t.Fatalf("ReleaseIssue: %v", err)
		}
		if kept := g.sandbox(loc.Sandbox.Name); kept == nil || kept.UID != s.UID {
			t.Fatalf("after the release the issue's Sandbox is %+v, want %s kept", kept, s.UID)
		}

		// The wake's launch brings the next pod of the same Sandbox, and its role starts.
		before := len(g.sent(spec.Claim))
		loc = g.relaunchOnNewPod(loc, &spec)
		frames := g.sent(spec.Claim)[before:]
		if !slices.ContainsFunc(frames, func(f shimwire.Frame) bool { _, ok := f.(shimwire.LauncherStart); return ok }) {
			t.Fatalf("the next pod's launcher was sent %v, want the role's start", frameTypes(frames))
		}
		if read, sent := sentBundle(t, frames); sent {
			t.Fatalf("the pod of the kept Sandbox was sent the bundle of release %d saved at %s, want nothing: its volume holds the work",
				testRelease, read.At)
		}
		if got := drops(); len(got) != 1 || got[0].release != testRelease || !got[0].keptSince.Equal(s.CreationTimestamp.Time) {
			t.Fatalf("the store dropped %+v, want release %d once, for the kept Sandbox made %s", got, testRelease, s.CreationTimestamp)
		}
		if _, held, err := st.WorkspaceBundleSize(g.ctx, testTree); err != nil || held {
			t.Fatalf("after the drop the store holds a bundle for %s = %t (%v), want none", testTree, held, err)
		}
		lines := linesHolding(logs.String(), "kept volume holds")
		if len(lines) != 1 || !strings.Contains(lines[0], "level=INFO") || !strings.Contains(lines[0], "issue="+testTree) ||
			!strings.Contains(lines[0], fmt.Sprintf("release=%d", testRelease)) {
			t.Fatalf("the runtime logged %q of the drop, want one Info line naming %s and release %d", lines, testTree, testRelease)
		}
		// The resolver hands a hello of that pod the same creation, off the informer's Sandbox.
		if since := accepted(t, g, loc); !since.Equal(s.CreationTimestamp.Time) {
			t.Fatalf("the resolver accepted the pod's launcher with its kept volume made %s, want %s", since, s.CreationTimestamp)
		}
	})

	t.Run("a kept Sandbox made after the save, as a reset makes it, is sent the bundle", func(t *testing.T) {
		st := bundleStore(t)
		saved := savedWorkspaceBundle()
		if err := st.PutWorkspaceBundle(context.Background(), testTree, saved.Release, saved); err != nil {
			t.Fatalf("save the released workspace's bundle: %v", err)
		}
		option, drops := wired(st)
		g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)), option)
		spec := workerSpec(t)
		loc := g.spawn(spec)
		s := g.sandbox(loc.Sandbox.Name)
		if s == nil || !ownsKeptVolume(s) || !s.CreationTimestamp.Time.After(saved.At) {
			t.Fatalf("the issue's Sandbox = %+v, want one that owns the kept volume, made after the save at %s", s, saved.At)
		}
		read, sent := sentBundle(t, g.sent(spec.Claim))
		if !sent || !bytes.Equal(read.Bundle, saved.Bundle) || read.Head != saved.Head {
			t.Fatalf("the pod of the kept Sandbox made after the save was sent %d bytes at head %s (sent %t), want the %d saved at %s",
				len(read.Bundle), read.Head, sent, len(saved.Bundle), saved.Head)
		}
		if got := drops(); len(got) != 0 {
			t.Fatalf("the store dropped %+v, want nothing: the new volume holds none of the work", got)
		}
		if since := accepted(t, g, loc); !since.Equal(s.CreationTimestamp.Time) {
			t.Fatalf("the resolver accepted the pod's launcher with its kept volume made %s, want %s", since, s.CreationTimestamp)
		}
	})

	t.Run("a Sandbox with an ephemeral volume, made before the save, is sent the bundle", func(t *testing.T) {
		st := bundleStore(t)
		option, drops := wired(st)
		g := newRig(t, nil, option)
		spec := workerSpec(t)
		loc := g.spawn(spec)
		s := g.sandbox(loc.Sandbox.Name)
		if s == nil || ownsKeptVolume(s) {
			t.Fatalf("the issue's Sandbox = %+v, want one whose pod's volume is its own", s)
		}
		saved := savedWorkspaceBundle()
		saved.At = rigNow.Add(10 * time.Minute)
		saved.PodUID = loc.Sandbox.PodUID
		if err := st.PutWorkspaceBundle(g.ctx, testTree, saved.Release, saved); err != nil {
			t.Fatalf("save the released workspace's bundle: %v", err)
		}
		if !s.CreationTimestamp.Time.Before(saved.At) {
			t.Fatalf("the Sandbox was made %s, want it before the save at %s, or this case proves nothing", s.CreationTimestamp, saved.At)
		}
		g.advance(11 * time.Minute)

		// An eviction deletes the pod and its volume, and leaves the Sandbox Running.
		if err := g.kube.Tracker().Delete(podsGVR, testNamespace, loc.Sandbox.Name); err != nil {
			t.Fatal(err)
		}
		g.eventually("the pod store to lose the evicted pod", func() bool {
			pod := g.r.storedPod(loc.Sandbox.Name)
			return pod == nil || string(pod.UID) != loc.Sandbox.PodUID
		})
		before := len(g.sent(spec.Claim))
		loc = g.relaunchOnNewPod(loc, &spec)
		if kept := g.sandbox(loc.Sandbox.Name); kept == nil || kept.UID != s.UID {
			t.Fatalf("after the eviction the issue's Sandbox is %+v, want %s kept", kept, s.UID)
		}
		read, sent := sentBundle(t, g.sent(spec.Claim)[before:])
		if !sent || !bytes.Equal(read.Bundle, saved.Bundle) || read.Head != saved.Head {
			t.Fatalf("the next pod of the ephemeral volume was sent %d bytes at head %s (sent %t), want the %d saved at %s",
				len(read.Bundle), read.Head, sent, len(saved.Bundle), saved.Head)
		}
		if got := drops(); len(got) != 0 {
			t.Fatalf("the store dropped %+v, want nothing: the pod's new volume holds none of the work", got)
		}
		if since := accepted(t, g, loc); !since.IsZero() {
			t.Fatalf("the resolver accepted the pod's launcher with a kept volume made %s, want none", since)
		}
	})
}
