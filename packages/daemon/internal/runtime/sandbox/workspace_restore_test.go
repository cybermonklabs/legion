package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// testRelease is the id of the idle release whose save the fixture's bundle carries: the daemon
// keeps the bundle of the release a launcher's restore report names, so a stored bundle has one.
const testRelease int64 = 11

// withSavedWorkspace gives the rig's runtime both halves of a restore over st: the one take that
// chooses between the issue's two copies and spends their attempts, and the keep the launcher's
// report runs, routed on the restore id alone by the store that owns both tables
// (store.KeepRestoredWorkspaceCopy) — the same two methods the daemon wires onto these options
// in production (daemon's sandboxRuntime).
func withSavedWorkspace(st *store.Store) rigOption {
	return withOptions(func(o *Options) {
		o.SavedWorkspaceBundle = st.TakeWorkspaceCopyForRestore
		o.WorkspaceBundleRestored = st.KeepRestoredWorkspaceCopy
	})
}

// runningCopySize is the size of issue's running copy and whether the issue holds one, read off
// the row itself: the store serves no such read, since nothing in the daemon decides on it but the
// restore's own transaction, and a test asks what that transaction left.
func runningCopySize(t *testing.T, st *store.Store, issue string) (int, bool) {
	t.Helper()
	var size int
	err := st.Pool().QueryRow(context.Background(), `select size_bytes from workspace_running_copies where issue = $1`, issue).Scan(&size)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read the running copy of %s: %v", issue, err)
	}
	return size, true
}

// runningCopySource is the pod issue's running copy names as the one its work is on, read off the
// row as runningCopySize is; "" when the issue holds none.
func runningCopySource(t *testing.T, st *store.Store, issue string) string {
	t.Helper()
	var source string
	err := st.Pool().QueryRow(context.Background(), `select source_pod_uid from workspace_running_copies where issue = $1`, issue).Scan(&source)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read the running copy of %s: %v", issue, err)
	}
	return source
}

// storedTime is the one time query reads off the store's own rows — a copy's `at`, a push's time,
// a pod's first ask — which a test that reads a restore's line compares against: every time the
// restore compares is the store's, and the line names those.
func storedTime(t *testing.T, st *store.Store, query string, args ...any) time.Time {
	t.Helper()
	var at time.Time
	if err := st.Pool().QueryRow(context.Background(), query, args...).Scan(&at); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return at
}

// The three times a restore's lines name, each as the store wrote it.
const (
	releaseBundleWritten = `select at from workspace_bundles where issue = $1`
	runningCopyWritten   = `select at from workspace_running_copies where issue = $1`
	firstAsked           = `select first_asked_at from workspace_restore_asks where issue = $1 and pod_uid = $2`
)

// runningWorkspaceCopy is the copy of a live pod's workspace as the daemon's cadence takes it:
// the same shape as a release's bundle, with its own body and its own head, taken off the pod
// named. It carries no release id, the table giving every copy a restore id of its own, and no
// time, the store stamping the copy with its own clock as it writes it
// (store.PutRunningWorkspaceCopy).
func runningWorkspaceCopy(pod string) runtime.WorkspaceBundle {
	return runtime.WorkspaceBundle{
		Bundle: bytes.Repeat([]byte("PACKjj\x00\x03"), (300<<10)/8),
		Head:   "9c0dd4f0ba3d8e7a1f4b2c6e5d8a9b0c1d2e3f40",
		Tips:   []string{"7b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f708192"},
		Bookmarks: map[string]string{
			"legion/" + testTree: "5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7081",
			"side":               "7b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f708192",
		},
		PodUID: pod,
	}
}

// savedWorkspaceBundle is a release's saved work as the store holds it: more than one
// shimwire.LauncherBundlePayloadBytes of git bundle, so the hop to the launcher is a sequence of
// slices and not one frame, with the head, the tips and the bookmarks a restore needs beside it —
// the issue's own branch, which the launcher is never sent, and one other, which it is. A test
// that saves it before its rig makes any pod has every pod of the rig first ask after the save,
// which is the only kind of pod a bundle is sent to (store.TakeWorkspaceCopyForRestore). Its
// source pod, the pod the work came off, is a pod of an earlier life of the issue that the rig
// never makes, so no pod of the rig is refused by uid; a test of the pod the work came off names
// that pod itself.
func savedWorkspaceBundle() runtime.WorkspaceBundle {
	return runtime.WorkspaceBundle{
		Bundle: bytes.Repeat([]byte("PACKjj\x00\x02"), (600<<10)/8),
		Head:   "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips:   []string{"da4f9f492e9f8349389c243743e0a000cd1c676f"},
		Bookmarks: map[string]string{
			"legion/" + testTree: "dffe88ae2f793e13436cef3921adc988433fbd4e",
			"side":               "da4f9f492e9f8349389c243743e0a000cd1c676f",
		},
		Release: testRelease,
		PodUID:  "pod-the-release-took-the-work-off",
	}
}

// sentBookmarks is what a launcher is sent of the bookmarks of a copy made by one of the two
// fixtures above: the one that is neither the issue's own branch nor its rescue branch. No restore
// sets either (bundleFrames), so each stays where the pod's own fetch put it, on what GitHub holds.
func sentBookmarks(b runtime.WorkspaceBundle) map[string]string {
	return map[string]string{"side": b.Bookmarks["side"]}
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
// ahead of the role's command (internal/launcher). Every bookmark of the bundle crosses but the
// issue's own branch and its rescue branch, and the daemon logs, in one line, the commit the
// bundle held each of those two on.
func TestASavedWorkspaceBundleReachesThePodsLauncherBeforeItStartsAnyRole(t *testing.T) {
	st := migratedStore(t)
	logs := &logBuffer{}
	saved := savedWorkspaceBundle()
	// The copy holds a local bookmark of the issue's rescue branch on a commit of its own, as the
	// shim's bundler keeps one that no remote branch holds.
	branch, rescue := "legion/"+testTree, "legion/"+testTree+"-rescue"
	saved.Bookmarks[rescue] = "0c4e5f6a7b8c9d0e1f2a3b4c5d6e7f8091a2b3c4"
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	written := storedTime(t, st, releaseBundleWritten, testTree)
	g := newRig(t, nil, withLog(logs), withSavedWorkspace(st))
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
	if want := sentBookmarks(saved); read.Head != saved.Head || !slices.Equal(read.Tips, saved.Tips) || !maps.Equal(read.Bookmarks, want) {
		t.Fatalf("the bundle crossed as head %s, tips %v and bookmarks %v, want %s, %v and %v",
			read.Head, read.Tips, read.Bookmarks, saved.Head, saved.Tips, want)
	}
	if !read.At.Equal(written) {
		t.Fatalf("the bundle crossed as written at %s, want the %s the store stamped it with", read.At, written)
	}
	line := restoreLine(t, logs, withheldBranchLine)
	carries := []string{"issue=" + testTree, `copy="release bundle"`}
	for _, name := range []string{branch, rescue} {
		carries = append(carries, name+":"+saved.Bookmarks[name])
	}
	for _, want := range carries {
		if !strings.Contains(line, want) {
			t.Fatalf("the withheld bookmarks logged %s, want it to carry %s", line, want)
		}
	}
	if strings.Contains(line, saved.Bookmarks["side"]) {
		t.Fatalf("the withheld bookmarks logged %s, which names the side bookmark the launcher was sent", line)
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
	st := migratedStore(t)
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	const refusal = "git bundle unbundle: object not found"
	g := newRig(t, nil, withFailedRestore(refusal), withSavedWorkspace(st))
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
// so it is the pod's identity and no time that leaves the attempt where it belongs.
func TestARelaunchInThePodBeingReleasedLeavesTheAttemptForItsReplacement(t *testing.T) {
	st := migratedStore(t)
	g := newRig(t, nil, withSavedWorkspace(st))
	spec := workerSpec(t)
	g.spawn(spec)
	released := g.pod(SandboxName(spec.Claim))

	// The release takes the workspace's unpushed work off that pod and saves it, while the pod
	// and its roles are still there: the save names that pod, as the runtime's own does
	// (Runtime.BundleWorkspace).
	saved := savedWorkspaceBundle()
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

	// The teardown takes the pod, the wake brings its replacement, and that pod — whose first ask
	// comes after the save — is answered the whole of the work.
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
	if want := sentBookmarks(saved); !bytes.Equal(read.Bundle, saved.Bundle) || read.Head != saved.Head ||
		!slices.Equal(read.Tips, saved.Tips) || !maps.Equal(read.Bookmarks, want) {
		t.Fatalf("the replacement pod was sent %d bytes at head %s with tips %v and bookmarks %v, want the %d saved at %s, %v and %v",
			len(read.Bundle), read.Head, read.Tips, read.Bookmarks, len(saved.Bundle), saved.Head, saved.Tips, want)
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
	st := migratedStore(t)
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

	d := newLaunchers(nil, st.TakeWorkspaceCopyForRestore, st.KeepRestoredWorkspaceCopy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	spec := workerSpec(t)
	session := d.accept(spec.Claim, testTree, shimwire.LauncherHello{LauncherID: "late", PodUID: "pod-uid"})
	frames, _, err := bundleFrames(testTree, first)
	if err != nil {
		t.Fatal(err)
	}
	d.restoreReported(session, shimwire.LauncherRestored{ID: frames[0].ID, Release: first.Release, OK: true})

	restore, err := st.TakeWorkspaceCopyForRestore(ctx, testTree, "pod-uid")
	if err != nil {
		t.Fatal(err)
	}
	if kept := restore.ReleaseBundle; kept.Take != runtime.WorkspaceBundleSend || !bytes.Equal(kept.Bundle.Bundle, second.Bundle) || kept.Bundle.Release != second.Release {
		t.Fatalf("after the first release's report the store holds %q of release %d (take=%v), want the %q release %d saved since",
			kept.Bundle.Bundle, kept.Bundle.Release, kept.Take, second.Bundle, second.Release)
	}
	// The pod of the second release restores it, and that report drops it.
	later, _, err := bundleFrames(testTree, second)
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
	st := migratedStore(t)
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	const refusal = "launcher_bundle sequence rebuilt to another digest than its slices declared"
	g := newRig(t, nil, withRefusedBundle(refusal), withSavedWorkspace(st))
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
	st := migratedStore(t)
	taken := runningWorkspaceCopy("pod-one")
	if err := st.PutRunningWorkspaceCopy(context.Background(), testTree, taken); err != nil {
		t.Fatalf("take the running copy off pod-one: %v", err)
	}
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
	kept := storedTime(t, st, runningCopyWritten, testTree)

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
// is recorded on it. Here the pod's first role start asked while the issue held nothing, and then
// the copy of the pod before it, read before that pod ended, was written. The pod's next role
// start — its roles container restarting — is sent nothing of it: the pod has been working since
// its first ask, and the copy would `jj edit` its workspace back over that work. The daemon says
// so at Info, naming the pod's first ask and the copy's time, both by the store's clock, and the
// copy keeps its one attempt, so the pod after this one is sent the work.
func TestARestoreLeavesACopyWrittenAfterThePodFirstAskedForThePodAfterIt(t *testing.T) {
	st := migratedStore(t)
	logs := &logBuffer{}
	g := newRig(t, nil, withLog(logs), withSavedWorkspace(st))
	spec := workerSpec(t)
	g.spawn(spec)
	pod := string(g.pod(SandboxName(spec.Claim)).UID)
	if frames := g.sent(spec.Claim); countOf(frames, shimwire.TypeLauncherBundle) != 0 {
		t.Fatalf("the pod's first role start was sent %v, want no slice: the issue held nothing", frameTypes(frames))
	}
	late := runningWorkspaceCopy("pod-before-this-one")
	if err := st.PutRunningWorkspaceCopy(g.ctx, testTree, late); err != nil {
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

// loggedAs is the value slog's text handler writes for an instant, which a test that reads one of
// the restore's own lines compares against (slog.TextHandler writes RFC 3339 to milliseconds).
func loggedAs(at time.Time) string { return at.Format("2006-01-02T15:04:05.000Z07:00") }

// restoreLine is the one line of the log that carries fragment, and fails the test when no line
// does: a restore writes one line about what it did with each copy, and a test that read "some
// line contains this" would pass on the wrong one.
func restoreLine(t *testing.T, logs *logBuffer, fragment string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, fragment) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the log holds %d lines carrying %q, want one:\n%s", len(found), fragment, logs.String())
	}
	return found[0]
}

// bundleBody is the whole of what the launcher was sent before the role's start, parsed back out
// of its slices, and the number of slices it took.
func bundleBody(t *testing.T, frames []shimwire.Frame) (shimwire.WorkspaceBundle, int) {
	t.Helper()
	var body []byte
	count := 0
	for _, frame := range frames {
		slice, ok := frame.(shimwire.LauncherBundle)
		if !ok {
			break
		}
		body = append(body, slice.Payload...)
		count++
	}
	if count == 0 {
		t.Fatalf("the launcher was sent %v, want a saved copy's slices before the role's start", frameTypes(frames))
	}
	read, err := shimwire.ParseWorkspaceBundle(body)
	if err != nil {
		t.Fatalf("the %d slices rebuilt no workspace bundle: %v", count, err)
	}
	return read, count
}

// A pod whose issue holds no release bundle at all is still sent work: the copy the daemon took
// while the pod that held it was running. That is the case this whole change exists for — a pod
// the cluster ended without a release, by an eviction, a node drain or an OOM kill, whose issue
// therefore has no release bundle — and the hop is the same one a release's bundle takes, down to
// the launcher's report, which keeps the copy as this pod's own because its restore id names it
// (store.MinRunningWorkspaceCopyRestoreID).
func TestAPodWhoseIssueHoldsOnlyARunningWorkspaceCopyRestoresIt(t *testing.T) {
	st := migratedStore(t)
	logs := &logBuffer{}
	taken := runningWorkspaceCopy("pod-the-cadence-copied")
	if err := st.PutRunningWorkspaceCopy(context.Background(), testTree, taken); err != nil {
		t.Fatalf("take the running copy of the workspace: %v", err)
	}
	written := storedTime(t, st, runningCopyWritten, testTree)
	g := newRig(t, nil, withLog(logs), withSavedWorkspace(st))
	spec := workerSpec(t)
	g.spawn(spec)

	read, slices := bundleBody(t, g.sent(spec.Claim))
	if want := sentBookmarks(taken); !bytes.Equal(read.Bundle, taken.Bundle) || read.Head != taken.Head ||
		!maps.Equal(read.Bookmarks, want) || !read.At.Equal(written) {
		t.Fatalf("the pod was sent %d bytes at head %s with bookmarks %v written at %s, want the %d of the running copy at %s with %v",
			len(read.Bundle), read.Head, read.Bookmarks, read.At, len(taken.Bundle), written, want)
	}
	// The line an operator reads says which of the two copies went back and when it was taken,
	// since the two differ in nothing else that is visible from outside.
	line := restoreLine(t, logs, reachedLine)
	for _, want := range []string{"issue=" + testTree, `copy="running copy"`, "taken=" + loggedAs(written)} {
		if !strings.Contains(line, want) {
			t.Fatalf("the restore logged %s, want it to carry %s", line, want)
		}
	}
	// The launcher reported the restore, and the report's id named the running copy: the row is
	// this pod's own now, the same work answered nothing to this pod, and the issue holds no
	// release bundle.
	if size, held := runningCopySize(t, st, testTree); !held || size != len(taken.Bundle) {
		t.Fatalf("after the launcher reported its restore the store holds %d bytes of the running copy (held=%t), want its %d kept", size, held, len(taken.Bundle))
	}
	if source := runningCopySource(t, st, testTree); source != string(g.pod(SandboxName(spec.Claim)).UID) {
		t.Fatalf("the kept running copy names pod %q as its source, want the pod that restored it", source)
	}
	if _, held, err := st.WorkspaceBundleSize(g.ctx, testTree); err != nil || held {
		t.Fatalf("the store holds a release bundle = %t (%v), want none: the issue was never released", held, err)
	}
	if slices < 2 {
		t.Fatalf("the running copy crossed in %d slices, want the sequence a copy of its size takes", slices)
	}
}

// An issue can hold both copies at once: a release saved a bundle, a wake cancelled it, the pod
// worked on and the cadence copied it again; or the cadence copied a pod minutes before the
// release that saved the bundle. The pod that replaces it is sent the newer of the two, and the
// older is closed to every later pod without being sent to this one.
//
// Closing it is the point. A copy left with its attempt unspent is a copy the pod after this one
// can still be sent, and that pod's workspace holds the descendants of the work this restore puts
// back, so the older copy would be applied over everything pushed in between. Its bytes stay in
// the database all the same, for an operator who wants them.
func TestAPodWhoseIssueHoldsBothSavedCopiesRestoresTheNewerAndClosesTheOlder(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	logs := &logBuffer{}
	// The release saved its bundle, a wake cancelled nothing of it, and the cadence took a copy
	// of the same workspace after it: the copy is the newer work.
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(ctx, testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	savedAt := storedTime(t, st, releaseBundleWritten, testTree)
	taken := runningWorkspaceCopy("pod-the-cadence-copied")
	if err := st.PutRunningWorkspaceCopy(ctx, testTree, taken); err != nil {
		t.Fatalf("take the running copy of the workspace: %v", err)
	}
	g := newRig(t, nil, withLog(logs), withSavedWorkspace(st))
	spec := workerSpec(t)
	g.spawn(spec)

	read, _ := bundleBody(t, g.sent(spec.Claim))
	if !bytes.Equal(read.Bundle, taken.Bundle) || read.Head != taken.Head {
		t.Fatalf("the pod was sent %d bytes at head %s, want the %d of the newer running copy at %s",
			len(read.Bundle), read.Head, len(taken.Bundle), taken.Head)
	}
	line := restoreLine(t, logs, supersededLine)
	for _, want := range []string{"issue=" + testTree, `copy="release bundle"`, "taken=" + loggedAs(savedAt),
		`newer="running copy"`, "newerTake=" + runtime.WorkspaceBundleSend.String()} {
		if !strings.Contains(line, want) {
			t.Fatalf("the closed copy logged %s, want it to carry %s", line, want)
		}
	}
	// The older copy's bytes are where they were, and no pod of this issue is ever sent them.
	size, held, err := st.WorkspaceBundleSize(g.ctx, testTree)
	if err != nil || !held || size != len(saved.Bundle) {
		t.Fatalf("the store holds %d bytes of the release bundle (held=%t, %v), want its %d kept for an operator",
			size, held, err, len(saved.Bundle))
	}
	restore, err := st.TakeWorkspaceCopyForRestore(g.ctx, testTree, "pod-after-this-one")
	if later := restore.ReleaseBundle; err != nil || later.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("the pod after this one was answered %d bytes of the release bundle (%v, %v), want nothing: this restore spent its attempt",
			len(later.Bundle.Bundle), later.Take, err)
	}
	// The newer copy went back and its report kept it as this pod's own, beside the one copy
	// nothing restored.
	if source := runningCopySource(t, st, testTree); source != string(g.pod(SandboxName(spec.Claim)).UID) {
		t.Fatalf("after the launcher reported its restore the running copy names pod %q, want the pod that restored it", source)
	}
}

// The line of a closed copy names the newer copy and what this pod's ask did with it, which is not
// always a send. Here the newer copy is this pod's own: its first role start could not read the
// store, so it started on what was pushed with nothing of its ask recorded, worked on, and was
// copied, and the release bundle of the pod before it was never restored. The pod's next role
// start is sent neither: its own copy is its workspace's work, and the bundle is closed all the
// same, since a pod that restored it would put it over the newer work. Its line says the newer
// one was answered nothing here rather than naming no copy at all.
func TestTheLineOfAClosedCopyNamesTheNewerOneAndWhatThisPodWasAnsweredOfIt(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	logs := &logBuffer{}
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(ctx, testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	savedAt := storedTime(t, st, releaseBundleWritten, testTree)
	for _, statement := range []string{
		`create function refuse_restore_ask() returns trigger language plpgsql as $$ begin raise exception 'the store cannot be read'; end $$`,
		`create trigger refuse_restore_ask before insert on workspace_restore_asks for each row execute function refuse_restore_ask()`,
	} {
		if _, err := st.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("make the pod's first take fail: %v", err)
		}
	}
	g := newRig(t, nil, withLog(logs), withSavedWorkspace(st))
	spec := workerSpec(t)
	g.spawn(spec)
	pod := string(g.pod(SandboxName(spec.Claim)).UID)
	if frames := g.sent(spec.Claim); countOf(frames, shimwire.TypeLauncherBundle) != 0 {
		t.Fatalf("the pod whose take failed was sent %v, want no slice", frameTypes(frames))
	}
	if _, err := st.Pool().Exec(ctx, `drop trigger refuse_restore_ask on workspace_restore_asks`); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRunningWorkspaceCopy(ctx, testTree, runningWorkspaceCopy(pod)); err != nil {
		t.Fatalf("take the running copy of this pod's workspace: %v", err)
	}

	before := len(g.sent(spec.Claim))
	g.connect(spec.Claim)
	g.r.launchers.restoreWorkspace(g.ctx, spec.Claim, pod)
	for _, frame := range g.sent(spec.Claim)[before:] {
		if slice, ok := frame.(shimwire.LauncherBundle); ok {
			t.Fatalf("the pod was sent slice %d of %d of copy %d, want nothing: the newer copy is its own and the older is closed",
				slice.Index, slice.Count, slice.Release)
		}
	}
	line := restoreLine(t, logs, supersededLine)
	for _, want := range []string{"issue=" + testTree, `copy="release bundle"`, "taken=" + loggedAs(savedAt),
		`newer="running copy"`, "newerTake=" + runtime.WorkspaceBundleNone.String()} {
		if !strings.Contains(line, want) {
			t.Fatalf("the closed copy logged %s, want it to carry %s", line, want)
		}
	}
	if restore, err := st.TakeWorkspaceCopyForRestore(ctx, testTree, "pod-after-this-one"); err != nil || restore.ReleaseBundle.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("the pod after this one was answered %d bytes of the release bundle (%v, %v), want nothing: this pod closed it",
			len(restore.ReleaseBundle.Bundle.Bundle), restore.ReleaseBundle.Take, err)
	}
}

// A restore of a running copy that fails leaves the work where the launcher's notice says it is,
// in the daemon's database, spends that copy's one attempt and names the issue on the daemon's
// log. What it must not do is hold the issue's pod: the idle rule reads the release bundles alone
// (store.workspaceBundleUnrestored, workflow.IdleHoldBundleUnrestored), and this issue has no
// release bundle, so its pod is still released when it next goes idle — which is right, since the
// work in the copy is also on the disk the copy was taken off, and that release would save it
// again. The store's own side of this is TestTheIdleViewHoldsNoPodForARunningWorkspaceCopy.
func TestAFailedRestoreOfARunningWorkspaceCopyKeepsItAndHoldsNoPod(t *testing.T) {
	st := migratedStore(t)
	logs := &logBuffer{}
	taken := runningWorkspaceCopy("pod-the-cadence-copied")
	if err := st.PutRunningWorkspaceCopy(context.Background(), testTree, taken); err != nil {
		t.Fatalf("take the running copy of the workspace: %v", err)
	}
	const refusal = "git bundle unbundle: object not found"
	g := newRig(t, nil, withLog(logs), withFailedRestore(refusal), withSavedWorkspace(st))
	spec := workerSpec(t)
	loc := g.spawn(spec)
	if loc.Sandbox.Generation != spec.Generation {
		t.Fatalf("the failed restore left the launch at %+v, want generation %d", loc, spec.Generation)
	}

	line := restoreLine(t, logs, restoreFailedLine)
	for _, want := range []string{"issue=" + testTree, refusal} {
		if !strings.Contains(line, want) {
			t.Fatalf("the failed restore logged %s, want it to carry %s", line, want)
		}
	}
	if size, held := runningCopySize(t, st, testTree); !held || size != len(taken.Bundle) {
		t.Fatalf("after the failed restore the store holds %d bytes of the running copy (held=%t), want its %d kept",
			size, held, len(taken.Bundle))
	}
	// That send was the copy's one attempt: the pod after this one is sent nothing of it.
	restore, err := st.TakeWorkspaceCopyForRestore(g.ctx, testTree, "pod-after-the-failure")
	if after := restore.Running; err != nil || after.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("the pod after the failed restore was answered %d bytes (%v, %v), want nothing", len(after.Bundle.Bundle), after.Take, err)
	}
	// Nothing of this issue holds its pod: there is no release bundle, which is the only row the
	// idle rule reads.
	if _, held, err := st.WorkspaceBundleSize(g.ctx, testTree); err != nil || held {
		t.Fatalf("the store holds a release bundle = %t (%v), want none: a failed restore of a running copy saves no bundle", held, err)
	}
}

// A pod whose issue's newer copy is a running copy older than the newest push of its branch the
// daemon recorded is sent nothing, and the daemon says so in a line an operator can act on: the
// issue, which copy, when it was written, and the push it would have rolled back. Restoring it
// would `jj edit` back to its head and move each bookmark `--allow-backwards` behind the pushed
// commits. The older release bundle beside it is not sent in its place: it is older still, so it
// is closed unsent by the newer copy, and its line names that copy and the refusal. The pod starts
// on what was pushed, and the bytes stay in the database for an operator. Every time is the
// store's, so the writes are made in the order the case needs: the bundle, the copy, the push.
func TestARestoreSendsNoRunningCopyOlderThanTheIssuesNewestPushAndLogsThePushItWouldRollBack(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	logs := &logBuffer{}
	earlier := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(ctx, testTree, earlier.Release, earlier); err != nil {
		t.Fatalf("save an earlier release's bundle: %v", err)
	}
	earlierAt := storedTime(t, st, releaseBundleWritten, testTree)
	taken := runningWorkspaceCopy("pod-the-cadence-copied")
	if err := st.PutRunningWorkspaceCopy(ctx, testTree, taken); err != nil {
		t.Fatalf("take the running copy of the workspace: %v", err)
	}
	written := storedTime(t, st, runningCopyWritten, testTree)
	const beforeHead, pushedHead = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1", "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		_, err := st.RecordIssuePush(ctx, tx, testTree, beforeHead, pushedHead)
		return err
	}); err != nil {
		t.Fatalf("record the push of the issue's branch: %v", err)
	}
	pushed := storedTime(t, st, `select pushed_at from issue_pushes where issue = $1`, testTree)
	g := newRig(t, nil, withLog(logs), withSavedWorkspace(st))
	spec := workerSpec(t)
	g.spawn(spec)

	for _, frame := range g.sent(spec.Claim) {
		if slice, ok := frame.(shimwire.LauncherBundle); ok {
			t.Fatalf("the pod was sent slice %d of %d of copy %d, want nothing: the newer copy is older than the push",
				slice.Index, slice.Count, slice.Release)
		}
	}
	line := restoreLine(t, logs, refusedPushedLine)
	for _, want := range []string{"issue=" + testTree, `copy="running copy"`, "taken=" + loggedAs(written),
		"pushed=" + loggedAs(pushed), "pushedHead=" + pushedHead} {
		if !strings.Contains(line, want) {
			t.Fatalf("the refusal of the running copy logged %q, want it to carry %s", line, want)
		}
	}
	line = restoreLine(t, logs, supersededLine)
	for _, want := range []string{"issue=" + testTree, `copy="release bundle"`, "taken=" + loggedAs(earlierAt),
		`newer="running copy"`, "newerTake=" + runtime.WorkspaceBundleRefusedPushed.String()} {
		if !strings.Contains(line, want) {
			t.Fatalf("the closed release bundle logged %q, want it to carry %s", line, want)
		}
	}
	if size, held, err := st.WorkspaceBundleSize(ctx, testTree); err != nil || !held || size != len(earlier.Bundle) {
		t.Fatalf("the store holds %d bytes of the release bundle (held=%t, %v), want its %d kept for an operator", size, held, err, len(earlier.Bundle))
	}
	if size, held := runningCopySize(t, st, testTree); !held || size != len(taken.Bundle) {
		t.Fatalf("the store holds %d bytes of the running copy (held=%t), want its %d kept for an operator", size, held, len(taken.Bundle))
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
	st := migratedStore(t)
	ctx := context.Background()
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(ctx, testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
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
	st := migratedStore(t)
	taken := runningWorkspaceCopy("pod-one")
	if err := st.PutRunningWorkspaceCopy(context.Background(), testTree, taken); err != nil {
		t.Fatalf("take the running copy off pod-one: %v", err)
	}
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
	st := migratedStore(t)
	saved := savedWorkspaceBundle()
	saved.PodUID = "pod-one"
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved.Release, saved); err != nil {
		t.Fatalf("save pod-one's release bundle: %v", err)
	}
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
