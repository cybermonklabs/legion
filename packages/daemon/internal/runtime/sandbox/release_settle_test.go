package sandbox

// Review round 13's first finding on cybermonklabs/legion#4049 (dispatch://INFRA-3583): the project
// release settles the standing records of a pod's replacements under the pod's launch turn that its
// delete holds (runtime.IssueSuspender's settle, internal/daemon's settleReplacements). The runtime
// runs each settle hook under that turn, once the Sandbox is found and before its first write, and
// hands it the read of a recorded act that takes no turn again (runtime.ActRead), since ReadAct,
// which takes it, answers ErrTurnBusy there. A launch the release's mark refuses just after
// RoundPodEnds answered yes leaves the pod running, so the round's record of that replacement reads
// not made under the release's turn.

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// settleReads is what a settle hook read under SuspendIssue: what ReadAct, which takes the pod's
// launch turn, answered there, the read handed to the hook of the pod's own act and of an earlier
// pod's, and the writes the runtime had made by then.
type settleReads struct {
	calls          int
	busy           error
	own, earlier   runtime.ActState
	ownErr, earErr error
	writes         []action
}

// settle is a settle hook that records what it read in reads, of the Sandbox whose pod is pod.
func (reads *settleReads) settle(g *rig, pod string) func(context.Context, runtime.ActRead) error {
	return func(ctx context.Context, read runtime.ActRead) error {
		reads.calls++
		reads.writes = g.writes()
		_, _, reads.busy = readAct(g, pod)
		reads.own, reads.ownErr = read(ctx, runtime.PodAct(pod))
		reads.earlier, reads.earErr = read(ctx, runtime.PodAct("uid-an-earlier-pod"))
		return nil
	}
}

// want holds reads to one call under the pod's launch turn, before any write, that read the pod's
// own act not made and an earlier pod's made.
func (reads *settleReads) want(t *testing.T) {
	t.Helper()
	if reads.calls != 1 {
		t.Fatalf("the settle hook ran %d times, want once", reads.calls)
	}
	if !errors.Is(reads.busy, runtime.ErrTurnBusy) {
		t.Errorf("ReadAct inside the settle hook answered %v, want ErrTurnBusy: the hook runs under the pod's launch turn", reads.busy)
	}
	if reads.ownErr != nil || reads.own != runtime.ActNotMade {
		t.Errorf("the hook's read of the act on the Sandbox's own pod = %d, %v; want it not made", reads.own, reads.ownErr)
	}
	if reads.earErr != nil || reads.earlier != runtime.ActMade {
		t.Errorf("the hook's read of the act on an earlier pod = %d, %v; want it made", reads.earlier, reads.earErr)
	}
	if len(reads.writes) != 0 {
		t.Errorf("the runtime had written %v when it ran the settle hook, want nothing: the hook comes before the first write", reads.writes)
	}
}

func TestSuspendIssueRunsItsSettleUnderThePodsLaunchTurnBeforeItsFirstWrite(t *testing.T) {
	t.Parallel()
	t.Run("the hook reads each act under the turn, and the delete goes on", func(t *testing.T) {
		g := newRig(t, nil)
		g.issueLaunchers(workerToken)
		loc := g.spawn(workerSpec(t))
		var reads settleReads
		g.clearActions()
		if err := g.r.SuspendIssue(g.ctx, testTree, testTree, authorized, reads.settle(g, loc.Sandbox.PodUID)); err != nil {
			t.Fatalf("SuspendIssue: %v", err)
		}
		reads.want(t)
		if s := g.sandbox(loc.Sandbox.Name); s != nil {
			t.Fatalf("after the suspension the API holds the Sandbox %+v, want it deleted", s)
		}
	})
	t.Run("a hook that refuses ends the suspension before any write", func(t *testing.T) {
		g := newRig(t, nil)
		g.issueLaunchers(workerToken)
		loc := g.spawn(workerSpec(t))
		before := g.sandbox(loc.Sandbox.Name)
		refused := errors.New("the runtime cannot say whether the recorded replacement of its pod was made")
		g.clearActions()
		err := g.r.SuspendIssue(g.ctx, testTree, testTree, authorized, func(context.Context, runtime.ActRead) error { return refused })
		if !errors.Is(err, refused) {
			t.Fatalf("SuspendIssue = %v, want the settle hook's error", err)
		}
		if writes := g.writes(); len(writes) != 0 {
			t.Fatalf("the suspension wrote %v after its settle hook refused, want nothing", writes)
		}
		if after := g.sandbox(loc.Sandbox.Name); !reflect.DeepEqual(after, before) {
			t.Fatalf("after the refused suspension the API holds the Sandbox %+v, want it unchanged, %+v", after, before)
		}
	})
	t.Run("control: a suspension with no hook deletes the Sandbox", func(t *testing.T) {
		g := newRig(t, nil)
		g.issueLaunchers(workerToken)
		loc := g.spawn(workerSpec(t))
		if err := g.r.SuspendIssue(g.ctx, testTree, testTree, authorized); err != nil {
			t.Fatalf("SuspendIssue: %v", err)
		}
		if s := g.sandbox(loc.Sandbox.Name); s != nil {
			t.Fatalf("after the suspension the API holds the Sandbox %+v, want it deleted", s)
		}
	})
}

// The first way the finding names, at the runtime: a launch into a pod a round covers asks
// RoundPodEnds, which answers yes with the release's mark still clear; the mark is set then, and the
// launch is refused (releaseHolds) before it reads, records or suspends anything, so the pod runs
// on as the Sandbox's own. The release's delete then runs its settle under the pod's launch turn,
// whose read of the round's act on that pod answers not made, and the daemon forgets its record
// there before the Sandbox goes.
func TestALaunchTheProjectReleaseRefusesAfterRoundPodEndsLeavesItsPodForTheSettle(t *testing.T) {
	t.Parallel()
	var asked []roundAsk
	held := &atomic.Bool{}
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.ProjectReleaseHolds = held.Load
		o.RoundPodEnds = func(_ context.Context, token claim.Token, podUID string) (bool, error) {
			asked = append(asked, roundAsk{token: token, pod: podUID})
			held.Store(true)
			return true, nil
		}
	}))
	loc := g.spawn(workerSpec(t))
	spec := workerSpec(t)
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	g.clearActions()
	_, err := g.r.Resume(g.ctx, &loc, spec)
	wantReleased(t, g, "the launch the mark refused after RoundPodEnds answered yes", err)
	if want := []roundAsk{{token: workerToken, pod: loc.Sandbox.PodUID}}; !slices.Equal(asked, want) {
		t.Fatalf("the launch asked RoundPodEnds %+v, want %+v", asked, want)
	}
	if s := g.sandbox(loc.Sandbox.Name); s == nil || s.mode() != modeRunning {
		t.Fatalf("the refused launch left the Sandbox %+v, want it running its pod", s)
	}
	var reads settleReads
	g.clearActions()
	if err := g.r.SuspendIssue(g.ctx, testTree, testTree, authorized, reads.settle(g, loc.Sandbox.PodUID)); err != nil {
		t.Fatalf("the release's SuspendIssue: %v", err)
	}
	reads.want(t)
	if s := g.sandbox(loc.Sandbox.Name); s != nil {
		t.Fatalf("after the release's delete the API holds the Sandbox %+v, want it deleted", s)
	}
}
