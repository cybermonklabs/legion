//go:build e2e

// The rig's pods (podRuntime, w5_rig_live_test.go) as a runtime whose processes are started
// through a launcher of their own in a shared pod, the Sandbox runtime's shape
// (runtime.InPlaceRelauncher, runtime/sandbox's inplace_relaunch.go): the seam check w5 of this
// change's verification names for the sixth claim's three runs and for the controller whose
// launcher is not connected. A claim's launcher is connected unless a step has the rig report
// otherwise (podRuntime.setLauncher):
//
//   - not connected: the relaunch is refused before anything is stopped, as the Sandbox runtime
//     refuses it, unless the relaunch may replace the pod (the daemon's own of its controller),
//     whose stop then ends the pod and whose next launch runs in a new one;
//   - replaces the pod: the relaunch is refused before anything is stopped;
//   - outlives the stop: the stop is answered and the process runs on, so the relaunch is refused
//     after the stop with runtime.ErrProcessOutlivedStop.
//
// Every call is recorded with whether the claim's process still ran when it was answered. This
// file names the head's own runtime symbols, so the negative control, which builds over the base,
// leaves it out; at the base podRuntime implements no InPlaceRelauncher, as the base's tmux
// runtime does not.
package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var _ runtime.InPlaceRelauncher = (*podRuntime)(nil)

// RelaunchRefusal answers the rig's report of loc's launcher, as the Sandbox runtime answers its
// own: nothing refuses a relaunch that may replace the pod.
func (p *podRuntime) RelaunchRefusal(_ context.Context, loc runtime.Locator, mayReplacePod bool) error {
	state := p.launcherOf(loc.Claim)
	var err error
	switch {
	case mayReplacePod:
	case state == w5LauncherNotConnected:
		err = fmt.Errorf("relaunch %s: no launcher of it in pod %s: %w", loc.Claim, podUIDOf(loc), runtime.ErrLauncherNotConnected)
	case state == w5LauncherReplacesPod:
		err = fmt.Errorf("relaunch %s: the rig reports that its launch needs a new pod: %w", loc.Claim, runtime.ErrRelaunchReplacesPod)
	}
	p.noteRelaunch(podRelaunchCall{at: time.Now(), call: "RelaunchRefusal", claim: loc.Claim, mayReplacePod: mayReplacePod,
		launcher: state, answer: answerOf(err), running: p.runs(loc)})
	return err
}

// StopForRelaunch stops loc's process through its launcher (the local runtime's Suspend), ends its
// pod where the launcher is not connected and the relaunch may replace the pod, refuses a stop
// through a launcher that is not connected, and answers a stop the process outlives with
// runtime.ErrProcessOutlivedStop, leaving the process running.
func (p *podRuntime) StopForRelaunch(ctx context.Context, loc runtime.Locator, mayReplacePod bool) error {
	state := p.launcherOf(loc.Claim)
	var err error
	switch {
	case state == w5LauncherNotConnected && !mayReplacePod:
		err = fmt.Errorf("stop %s for its relaunch: no launcher of it in pod %s: %w", loc.Claim, podUIDOf(loc), runtime.ErrLauncherNotConnected)
	case state == w5LauncherNotConnected:
		err = p.Suspend(ctx, loc)
		if err == nil {
			p.replacePod(p.podOf(loc.Claim))
			p.setLauncher(loc.Claim, "")
		}
	case state == w5LauncherOutlivesStop:
		err = fmt.Errorf("stop %s for its relaunch: the rig's launcher answered the stop and the process still runs: %w", loc.Claim, runtime.ErrProcessOutlivedStop)
	default:
		err = p.Suspend(ctx, loc)
	}
	p.noteRelaunch(podRelaunchCall{at: time.Now(), call: "StopForRelaunch", claim: loc.Claim, mayReplacePod: mayReplacePod,
		launcher: state, answer: answerOf(err), running: p.runs(loc)})
	return err
}

// runs is whether the local process loc stands for still runs.
func (p *podRuntime) runs(loc runtime.Locator) bool {
	launch := p.current(loc.Claim)
	if launch == nil {
		return false
	}
	return launch.running() && p.local(loc).Incarnation == launch.pane.locator.Incarnation
}

func podUIDOf(loc runtime.Locator) string {
	if loc.Sandbox == nil {
		return ""
	}
	return loc.Sandbox.PodUID
}

func answerOf(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}
