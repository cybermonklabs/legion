package sandbox

import (
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// A relaunch of an unreachable claim on its same session (R5) whose role's launcher is not
// connected is refused before anything is stopped: a start through a launcher that does not connect
// would wait out the launch bound and be charged, and the stop before it would have ended the
// process for nothing. So neither the check nor the stop sends a frame to any launcher of the pod
// or writes the Sandbox, and the pod and the sibling role in it run on.
func TestARelaunchWhoseLauncherIsNotConnectedIsRefusedBeforeAnythingStops(t *testing.T) {
	g := newRig(t, nil)
	tester := g.spawn(workerSpec(t))
	architect := g.spawn(rootSpec(t))
	g.disconnect(workerToken)
	before := g.frameCounts(workerToken, rootToken)
	g.clearActions()

	if err := g.r.RelaunchRefusal(g.ctx, tester, false); !errors.Is(err, runtime.ErrLauncherNotConnected) {
		t.Fatalf("RelaunchRefusal = %v, want %v", err, runtime.ErrLauncherNotConnected)
	}
	if err := g.r.StopForRelaunch(g.ctx, tester, false); !errors.Is(err, runtime.ErrLauncherNotConnected) {
		t.Fatalf("StopForRelaunch = %v, want %v and nothing stopped", err, runtime.ErrLauncherNotConnected)
	}
	g.expectNothingStopped(tester, before)
	g.expectRunning(rootToken, architect.Sandbox.PodUID, architect.Sandbox.Generation)
	if s := g.sandbox(tester.Sandbox.Name); s == nil || s.mode() != modeRunning {
		t.Fatalf("the issue's Sandbox after the refusal = %+v, want it Running as it was", s)
	}
}

// A relaunch whose launch would replace the pod is refused before anything is stopped, naming why:
// replacing an issue pod ends every other role running in it. The check reads the pod as the launch
// itself would (replacement), so the launch that follows a refusal anyway does replace the pod:
// the check and the launch never disagree.
func TestARelaunchThatWouldReplaceThePodIsRefusedBeforeAnythingStops(t *testing.T) {
	for name, tc := range map[string]struct {
		// rig is the issue's pod with the tester and the architect running generation 1 in it,
		// through connected launchers, and the tester's recorded locator.
		rig func(t *testing.T) (*rig, runtime.Locator)
		why string
	}{
		"a launcher Secret of the pod no longer binds it": {
			rig: func(t *testing.T) (*rig, runtime.Locator) {
				g := newRig(t, nil)
				tester := g.spawn(workerSpec(t))
				g.spawn(rootSpec(t))
				if err := g.kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("secrets"), testNamespace, roleSecretName(tester.Sandbox.Name, claim.RoleReviewer)); err != nil {
					t.Fatal(err)
				}
				return g, tester
			},
			why: "launcher Secret",
		},
		"its Sandbox is of the release before the pod-owned disk": {
			rig: func(t *testing.T) (*rig, runtime.Locator) {
				g := supersededIssue(t)
				for _, token := range []claim.Token{workerToken, rootToken} {
					g.connectRunning(token, 1)
					g.launcher(token)
				}
				g.eventually("both launchers to report their child", func() bool {
					tester, testerConnected := g.r.launchers.state(workerToken, "uid-pod-old")
					architect, architectConnected := g.r.launchers.state(rootToken, "uid-pod-old")
					return testerConnected && tester.Child != nil && architectConnected && architect.Child != nil
				})
				return g, sandboxLocator(workerToken, "uid-pod-old")
			},
			why: string(replacedFromPreviousRelease),
		},
	} {
		t.Run(name, func(t *testing.T) {
			g, tester := tc.rig(t)
			before := g.frameCounts(workerToken, rootToken)
			g.clearActions()

			err := g.r.RelaunchRefusal(g.ctx, tester, false)
			if !errors.Is(err, runtime.ErrRelaunchReplacesPod) || !strings.Contains(err.Error(), tc.why) {
				t.Fatalf("RelaunchRefusal = %v, want %v naming %q", err, runtime.ErrRelaunchReplacesPod, tc.why)
			}
			g.expectNothingStopped(tester, before)
			g.expectRunning(workerToken, tester.Sandbox.PodUID, 1)
			g.expectRunning(rootToken, tester.Sandbox.PodUID, 1)

			spec := workerSpec(t)
			spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-g2", resumeSession
			next, err := g.r.Resume(g.ctx, &tester, spec)
			if err != nil {
				t.Fatalf("Resume the tester: %v", err)
			}
			if next.Sandbox.PodUID == tester.Sandbox.PodUID {
				t.Fatalf("the launch kept pod %s, which the check said it would replace", tester.Sandbox.PodUID)
			}
		})
	}
}

// A stop through the role's launcher that the process outlives — the launcher answers it OK false,
// its child still running — refuses the relaunch after that one stop and does nothing more: no
// second frame to the launcher, none to a sibling, no write of the Sandbox, the launcher left
// connected and running its child, and the claim still watched, since its process runs on.
// Starting the next generation beside it would leave two processes writing one session.
func TestAStopTheProcessOutlivesRefusesTheRelaunchAndDoesNothingMore(t *testing.T) {
	const answer = "signal role tester: operation not permitted"
	g := newRig(t, nil, withOutlivedStop(answer))
	tester := g.spawn(workerSpec(t))
	architect := g.spawn(rootSpec(t))
	before := g.frameCounts(workerToken, rootToken)
	g.clearActions()

	if err := g.r.RelaunchRefusal(g.ctx, tester, false); err != nil {
		t.Fatalf("RelaunchRefusal = %v, want none: the launcher is connected and the pod kept", err)
	}
	err := g.r.StopForRelaunch(g.ctx, tester, false)
	if !errors.Is(err, runtime.ErrProcessOutlivedStop) || !strings.Contains(err.Error(), answer) {
		t.Fatalf("StopForRelaunch = %v, want %v carrying the launcher's answer %q", err, runtime.ErrProcessOutlivedStop, answer)
	}
	expectOneStop(t, g.framesSince(workerToken, before), tester.Sandbox.Generation)
	if frames := g.framesSince(rootToken, before); len(frames) > 0 {
		t.Errorf("the architect's launcher was sent %+v", frames)
	}
	if writes := g.podWrites(); len(writes) > 0 {
		t.Errorf("the runtime wrote %v after a stop the process outlived", writes)
	}
	g.expectRunning(workerToken, tester.Sandbox.PodUID, tester.Sandbox.Generation)
	g.expectRunning(rootToken, architect.Sandbox.PodUID, architect.Sandbox.Generation)
	if recorded, found := g.r.recorded(workerToken); !found || recorded != tester {
		t.Errorf("the runtime records %+v (%t) for the tester, want its locator still watched: its process runs on", recorded, found)
	}
}

// A relaunch through a connected launcher into a pod the launch keeps stops only that role's
// generation, through its launcher, and the claim's next generation then resumes in the same pod:
// the pod is never suspended, and the sibling's launcher gets no frame and runs its child on.
func TestARelaunchThroughAConnectedLauncherStopsAndStartsOnlyThatRole(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	tester := g.spawn(spec)
	architect := g.spawn(rootSpec(t))
	before := g.frameCounts(workerToken, rootToken)
	g.clearActions()

	if err := g.r.RelaunchRefusal(g.ctx, tester, false); err != nil {
		t.Fatalf("RelaunchRefusal = %v, want none", err)
	}
	if err := g.r.StopForRelaunch(g.ctx, tester, false); err != nil {
		t.Fatalf("StopForRelaunch = %v", err)
	}
	expectOneStop(t, g.framesSince(workerToken, before), tester.Sandbox.Generation)
	if state, connected := g.r.launchers.state(workerToken, tester.Sandbox.PodUID); !connected || state.Child != nil {
		t.Fatalf("the tester's launcher after the stop = connected %t, %+v; want it connected with no child", connected, state)
	}

	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-g2", resumeSession
	next, err := g.r.Resume(g.ctx, &tester, spec)
	if err != nil {
		t.Fatalf("Resume the tester: %v", err)
	}
	if next.Sandbox.PodUID != tester.Sandbox.PodUID {
		t.Fatalf("the relaunch moved the tester from pod %s to %s, want the same pod", tester.Sandbox.PodUID, next.Sandbox.PodUID)
	}
	frames := g.framesSince(workerToken, before)
	if len(frames) != 2 {
		t.Fatalf("the tester's launcher was sent %+v, want the stop of generation 1 and the start of generation 2", frames)
	}
	if start, ok := frames[1].(shimwire.LauncherStart); !ok || start.Generation != 2 || start.ResumeFile != resumeSession {
		t.Fatalf("the tester's second frame = %+v, want generation 2 resuming %s", frames[1], resumeSession)
	}
	expectSteps(t, steps(t, g.writes(), tester.Sandbox.Name), "record addresses")
	if frames := g.framesSince(rootToken, before); len(frames) > 0 {
		t.Errorf("the architect's launcher was sent %+v", frames)
	}
	g.expectRunning(rootToken, architect.Sandbox.PodUID, architect.Sandbox.Generation)
}

// The check judges a microVM issue's pod by the launch the issue's next generation makes, with the
// microVM block its issue now runs in (relaunchLaunch, as prepare decides it). So it reads the disk
// a kept volume's Sandbox owns as the one that launch renders, and the pod's shape record as that
// launch's shape, as the launch itself does. Built as a gVisor launch, it read the kept volume as
// one to replace ("the launch now keeps no volume") and the pod's shape as drifted. The two then
// agreed on this pod only because that drift turned off the disk's rule. They part where the shape
// does not drift: a Sandbox of the release before the pod-owned disk, whose live pod was built
// under gVisor before its issue moved to a microVM, which the launch keeps for its round to end.
func TestTheRelaunchCheckReadsAMicroVMPodAsItsLaunchDoes(t *testing.T) {
	g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)))
	tester := g.spawn(workerSpec(t))
	s := g.sandbox(tester.Sandbox.Name)
	if !ownsKeptVolume(s) {
		t.Fatalf("the Sandbox owns %+v, want the kept `issue` template, or this case proves nothing", s.Spec.VolumeClaimTemplates)
	}
	l := g.r.relaunchLaunch(s, workerToken, claim.RoleTester)
	if why := g.r.supersededDisk(s, l); why != "" {
		t.Errorf("the check's launch reads the Sandbox's disk as %q, want the one it renders", why)
	}
	if drifted := g.r.shapeDrift(s, l.shapeKey()); len(drifted) > 0 {
		t.Errorf("the check's launch reads the pod's shape as drifted: %v, want none", drifted)
	}
	if err := g.r.RelaunchRefusal(g.ctx, tester, false); err != nil {
		t.Fatalf("RelaunchRefusal = %v, want none: the launch keeps the pod", err)
	}
}

// The check asks the daemon nothing of the update round over the pod (Options.RoundPodEnds): the
// daemon records each answer as the act of the launch that asked, so only the launch that follows
// the stop asks, under the pod's launch turn. A round the daemon reads as over refuses no relaunch,
// since no role process runs in such a pod and its replacement ends none of the roles the check
// guards; the launch then makes that replacement as the round's own, naming the pod under the
// round's mark, and the claim's next generation starts in the new pod.
func TestTheRelaunchCheckAsksNothingOfTheUpdateRound(t *testing.T) {
	var asked []roundAsk
	g := newRig(t, nil, withRoundPodEnds(true, nil, &asked))
	spec := workerSpec(t)
	tester := g.spawn(spec)
	name, old := tester.Sandbox.Name, tester.Sandbox.PodUID
	asked = nil
	if err := g.r.RelaunchRefusal(g.ctx, tester, false); err != nil {
		t.Fatalf("RelaunchRefusal = %v, want none: the check asks nothing of the round", err)
	}
	if len(asked) != 0 {
		t.Fatalf("the check asked the daemon %+v, want nothing", asked)
	}
	if err := g.r.StopForRelaunch(g.ctx, tester, false); err != nil {
		t.Fatalf("StopForRelaunch = %v", err)
	}
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-g2", resumeSession
	next, err := g.r.Resume(g.ctx, &tester, spec)
	if err != nil {
		t.Fatalf("Resume the tester: %v", err)
	}
	if want := []roundAsk{{token: workerToken, pod: old}}; !slices.Equal(asked, want) {
		t.Errorf("the launch asked the daemon %+v, want %+v", asked, want)
	}
	if next.Sandbox.PodUID == old {
		t.Fatalf("the launch kept pod %s, whose round the daemon reads as over", old)
	}
	if s := g.sandbox(name); s.Annotations[roundDisplacedAnnotation] != old || s.Annotations[displacedAnnotation] != old {
		t.Errorf("the Sandbox names %q replaced by its round and %q displaced, want %s for both", s.Annotations[roundDisplacedAnnotation], s.Annotations[displacedAnnotation], old)
	}
}

// The daemon's own relaunch of its controller (mayReplacePod) is never refused for the pod: no
// other role runs in the controller's pod, so a launcher that is not connected and a launch that
// needs a new pod are both answered by a new pod. Where its launcher is not connected, the stop
// ends the pod as a launch's replacement does — the Sandbox suspended, naming the pod it replaces
// — and the Resume that follows brings a new pod. Where the launch needs a new pod, the stop goes
// through the launcher and the Resume replaces the pod. The same checks without mayReplacePod
// refuse, so the answer is the pod's to give and not a check that reads nothing.
func TestTheDaemonsRelaunchOfItsControllerReplacesItsPodWhereItMust(t *testing.T) {
	t.Run("its launcher is not connected", func(t *testing.T) {
		g := newRig(t, nil)
		spec := controllerSpec(t)
		loc := g.spawn(spec)
		g.disconnect(controllerToken)

		if err := g.r.RelaunchRefusal(g.ctx, loc, false); !errors.Is(err, runtime.ErrLauncherNotConnected) {
			t.Fatalf("RelaunchRefusal without mayReplacePod = %v, want %v", err, runtime.ErrLauncherNotConnected)
		}
		if err := g.r.RelaunchRefusal(g.ctx, loc, true); err != nil {
			t.Fatalf("RelaunchRefusal = %v, want none", err)
		}
		if err := g.r.StopForRelaunch(g.ctx, loc, true); err != nil {
			t.Fatalf("StopForRelaunch = %v", err)
		}
		s := g.sandbox(loc.Sandbox.Name)
		if s == nil || s.mode() != modeSuspended || s.Annotations[displacedAnnotation] != loc.Sandbox.PodUID {
			t.Fatalf("the controller's Sandbox after the stop = %+v, want it Suspended naming pod %s as displaced", s, loc.Sandbox.PodUID)
		}
		if pod := g.pod(loc.Sandbox.Name); pod != nil && string(pod.UID) == loc.Sandbox.PodUID {
			t.Fatalf("the controller's pod %s outlived the stop", loc.Sandbox.PodUID)
		}

		spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-controller-g2", controllerSession
		next, err := g.r.Resume(g.ctx, &loc, spec)
		if err != nil {
			t.Fatalf("Resume the controller: %v", err)
		}
		if next.Sandbox.PodUID == loc.Sandbox.PodUID {
			t.Fatalf("the controller resumed in its ended pod %s", loc.Sandbox.PodUID)
		}
		if start := lastStart(t, g, controllerToken); start.Generation != 2 || start.ResumeFile != controllerSession {
			t.Fatalf("the controller's last start is generation %d resuming %q, want generation 2 resuming its session", start.Generation, start.ResumeFile)
		}
	})
	t.Run("its launch needs a new pod", func(t *testing.T) {
		g := newRig(t, nil)
		spec := controllerSpec(t)
		loc := g.spawn(spec)
		r2 := secondRuntime(t, g, servingAddedOwner)()
		g.connectRunning(controllerToken, loc.Sandbox.Generation)
		g.eventually("the controller's launcher to report its child to the restarted daemon", func() bool {
			state, connected := r2.launchers.state(controllerToken, loc.Sandbox.PodUID)
			return connected && state.Child != nil
		})
		moved := (movedAddress{roleVolume(ghVolume, claim.RoleController), ghDirsName(staticDirs), ghDirsName(servedDirs)}).String()

		if err := r2.RelaunchRefusal(g.ctx, loc, false); !errors.Is(err, runtime.ErrRelaunchReplacesPod) || !strings.Contains(err.Error(), moved) {
			t.Fatalf("RelaunchRefusal without mayReplacePod = %v, want %v naming %q", err, runtime.ErrRelaunchReplacesPod, moved)
		}
		if err := r2.RelaunchRefusal(g.ctx, loc, true); err != nil {
			t.Fatalf("RelaunchRefusal = %v, want none", err)
		}
		before := g.frameCounts(controllerToken)
		if err := r2.StopForRelaunch(g.ctx, loc, true); err != nil {
			t.Fatalf("StopForRelaunch = %v", err)
		}
		expectOneStop(t, g.framesSince(controllerToken, before), loc.Sandbox.Generation)

		spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-controller-g2", controllerSession
		next, err := r2.Resume(g.ctx, &loc, spec)
		if err != nil {
			t.Fatalf("Resume the controller: %v", err)
		}
		if next.Sandbox.PodUID == loc.Sandbox.PodUID {
			t.Fatalf("the launch kept pod %s, whose gh volume names no directory for %s", loc.Sandbox.PodUID, addedOwner)
		}
		if named := g.sandbox(loc.Sandbox.Name).Annotations[displacedAnnotation]; named != loc.Sandbox.PodUID {
			t.Fatalf("the controller's Sandbox names %q as the pod the daemon replaced, want %s", named, loc.Sandbox.PodUID)
		}
	})
}

// disconnect drops token's fake launcher connection, as a launcher whose connection to the daemon
// ended while its pod runs on: the runtime then holds no launcher of the role for that pod.
func (g *rig) disconnect(token claim.Token) {
	g.t.Helper()
	g.r.launchers.mu.Lock()
	session := g.r.launchers.sessions[token]
	g.r.launchers.mu.Unlock()
	if session == nil {
		g.t.Fatalf("%s has no launcher to disconnect", token)
	}
	session.close()
}

// frameCounts is how many commands each of tokens' fake launchers has received so far, for
// framesSince to read what came after.
func (g *rig) frameCounts(tokens ...claim.Token) map[claim.Token]int {
	counts := map[claim.Token]int{}
	for _, token := range tokens {
		counts[token] = len(g.sent(token))
	}
	return counts
}

// framesSince is every command token's fake launcher received after counts was taken.
func (g *rig) framesSince(token claim.Token, counts map[claim.Token]int) []shimwire.Frame {
	return g.sent(token)[counts[token]:]
}

// podWrites are the runtime's writes of a Sandbox or a pod since its actions were last cleared:
// a suspension, a mode change, an address record, a delete.
func (g *rig) podWrites() []action {
	return slices.DeleteFunc(g.writes(), func(a action) bool { return a.resource != "sandboxes" && a.resource != "pods" })
}

// expectNothingStopped fails the test unless, since counts was taken and the actions were last
// cleared, no command reached a launcher counts names, the runtime wrote no Sandbox and no pod, and
// loc's pod still runs.
func (g *rig) expectNothingStopped(loc runtime.Locator, counts map[claim.Token]int) {
	g.t.Helper()
	for token := range counts {
		if frames := g.framesSince(token, counts); len(frames) > 0 {
			g.t.Errorf("%s's launcher was sent %+v", token, frames)
		}
	}
	if writes := g.podWrites(); len(writes) > 0 {
		g.t.Errorf("the runtime wrote %v", writes)
	}
	if pod := g.pod(loc.Sandbox.Name); pod == nil || string(pod.UID) != loc.Sandbox.PodUID || pod.DeletionTimestamp != nil {
		g.t.Errorf("the pod after the refusal = %+v, want %s running on", pod, loc.Sandbox.PodUID)
	}
}

// expectRunning fails the test unless token's launcher in podUID is connected and runs generation.
func (g *rig) expectRunning(token claim.Token, podUID string, generation uint64) {
	g.t.Helper()
	if state, connected := g.r.launchers.state(token, podUID); !connected || state.Child == nil || state.Child.Generation != generation {
		g.t.Errorf("%s's launcher = connected %t, %+v; want it running generation %d", token, connected, state, generation)
	}
}

// expectOneStop fails the test unless frames is exactly one stop, of generation.
func expectOneStop(t *testing.T, frames []shimwire.Frame, generation uint64) {
	t.Helper()
	if len(frames) != 1 {
		t.Fatalf("the launcher was sent %+v, want one stop of generation %d", frames, generation)
	}
	if stop, ok := frames[0].(shimwire.LauncherStop); !ok || stop.Generation != generation {
		t.Fatalf("the launcher was sent %+v, want a stop of generation %d", frames[0], generation)
	}
}
