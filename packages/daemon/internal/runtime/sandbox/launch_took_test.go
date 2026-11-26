package sandbox

// R7 of #5285 (spec v20, verification v15's c-forced-replacement-told), the launch's
// side: a launch that replaces an issue pod on its own account takes a disk that may hold work no
// role pushed, so it has the pod's launcher read the workspace first where a process in the pod can
// make the read, and once the suspension that takes the pod is written it hands the daemon what it
// took (Options.LaunchTook, runtime.TakenPod), which tells the issue's architect once (the daemon's
// tellLaunchTook, internal/daemon's launch_took_test.go). One test each for a moved facet
// (movedInPod), a superseded disk shape, a kept pod whose stored template cannot be read and a pod
// one of whose launchers is not bound to it, each with another role running in the pod and a
// workspace that holds one commit origin does not hold: the pod is replaced and the daemon is
// handed the commit. A pod that is not live, a pod whose launchers are not bound and none of which
// reads a workspace, and a kept volume a launch deletes with no pod of its Sandbox: the disk is
// taken, no read is sent, and the daemon is handed why no read could be made. Each fails with the
// hand-off taken out.

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// tookCommit is the one commit origin does not hold that the workspace of each pod here holds, and
// oneCommitRead the launcher's answer that names it.
const tookCommit = "f2f134541b30dfa5160a5d1ce5e17b76c967c661"

var oneCommitRead = shimwire.WorkspaceWorkResult{OK: true, Head: "9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d", Count: 1, Commits: []string{tookCommit}}

// tookLaunches is a LaunchTook that keeps every TakenPod the runtime hands it, as the daemon's
// tellLaunchTook takes them.
type tookLaunches struct {
	mu    sync.Mutex
	taken []runtime.TakenPod
}

func (l *tookLaunches) record(_ context.Context, taken runtime.TakenPod) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.taken = append(l.taken, taken)
}

func (l *tookLaunches) all() []runtime.TakenPod {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.taken)
}

// want holds the runtime to having handed the daemon exactly one replacement, want, under an
// operation of its own and with why it was made: every other field as want names it, the act's
// Takes and Target among them, why's words in its Why, and, where no read could be made, because's
// words in its Err ("" for a read that was made).
func (l *tookLaunches) want(t *testing.T, want runtime.TakenPod, why, because string) {
	t.Helper()
	taken := l.all()
	if len(taken) != 1 {
		t.Fatalf("the daemon was handed %d replacements %+v, want the one this launch made", len(taken), taken)
	}
	got := taken[0]
	if got.Operation == "" || got.Why == "" || !strings.Contains(got.Why, why) ||
		(got.Err == nil) != (because == "") || (got.Err != nil && !strings.Contains(got.Err.Error(), because)) {
		t.Fatalf("the daemon was handed %+v, want an operation of its own, why it was made (naming %q), and why no read could be made naming %q (\"\" for a read made)",
			got, why, because)
	}
	got.Operation, got.Why, got.Err = "", "", nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the daemon was handed %+v, want %+v", got, want)
	}
}

// readTaken is the replacement of pod that claim's launch made, which takes act, over the read
// through the pod's launcher that found tookCommit (oneCommitRead).
func readTaken(claim claim.Token, pod string, act runtime.RecordedAct) runtime.TakenPod {
	return runtime.TakenPod{Claim: claim, Issue: testTree, PodUID: pod, Act: act,
		Work: runtime.WorkspaceWork{Head: oneCommitRead.Head, Commits: []string{tookCommit}, Count: 1, PodUID: pod}}
}

// noReadSent holds every launcher of tokens to having been sent no workspace read.
func noReadSent(t *testing.T, g *rig, tokens ...claim.Token) {
	t.Helper()
	for _, token := range tokens {
		for _, frame := range g.sent(token) {
			if _, ok := frame.(shimwire.WorkspaceWork); ok {
				t.Fatalf("the launcher of %s was sent a workspace read, want none: no process of the pod could make it", token)
			}
		}
	}
}

// The launch replaces a pod whose launchers dial a stream this daemon does not serve (movedInPod):
// no worker in it can be asked to push its work first. The reviewer runs in the pod beside the
// tester's own process. The pod's launcher reads the workspace before the replacement, and the
// daemon is handed the commit it found.
func TestALaunchThatReplacesAPodWhoseStreamMovedSaysWhatItTook(t *testing.T) {
	var took tookLaunches
	g := newRig(t, nil, withWorkspaceRead(oneCommitRead))
	loc := g.spawn(workerSpec(t))
	r2 := restartedOver(t, g, loc, func(o *Options) {
		moveStream(o)
		o.LaunchTook = took.record
	})
	g.connectRunning(otherToken, 1)
	fresh := resumeTester(t, g, r2, loc)
	if fresh.Sandbox.PodUID == loc.Sandbox.PodUID {
		t.Fatalf("Resume kept the pod %s, whose launchers dial a stream this daemon does not serve", loc.Sandbox.PodUID)
	}
	took.want(t, readTaken(workerToken, loc.Sandbox.PodUID, runtime.PodAct(loc.Sandbox.PodUID)), "", "")
}

// The launch replaces an issue Sandbox of the disk shape before the pod-owned disk, Sandbox and pod
// together (supersededDisk), with the reviewer running in that pod. The pod's launcher reads the
// workspace first, and the daemon is handed the commit it found.
func TestALaunchThatReplacesASandboxOfTheSupersededDiskShapeSaysWhatItTook(t *testing.T) {
	var took tookLaunches
	g := supersededIssue(t, withWorkspaceRead(oneCommitRead), withOptions(func(o *Options) { o.LaunchTook = took.record }))
	g.connectRunning(otherToken, 1)
	loc := g.spawn(workerSpec(t))
	if loc.Sandbox.PodUID == "uid-pod-old" {
		t.Fatalf("the launch kept the pod uid-pod-old of the superseded disk shape, want a pod of the Sandbox it created")
	}
	// The launch takes the Sandbox's live pod with its volume (WithPod).
	want := readTaken(workerToken, "uid-pod-old", runtime.VolumeAct("uid-sandbox-superseded"))
	want.WithPod = true
	took.want(t, want, "disk shape", "")
}

// The launch replaces a drifted pod whose stored template says nothing this daemon can read of what
// the pod carries (keptBy), with the reviewer running in it. The pod's launcher reads the workspace
// first, and the daemon is handed the commit it found.
func TestALaunchThatCannotReadAKeptPodsTemplateSaysWhatItTook(t *testing.T) {
	var took tookLaunches
	g := newRig(t, nil, withWorkspaceRead(oneCommitRead))
	loc := g.spawn(workerSpec(t))
	r2 := restartedOver(t, g, loc, func(o *Options) {
		withBrokerAndOverlay(o)
		o.LaunchTook = took.record
	})
	g.connectRunning(otherToken, 1)
	if drifted := driftOf(t, g, r2, loc); len(drifted) == 0 {
		t.Fatalf("the pod has drifted from nothing, so this case proves nothing")
	}
	forgetPodContainers(t, g, r2, loc.Sandbox.Name)
	fresh := resumeTester(t, g, r2, loc)
	if fresh.Sandbox.PodUID == loc.Sandbox.PodUID {
		t.Fatalf("Resume kept the pod %s, whose stored template says nothing of what it was built with", loc.Sandbox.PodUID)
	}
	took.want(t, readTaken(workerToken, loc.Sandbox.PodUID, runtime.PodAct(loc.Sandbox.PodUID)), "stored template", "")
}

// The reviewer's start replaces the pod the tester runs in, because the reviewer's launcher Secret
// binds another pod (launcherBound). The tester's launcher, connected and authenticated for that
// pod, can still read its workspace, so it does, and the daemon is handed the commit it found: one
// launcher bound wrongly is no word that no process of the pod can read (GPT-6.1 Sol's point 4 on
// #5285 at 0259f8af, the Legion reviewer's point 4).
func TestALaunchIntoAPodWithUnboundLaunchersReadsThroughALauncherThatCan(t *testing.T) {
	var took tookLaunches
	g := newRig(t, nil, withWorkspaceRead(oneCommitRead), withOptions(func(o *Options) { o.LaunchTook = took.record }))
	testerLoc := g.spawn(workerSpec(t))
	editSecret(g, roleSecretName(testerLoc.Sandbox.Name, claim.RoleReviewer), func(s *corev1.Secret) {
		s.Annotations[launcherPodUIDAnnotation] = "uid-another-pod"
	})
	reviewerLoc := g.spawn(testSpec(t, otherToken, claim.RoleReviewer, testTree))
	if reviewerLoc.Sandbox.PodUID == testerLoc.Sandbox.PodUID {
		t.Fatalf("the reviewer's start kept the pod %s, whose reviewer launcher binds another pod", testerLoc.Sandbox.PodUID)
	}
	took.want(t, readTaken(otherToken, testerLoc.Sandbox.PodUID, runtime.PodAct(testerLoc.Sandbox.PodUID)), "not bound", "")
}

// The control: the same replacement where no launcher of the pod says it reads a workspace. No
// read is sent, and the daemon is handed that no read could be made, because no launcher of the
// pod can make it.
func TestALaunchIntoAPodWithUnboundLaunchersAndNoReaderSaysNoReadCouldBeMade(t *testing.T) {
	var took tookLaunches
	g := newRig(t, nil, withOptions(func(o *Options) { o.LaunchTook = took.record }))
	testerLoc := g.spawn(workerSpec(t))
	editSecret(g, roleSecretName(testerLoc.Sandbox.Name, claim.RoleReviewer), func(s *corev1.Secret) {
		s.Annotations[launcherPodUIDAnnotation] = "uid-another-pod"
	})
	reviewerLoc := g.spawn(testSpec(t, otherToken, claim.RoleReviewer, testTree))
	if reviewerLoc.Sandbox.PodUID == testerLoc.Sandbox.PodUID {
		t.Fatalf("the reviewer's start kept the pod %s, whose reviewer launcher binds another pod", testerLoc.Sandbox.PodUID)
	}
	pod := testerLoc.Sandbox.PodUID
	took.want(t, runtime.TakenPod{Claim: otherToken, Issue: testTree, PodUID: pod, Act: runtime.PodAct(pod), Work: runtime.WorkspaceWork{PodUID: pod}},
		"not bound", "no launcher")
	noReadSent(t, g, workerToken, otherToken)
}

// suspendedKept is a rig whose issue Sandbox keeps a volume, Suspended by a round's ReplacePod over
// the pod that loc records, with that pod gone. failInit has that pod show a failed workspace-init
// first, so the suspension writes the reset mark (volumeFault).
func suspendedKept(t *testing.T, failInit bool, options ...rigOption) (*rig, runtime.Locator) {
	t.Helper()
	g := newRig(t, nil, append([]rigOption{withOptions(keptVolumes(SandboxMicroVM))}, options...)...)
	loc := g.spawn(workerSpec(t))
	if failInit {
		g.editPod(loc.Sandbox.Name, "the pod store to see the failed workspace-init", func(p *corev1.Pod) {
			p.Status = corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{
				{Name: fetchContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}}},
				{Name: initContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}},
			}}
		})
	}
	if err := g.r.ReplacePod(g.ctx, runtime.Known{Claim: workerToken, Locator: &loc}, stillDue); err != nil {
		t.Fatalf("ReplacePod: %v", err)
	}
	g.eventually("the replaced pod to go", func() bool { return g.r.storedPod(loc.Sandbox.Name) == nil })
	if s := g.sandbox(loc.Sandbox.Name); s == nil || s.mode() != modeSuspended || len(s.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("after ReplacePod the issue's Sandbox is %+v, want it Suspended with its kept volume", s)
	}
	return g, loc
}

// relaunchKept is the tester's Resume from loc through r, as the death path's relaunch makes it.
func relaunchKept(g *rig, r *Runtime, loc runtime.Locator) (runtime.Locator, error) {
	spec := workerSpec(g.t)
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	g.launcher(spec.Claim)
	return r.Resume(g.ctx, &loc, spec)
}

// volumeTaken is the launch's replacement that deletes the issue's Sandbox of uid sandbox and the
// kept volume it owns (runtime.VolumeAct), naming no pod, since the Sandbox ran none of its own
// (the Legion reviewer's minor point 4), with no read made.
func volumeTaken(sandbox types.UID) runtime.TakenPod {
	return runtime.TakenPod{Claim: workerToken, Issue: testTree, Act: runtime.VolumeAct(string(sandbox))}
}

// A launch that deletes a kept volume its issue's Sandbox holds with no pod of its own (GPT-6.1
// Sol's point 3 on #5285 at 0259f8af, the Legion reviewer's point 5): the round's ReplacePod left
// the Sandbox Suspended with its volume, and the next launch replaces the Sandbox, and the volume
// with it, because the launch renders another disk shape or because the volume carries the reset
// mark. No process runs to read the volume, so the daemon is handed that no read could be made, and
// an act that takes the volume, whose target is the Sandbox's uid (GPT-6.1 Sol's point 4 on #5285
// at fa9deb04). It names no pod: the Sandbox's record of a displaced pod may be of a pod of the
// Sandbox it replaced, which never used this volume (the Legion reviewer's minor point 4). Each
// launch is held to completing, and the Sandbox to being a new one (the Legion reviewer's minor
// point 6, GPT-6.1 Sol's non-blocking point on fa9deb04). The control is a launch that keeps the
// volume: it completes in the same Sandbox, deletes no disk and is handed nothing.
func TestALaunchThatDeletesAKeptVolumeWithNoPodSaysWhatItTook(t *testing.T) {
	newSandbox := func(t *testing.T, g *rig, loc runtime.Locator, old types.UID, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if s := g.sandbox(loc.Sandbox.Name); s == nil || s.UID == old {
			t.Fatalf("after the launch the issue's Sandbox is %+v, want a new Sandbox in place of %s", s, old)
		}
	}
	t.Run("the launch keeps no volume now", func(t *testing.T) {
		var took tookLaunches
		g, loc := suspendedKept(t, false, withWorkspaceRead(oneCommitRead))
		old := g.sandbox(loc.Sandbox.Name).UID
		r2 := secondRuntime(t, g, func(o *Options) { o.LaunchTook = took.record })()
		_, err := relaunchKept(g, r2, loc)
		newSandbox(t, g, loc, old, err)
		took.want(t, volumeTaken(old), "the volume it keeps", "no live pod")
		noReadSent(t, g, workerToken)
	})
	t.Run("the volume carries the reset mark", func(t *testing.T) {
		var took tookLaunches
		g, loc := suspendedKept(t, true, withWorkspaceRead(oneCommitRead), withOptions(func(o *Options) { o.LaunchTook = took.record }))
		if mark := g.sandbox(loc.Sandbox.Name).Annotations[volumeResetAnnotation]; mark == "" {
			t.Fatal("the suspension wrote no reset mark, so this case proves nothing")
		}
		old := g.sandbox(loc.Sandbox.Name).UID
		_, err := relaunchKept(g, g.r, loc)
		newSandbox(t, g, loc, old, err)
		took.want(t, volumeTaken(old), "the volume it keeps", "no live pod")
		noReadSent(t, g, workerToken)
	})
	t.Run("control: the launch keeps the volume", func(t *testing.T) {
		var took tookLaunches
		g, loc := suspendedKept(t, false, withWorkspaceRead(oneCommitRead), withOptions(func(o *Options) { o.LaunchTook = took.record }))
		old := g.sandbox(loc.Sandbox.Name).UID
		if _, err := relaunchKept(g, g.r, loc); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if s := g.sandbox(loc.Sandbox.Name); s == nil || s.UID != old || len(s.Spec.VolumeClaimTemplates) != 1 {
			t.Fatalf("after the launch the issue's Sandbox is %+v, want Sandbox %s with its kept volume", s, old)
		}
		if taken := took.all(); len(taken) != 0 {
			t.Fatalf("the daemon was handed %+v, want nothing: the launch keeps the volume and its Sandbox", taken)
		}
	})
}

// GPT-6.1 Sol's point 4 on #5285 at fa9deb04 (the Legion reviewer's point B): an act that takes a
// kept volume (runtime.TakesVolume) is made only once the Sandbox of its target uid is gone. The
// launch above that resets the volume records its act (Options.LaunchTakes) before it deletes the
// Sandbox. Where it stops before the delete, as a daemon that stops there, and where the API server
// refuses the delete, the Sandbox stands Suspended with its volume, and the act reads uncertain: the
// suspension proves only that the pod was taken. Once the launch deletes the Sandbox, the act reads
// made.
func TestARecordedVolumeDeleteIsMadeOnlyOnceItsSandboxIsGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		// stopAtRecord stops the launch at its record's hook (Options.LaunchTakes), as a daemon
		// that stops there; refuseDelete has the API server refuse the Sandbox's delete.
		stopAtRecord bool
		refuseDelete bool
		want         runtime.ActState
	}{
		{name: "the launch stops before it deletes the Sandbox", stopAtRecord: true, want: runtime.ActUncertain},
		{name: "the API server refuses the Sandbox's delete", refuseDelete: true, want: runtime.ActUncertain},
		{name: "the launch deletes the Sandbox", want: runtime.ActMade},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var takes tookLaunches
			record := func(ctx context.Context, taken runtime.TakenPod) error {
				takes.record(ctx, taken)
				if tc.stopAtRecord {
					return errors.New("the daemon stops")
				}
				return nil
			}
			g, loc := suspendedKept(t, true, withOptions(func(o *Options) { o.LaunchTakes = record }))
			old := g.sandbox(loc.Sandbox.Name).UID
			if tc.refuseDelete {
				g.dyn.PrependReactor("delete", "sandboxes", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
					return true, nil, errors.New("the api server refuses the delete")
				})
			}
			_, err := relaunchKept(g, g.r, loc)
			if stopped := tc.stopAtRecord || tc.refuseDelete; stopped != (err != nil) {
				t.Fatalf("Resume = %v, want it to fail=%t", err, stopped)
			}
			takes.want(t, volumeTaken(old), "the volume it keeps", "no live pod")
			state, decided := runtime.ActUncertain, false
			if err := g.r.ReadAct(g.ctx, testTree, takes.all()[0].Act,
				func(_ context.Context, read runtime.ActState) error { state, decided = read, true; return nil }); err != nil || !decided {
				t.Fatalf("ReadAct = %v, decided=%t; want the act read", err, decided)
			}
			if state != tc.want {
				t.Fatalf("the act that deletes Sandbox %s reads %d, want %d", old, state, tc.want)
			}
		})
	}
}

// The death path's Resume over a Failed issue pod its Sandbox still runs: the pod is not live, so
// no process in it can read the workspace, and the daemon is handed that no read could be made.
func TestALaunchOverAPodThatIsNotLiveSaysNoReadCouldBeMade(t *testing.T) {
	var took tookLaunches
	name := SandboxName(workerToken)
	labels := claimLabels(claim.RoleTester)
	oldSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName(name), Namespace: testNamespace, OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: name, UID: "uid-sandbox-kept",
		}}},
		Data: map[string][]byte{provisionTokenKey: []byte("ghs_old")},
	}
	g := newRig(t, []k8sruntime.Object{
		sandboxObject(t, name, "uid-sandbox-kept", modeRunning, labels),
		podObject(name, "uid-pod-dead", "uid-sandbox-kept", labels, corev1.PodStatus{
			Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{terminated(workerContainer, 137, "Error")},
		}),
		oldSecret,
	}, withWorkspaceRead(oneCommitRead), withOptions(func(o *Options) { o.LaunchTook = took.record }))
	g.suspendDelay.Store(int64(50 * time.Millisecond))
	g.launcher(workerToken)
	spec := workerSpec(t)
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-g2", resumeSession
	dead := sandboxLocator(workerToken, "uid-pod-dead")
	loc, err := g.r.Resume(g.ctx, &dead, spec)
	if err != nil {
		t.Fatal(err)
	}
	if loc.Sandbox.PodUID == "uid-pod-dead" {
		t.Fatalf("the Resume kept the failed pod uid-pod-dead")
	}
	took.want(t, runtime.TakenPod{Claim: workerToken, Issue: testTree, PodUID: "uid-pod-dead", Act: runtime.PodAct("uid-pod-dead")}, "not live", "not live")
	noReadSent(t, g, workerToken)
}
