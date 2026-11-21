// The restore to a pod whose Sandbox owns the issue's kept volume: every copy of the issue's
// workspace taken since that Sandbox was made came off the volume the pod mounts, so the pod is
// sent none of it and the store drops it, and every other copy is restored as for any pod. The
// fixtures these tests share with the rest of the restore's tests are in workspace_restore_test.go.
package sandbox

import (
	"bytes"
	"context"
	"fmt"
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

// keptDrop is one drop the store answered that deleted a copy, beside the creation of the kept
// Sandbox the runtime asked it for.
type keptDrop struct {
	dropped   runtime.WorkspaceRestore
	keptSince time.Time
}

// withKeptVolumeDrop gives the rig's runtime the store's three calls of the restore as the daemon
// hands them to it (daemon's sandboxRuntime): the take and the keep of withSavedWorkspace, and the
// drop of the copies a pod's kept volume holds, bound to the rig's project. It answers the drops
// that deleted a copy, for the test to read.
func withKeptVolumeDrop(st *store.Store) (rigOption, func() []keptDrop) {
	var mu sync.Mutex
	var drops []keptDrop
	option := withOptions(func(o *Options) {
		o.SavedWorkspaceBundle, o.WorkspaceBundleRestored = takeCopies(st), st.KeepRestoredWorkspaceCopy
		o.HeldWorkspaceCopies = func(ctx context.Context, issue, podUID string, keptSince time.Time) (runtime.WorkspaceRestore, error) {
			dropped, err := st.DropHeldWorkspaceCopies(ctx, testProject, issue, podUID, keptSince)
			if dropped.ReleaseBundle.Take == runtime.WorkspaceBundleHeldByVolume || dropped.Running.Take == runtime.WorkspaceBundleHeldByVolume {
				mu.Lock()
				drops = append(drops, keptDrop{dropped: dropped, keptSince: keptSince})
				mu.Unlock()
			}
			return dropped, err
		}
	})
	return option, func() []keptDrop {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(drops)
	}
}

// sentCopy is the copy that the slices among frames rebuild, and false where frames hold no slice.
func sentCopy(t *testing.T, frames []shimwire.Frame) (shimwire.WorkspaceBundle, bool) {
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

// acceptedKeptSince is the Sandbox creation that the resolver hands the session of the tester's
// launcher on the pod of loc, read off the informer's Sandbox as for any hello.
func acceptedKeptSince(t *testing.T, g *rig, loc runtime.Locator) time.Time {
	t.Helper()
	handler, _, reason := g.r.LauncherResolver()(rigLauncherHello(g, loc.Sandbox.Name, loc.Sandbox.PodUID, claim.RoleTester))
	session, ok := handler.(*launcherSession)
	if !ok {
		t.Fatalf("the tester's launcher on the pod %s was refused: %s", loc.Sandbox.PodUID, reason)
	}
	return session.keptSince
}

// evict deletes the pod of loc as an eviction does, which leaves its Sandbox Running, and waits
// for the runtime's pod store to lose it.
func evict(t *testing.T, g *rig, loc runtime.Locator) {
	t.Helper()
	if err := g.kube.Tracker().Delete(podsGVR, testNamespace, loc.Sandbox.Name); err != nil {
		t.Fatal(err)
	}
	g.eventually("the pod store to lose the evicted pod", func() bool {
		pod := g.r.storedPod(loc.Sandbox.Name)
		return pod == nil || string(pod.UID) != loc.Sandbox.PodUID
	})
}

// A pod whose own kept volume holds the work of a copy is sent none of it (R5), the release bundle
// and the running copy alike. The release keeps the Sandbox that owns the issue's kept volume,
// Suspended, and an eviction keeps it Running, and the issue's next pod mounts that volume, with
// the workspace each copy was taken from. The pod's launcher would refuse a copy sent there,
// because the workspace is older than the copy (internal/launcher's restoreWorkspace). It would
// log a notice that the work was lost, and a release bundle would hold the issue's pod
// (workflow.IdleHoldBundleUnrestored). So the daemon sends that pod no slice, drops the copy, and
// logs one Info line that names which copy it dropped.
//
// A rule that drops the release bundle alone sends the running copy taken off the evicted pod,
// and fails the second case; a rule that sends every copy fails both.
func TestAKeptVolumeTakesNoSavedCopyItHolds(t *testing.T) {
	for _, kind := range []runtime.WorkspaceCopyKind{runtime.WorkspaceCopyReleaseBundle, runtime.WorkspaceCopyRunning} {
		t.Run(string(kind), func(t *testing.T) {
			st := bundleStore(t)
			option, drops := withKeptVolumeDrop(st)
			var logs logBuffer
			g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)), option, withLog(&logs))
			// The API server makes the Sandbox a minute before the copy is taken, by a clock that
			// agrees with the database's.
			g.now.Store(new(time.Now().UTC().Add(-time.Minute)))
			spec := workerSpec(t)
			loc := g.spawn(spec)
			s := g.sandbox(loc.Sandbox.Name)
			if s == nil || !ownsKeptVolume(s) {
				t.Fatalf("the issue's Sandbox = %+v, want one that owns the kept volume, or this case proves nothing", s)
			}

			var taken time.Time
			var want []byte
			switch kind {
			case runtime.WorkspaceCopyReleaseBundle:
				// The idle release takes the work off the pod, saves it, and keeps the Sandbox and
				// its volume.
				saved := savedWorkspaceBundle()
				saved.PodUID = loc.Sandbox.PodUID
				saveReleaseBundle(t, st, saved)
				taken, want = storedTime(t, st, releaseBundleTaken, testTree), saved.Bundle
				if err := g.r.ReleaseIssue(g.ctx, testTree, testTree, authorized); err != nil {
					t.Fatalf("ReleaseIssue: %v", err)
				}
			case runtime.WorkspaceCopyRunning:
				// The cadence copies the pod's workspace, and then the cluster evicts the pod.
				copied := runningWorkspaceCopy(loc.Sandbox.PodUID)
				saveRunningCopy(t, st, copied)
				taken, want = storedTime(t, st, runningCopyTaken, testTree), copied.Bundle
				evict(t, g, loc)
			}
			if !s.CreationTimestamp.Time.Before(taken) {
				t.Fatalf("the kept Sandbox was made %s, want it before the copy taken at %s", s.CreationTimestamp, taken)
			}

			// The wake's launch brings the next pod of the same Sandbox, and its role starts.
			before := len(g.sent(spec.Claim))
			loc = g.relaunchOnNewPod(loc, &spec)
			if kept := g.sandbox(loc.Sandbox.Name); kept == nil || kept.UID != s.UID {
				t.Fatalf("the next pod's Sandbox is %+v, want %s kept", kept, s.UID)
			}
			frames := g.sent(spec.Claim)[before:]
			if !slices.ContainsFunc(frames, func(f shimwire.Frame) bool { _, ok := f.(shimwire.LauncherStart); return ok }) {
				t.Fatalf("the next pod's launcher was sent %v, want the role's start", frameTypes(frames))
			}
			if read, sent := sentCopy(t, frames); sent {
				t.Fatalf("the pod of the kept Sandbox was sent %d bytes of a copy taken at %s, want nothing: its volume holds the %s's %d bytes",
					len(read.Bundle), read.At, kind, len(want))
			}
			got := drops()
			if len(got) != 1 || got[0].dropped.Of(kind).Take != runtime.WorkspaceBundleHeldByVolume || !got[0].keptSince.Equal(s.CreationTimestamp.Time) {
				t.Fatalf("the store dropped %+v, want the %s once, for the kept Sandbox made %s", got, kind, s.CreationTimestamp)
			}
			if _, held, err := st.WorkspaceBundleSize(g.ctx, testTree); err != nil || held {
				t.Fatalf("after the drop the store holds a release bundle for %s = %t (%v), want none", testTree, held, err)
			}
			if _, held := runningCopySize(t, st, testTree); held {
				t.Fatalf("after the drop the store holds a running copy for %s, want none", testTree)
			}
			line := restoreLine(t, &logs, keptVolumeHoldsLine)
			for _, carried := range []string{"level=INFO", "issue=" + testTree, fmt.Sprintf("copy=%q", kind),
				fmt.Sprintf("restore=%d", got[0].dropped.Of(kind).Bundle.Release), "taken=" + loggedAs(taken),
				"volume_created=" + loggedAs(s.CreationTimestamp.Time)} {
				if !strings.Contains(line, carried) {
					t.Fatalf("the drop logged %q, want it to carry %s", line, carried)
				}
			}
			// The resolver hands a hello of that pod the same creation, off the informer's Sandbox.
			if since := acceptedKeptSince(t, g, loc); !since.Equal(s.CreationTimestamp.Time) {
				t.Fatalf("the resolver accepted the pod's launcher with its kept volume made %s, want %s", since, s.CreationTimestamp)
			}
		})
	}
}

// A pod whose volume does not hold the work of a copy restores it, the release bundle and the
// running copy alike: the pod of a kept Sandbox made after the copy was taken, which is the
// Sandbox that a reset or a change of the disk's shape makes, and the pod of a Sandbox with an
// ephemeral volume, whose own pod made its volume. That ephemeral Sandbox was made before the copy,
// as an eviction leaves it. So a rule that reads the Sandbox's age and not its volume fails the
// test, and so does a rule that never sends a copy.
func TestAPodWhoseVolumeHoldsNoneOfACopysWorkRestoresIt(t *testing.T) {
	save := func(t *testing.T, st *store.Store, kind runtime.WorkspaceCopyKind, pod string) []byte {
		t.Helper()
		if kind == runtime.WorkspaceCopyReleaseBundle {
			saved := savedWorkspaceBundle()
			saved.PodUID = pod
			saveReleaseBundle(t, st, saved)
			return saved.Bundle
		}
		copied := runningWorkspaceCopy(pod)
		saveRunningCopy(t, st, copied)
		return copied.Bundle
	}
	for _, kind := range []runtime.WorkspaceCopyKind{runtime.WorkspaceCopyReleaseBundle, runtime.WorkspaceCopyRunning} {
		t.Run(string(kind)+" of an earlier volume, to a kept Sandbox a reset made", func(t *testing.T) {
			st := bundleStore(t)
			want := save(t, st, kind, "pod-of-the-volume-the-reset-replaced")
			option, drops := withKeptVolumeDrop(st)
			g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)), option)
			// The reset makes the Sandbox an hour after the copy was taken.
			g.now.Store(new(time.Now().UTC().Add(time.Hour)))
			spec := workerSpec(t)
			loc := g.spawn(spec)
			s := g.sandbox(loc.Sandbox.Name)
			if s == nil || !ownsKeptVolume(s) {
				t.Fatalf("the issue's Sandbox = %+v, want one that owns the kept volume", s)
			}
			read, sent := sentCopy(t, g.sent(spec.Claim))
			if !sent || !bytes.Equal(read.Bundle, want) {
				t.Fatalf("the pod of the kept Sandbox made after the copy was sent %d bytes (sent %t), want the %s's %d", len(read.Bundle), sent, kind, len(want))
			}
			if got := drops(); len(got) != 0 {
				t.Fatalf("the store dropped %+v, want nothing: the new volume holds none of the work", got)
			}
			if since := acceptedKeptSince(t, g, loc); !since.Equal(s.CreationTimestamp.Time) {
				t.Fatalf("the resolver accepted the pod's launcher with its kept volume made %s, want %s", since, s.CreationTimestamp)
			}
		})

		t.Run(string(kind)+" to the next pod of an ephemeral volume", func(t *testing.T) {
			st := bundleStore(t)
			option, drops := withKeptVolumeDrop(st)
			g := newRig(t, nil, option)
			g.now.Store(new(time.Now().UTC().Add(-time.Minute)))
			spec := workerSpec(t)
			loc := g.spawn(spec)
			s := g.sandbox(loc.Sandbox.Name)
			if s == nil || ownsKeptVolume(s) {
				t.Fatalf("the issue's Sandbox = %+v, want one whose pod's volume is its own", s)
			}
			want := save(t, st, kind, loc.Sandbox.PodUID)

			// An eviction deletes the pod and its volume, and leaves the Sandbox Running.
			evict(t, g, loc)
			before := len(g.sent(spec.Claim))
			loc = g.relaunchOnNewPod(loc, &spec)
			if kept := g.sandbox(loc.Sandbox.Name); kept == nil || kept.UID != s.UID {
				t.Fatalf("after the eviction the issue's Sandbox is %+v, want %s kept", kept, s.UID)
			}
			read, sent := sentCopy(t, g.sent(spec.Claim)[before:])
			if !sent || !bytes.Equal(read.Bundle, want) {
				t.Fatalf("the next pod of the ephemeral volume was sent %d bytes (sent %t), want the %s's %d", len(read.Bundle), sent, kind, len(want))
			}
			if got := drops(); len(got) != 0 {
				t.Fatalf("the store dropped %+v, want nothing: the pod's new volume holds none of the work", got)
			}
			if since := acceptedKeptSince(t, g, loc); !since.IsZero() {
				t.Fatalf("the resolver accepted the pod's launcher with a kept volume made %s, want none", since)
			}
		})
	}
}
