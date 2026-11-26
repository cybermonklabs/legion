package daemon

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// launchRounds is the seam between the Kubernetes runtime, which openSupervision builds before the
// supervision exists, and three hooks of the supervision's that a launch calls under the pod's
// launch turn: the reading of the update round over a pod a launch would keep
// (supervision.roundPodEndsAtLaunch), the record of what a launch's own
// replacement of an issue pod is about to take (supervision.recordLaunchTakes), and the telling of
// what it took (supervision.tellLaunchTook, R7 of #5285). prepareSandbox makes one
// and hands its ends, its takes and its took to the runtime (sandbox.Options.RoundPodEnds,
// LaunchTakes, LaunchTook), and the boot fills it the moment the supervision is open
// (plan.launchRounds, daemon.run).
//
// It is filled once, in the boot's own goroutine, before any claim's machine exists and so before
// any launch can ask: there is nothing to lock, and nothing ever replaces what was filled.
type launchRounds struct {
	read   runtime.RoundPodEnds
	record runtime.LaunchTakes
	tell   runtime.LaunchTook
}

// bind gives the holder the supervision's reading of the round, its record of what a launch is
// about to take, and its telling of what a launch took.
func (l *launchRounds) bind(read runtime.RoundPodEnds, record runtime.LaunchTakes, tell runtime.LaunchTook) {
	l.read, l.record, l.tell = read, record, tell
}

// ends is the runtime's RoundPodEnds: the supervision's reading of the round over podUID. A holder
// nothing filled keeps the pod, as a runtime handed no reading keeps it; only a boot that skipped
// plan.launchRounds.bind leaves it so, and the pod's round then replaces the pod as it did before
// the reading existed.
func (l *launchRounds) ends(ctx context.Context, token claim.Token, podUID string) (bool, error) {
	if l.read == nil {
		return false, nil
	}
	return l.read(ctx, token, podUID)
}

// takes is the runtime's LaunchTakes: the supervision's record of what a launch's own replacement
// is about to take. A holder nothing filled records nothing and refuses nothing.
func (l *launchRounds) takes(ctx context.Context, taken runtime.TakenPod) error {
	if l.record == nil {
		return nil
	}
	return l.record(ctx, taken)
}

// took is the runtime's LaunchTook: the supervision's telling of what a launch's own replacement
// took. A holder nothing filled tells nobody, as a runtime handed no LaunchTook tells nobody.
func (l *launchRounds) took(ctx context.Context, taken runtime.TakenPod) {
	if l.tell != nil {
		l.tell(ctx, taken)
	}
}
