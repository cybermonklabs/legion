package sandbox

import (
	"context"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var _ runtime.InPlaceRelauncher = (*Runtime)(nil)

// RelaunchRefusal satisfies runtime.InPlaceRelauncher: why a relaunch of loc's process on its same
// session (R5) cannot stop it and start the next generation through the same launcher in the same
// pod. Without mayReplacePod it refuses a role whose launcher for loc's pod is not connected
// (runtime.ErrLauncherNotConnected), since a start through it would wait out the launch bound and
// be charged, and a launch that would replace the pod (runtime.ErrRelaunchReplacesPod), since that
// ends every other role running in it, naming why. The pod is judged by relaunch's own decision
// (replacement), over the launch the claim's next generation makes: the pod's kind is the claim's
// role's (podKindFor), an issue pod runs in the sandbox the configuration now gives the issue its
// Sandbox is labelled with (SandboxConfig.ofIssueLabel, the rule storedShapeKey keeps), as prepare
// decides it for the launch, and the launch installs no plugin pair. An installing launch keeps a
// pod the check would replace only where no round covers the pod, which is the controller's, whose
// relaunch is the daemon's own and may replace it.
//
// The check guards the other roles running in the pod, and it keeps one decision of the launch out
// of its reading: the update round over the pod (Options.RoundPodEnds), which the daemon records as
// the act of the launch that asked, so the check asks nothing of it (replacement's rounds). Where
// the claim has not answered the round and the round does not pass it over, the daemon holds the
// pod from the relaunch's stop until its launch starts, at most until the round's bound
// (internal/daemon's roundPodEnds, relaunchesInPlace), against the launch's own reading and the
// sweep's replacement by issue alike, so the claim comes back where its unpushed work is. Without
// that hold, and with nothing else holding the pod, the launch replaces a pod the round is done
// with as the round's own replacement. A check that passed is thus no promise that the next
// generation starts in the same pod, only that no other role running in it is ended for this
// relaunch.
//
// With mayReplacePod nothing refuses: the daemon's own relaunch of its controller replaces a pod in
// which no other role runs, so it reads nothing. It stops, starts and writes nothing; the launcher
// Secrets it reads are held as their roles' credentials, as any launch's read holds them
// (launcherBound).
func (r *Runtime) RelaunchRefusal(ctx context.Context, loc runtime.Locator, mayReplacePod bool) error {
	if err := r.checkLocator(loc); err != nil {
		return err
	}
	if mayReplacePod {
		return nil
	}
	if _, connected := r.launchers.state(loc.Claim, loc.Sandbox.PodUID); !connected {
		return fmt.Errorf("relaunch %s: no launcher of it in pod %s: %w", loc.Claim, loc.Sandbox.PodUID, runtime.ErrLauncherNotConnected)
	}
	role, ok := loc.Claim.Role()
	if !ok {
		return fmt.Errorf("relaunch %s: the claim names no role", loc.Claim)
	}
	s, err := r.storedSandbox(loc.Sandbox.Name)
	if err != nil {
		return fmt.Errorf("relaunch %s: %w", loc.Claim, err)
	}
	if s == nil {
		return fmt.Errorf("relaunch %s: its Sandbox %s is gone: %w", loc.Claim, loc.Sandbox.Name, runtime.ErrRelaunchReplacesPod)
	}
	l := r.relaunchLaunch(s, loc.Claim, role)
	decided, err := r.replacement(ctx, s, r.storedPod(s.Name), &l, nil)
	if err != nil {
		return fmt.Errorf("relaunch %s: %w", loc.Claim, err)
	}
	if decided.why != "" {
		return fmt.Errorf("relaunch %s: %s: %w", loc.Claim, decided.why, runtime.ErrRelaunchReplacesPod)
	}
	return nil
}

// relaunchLaunch is the launch the next generation of token, a claim of role, makes into the
// Sandbox read as s, as far as relaunch's decision (replacement) reads it: the claim, its role and
// the pod kind of that role (podKindFor), and for an issue pod the microVM block where the
// configuration now runs the issue its Sandbox is labelled with in a microVM
// (SandboxConfig.ofIssueLabel, the rule storedShapeKey keeps), as prepare decides it. The disk the
// launch renders (supersededDisk) and the shape it compares the pod with (shapeKey) both read that
// block, so a launch without it would judge a microVM pod against a gVisor one's disk and shape.
// It installs no plugin pair.
func (r *Runtime) relaunchLaunch(s *sandbox, token claim.Token, role claim.Role) launch {
	kind := podKindFor(role)
	l := launch{spec: runtime.SpawnSpec{Claim: token, Role: role}, name: s.Name, kind: kind, roles: kind.roles()}
	if _, issue := kind.(issuePod); issue && r.sandboxConfig.ofIssueLabel(s.Labels[labelIssue]) == SandboxMicroVM {
		l.microVM = r.sandboxConfig.MicroVM
	}
	return l
}

// StopForRelaunch satisfies runtime.InPlaceRelauncher: it stops loc's process for its relaunch on
// its same session. Through the role's connected launcher it stops loc's generation as Suspend
// does, the same frame within the same bounds, and stops nothing where the stores or the launcher
// show that generation already ended (mayRun). A stop that fails or is answered other than OK is
// runtime.ErrProcessOutlivedStop, carrying the launcher's answer: the process may run on, the
// claim stays watched and the launcher is left as it is, so nothing is started beside it.
//
// With the launcher not connected, mayReplacePod ends the pod instead (endPod), and without it the
// stop is refused with runtime.ErrLauncherNotConnected, stopping nothing.
func (r *Runtime) StopForRelaunch(ctx context.Context, loc runtime.Locator, mayReplacePod bool) error {
	if err := r.checkLocator(loc); err != nil {
		return err
	}
	if _, connected := r.launchers.state(loc.Claim, loc.Sandbox.PodUID); !connected {
		if !mayReplacePod {
			return fmt.Errorf("stop %s for its relaunch: no launcher of it in pod %s: %w", loc.Claim, loc.Sandbox.PodUID, runtime.ErrLauncherNotConnected)
		}
		return r.endPod(ctx, loc)
	}
	runs, err := r.mayRun(loc)
	if err != nil {
		return fmt.Errorf("stop %s for its relaunch: %w", loc.Claim, err)
	}
	if runs {
		if err := r.stopGeneration(ctx, loc); err != nil {
			return fmt.Errorf("stop %s for its relaunch: %w: %w", loc.Claim, runtime.ErrProcessOutlivedStop, err)
		}
	}
	r.forgetIf(loc)
	return nil
}

// endPod ends the pod loc's process runs in, whose launcher is not connected, for the daemon's own
// relaunch of the one role that pod runs, its controller: a stop no launcher can carry. It does
// what a launch replacing the pod does, under the pod's launch turn (lockPod), so no launch of it
// runs in between: it suspends the Sandbox (suspendReplaced), naming the pod as displaced where
// its controller may run in it (runsNoRole), and waits the pod out (awaitPodGone), so the pod's
// end is read as the daemon's own act and not a death. The Resume that follows finds the Sandbox
// Suspended and makes a new pod; a pod its node never started ran no role, so its suspension
// names nothing displaced, as relaunch's does. A pod that is not loc's, or has ended, holds no
// process of loc's, and nothing is done. A suspension the API server refused ended nothing, and
// is returned as it is; a pod still there when the wait ends is runtime.ErrProcessOutlivedStop,
// carrying what the cluster answered, and the claim stays watched.
func (r *Runtime) endPod(ctx context.Context, loc runtime.Locator) error {
	fail := func(step string, err error) error {
		return fmt.Errorf("stop %s for its relaunch: %s: %w", loc.Claim, step, err)
	}
	release, err := r.lockPod(ctx, loc.Sandbox.Name)
	if err != nil {
		return fail("take its pod's launch turn", err)
	}
	defer release()
	s, err := r.storedSandbox(loc.Sandbox.Name)
	if err != nil {
		return fail("read its sandbox", err)
	}
	if s == nil {
		r.forgetIf(loc)
		return nil
	}
	pod := r.storedPod(s.Name)
	if !ownedBy(pod, s.UID) || string(pod.UID) != loc.Sandbox.PodUID || terminal(pod) {
		r.forgetIf(loc)
		return nil
	}
	idle, _, _ := r.runsNoRole(s, pod, r.now())
	suspended, step, err := r.suspendReplaced(ctx, s, pod, idle == "", launchReplacement)
	if err != nil {
		return fail(step, err)
	}
	s = suspended
	if _, err := r.awaitPodGone(ctx, s); err != nil {
		return fmt.Errorf("stop %s for its relaunch: %w: %w; %s", loc.Claim, runtime.ErrProcessOutlivedStop, err, r.waitedOut(ctx, s))
	}
	r.forgetIf(loc)
	return nil
}
