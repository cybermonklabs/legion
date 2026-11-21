package daemon

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A relaunch of a claim Envoy cannot reach (R5), whose process is the last one in the pod of an
// update round that the round's ask never reached, keeps that pod before the round's bound. The
// relaunch asks nothing of the round before it stops the process (the runtime's RelaunchRefusal),
// and once its stop has ended that process no process of the pod is the claim's, the claim holds
// no ask, and, between its decisions, its machine holds nothing. So until the relaunch's launch
// has come up, both readers of the round would take the round as over and replace the pod, up to
// the bound early, and the claim's ephemeral disk and what it had not pushed with it: the launch,
// as the round's own replacement (roundPodEndsAtLaunch), and the sweep, by issue (issuePodKept).
// The relaunch holds the pod for both (roundPodEnds, relaunchesInPlace): the claim comes back in
// the round's pod, which nothing replaced, and the launch says why it kept it.
//
// Two rows. In the first the launch follows the stop at once. In the second the store does not
// answer the launch's recheck after the stop, so the launch waits to be tried again at the probe
// interval (supervise's relaunchRefused, relaunchLater), the claim launching with no process and
// its machine deciding nothing; the sweep runs three times in that wait, and only then the retry.
func TestARelaunchOfTheLastProcessUnderARoundItsAskNeverReachedKeepsThePod(t *testing.T) {
	for _, tc := range []struct {
		name string
		// unanswered is whether the store does not answer the launch's first recheck after the
		// relaunch's stop.
		unanswered bool
	}{
		{name: "its launch right after the stop"},
		{name: "a sweep while its launch waits on a recheck the store did not answer", unanswered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opts podUpdateRigOptions
			var refused atomic.Int32
			if tc.unanswered {
				// The recheck after the stop is the one whose claim records the relaunch with no
				// generation yet and no process (relaunchUnreachable, launchAfter); the one before
				// the stop still records the process, and the retry is answered.
				opts.checkLaunch = func(_ context.Context, c supervise.Claim) error {
					if !c.LastRelaunch.IsZero() && c.LastRelaunchGeneration == 0 && c.Locator == nil && refused.CompareAndSwap(0, 1) {
						return errors.New("the store did not answer the launch's recheck")
					}
					return nil
				}
			}
			implementer := podUpdateRole{role: claim.RoleImplementer, state: supervise.StateIdle, session: true, inPod: true,
				contract: api.DaemonAPIVersion, round: podUpdateRound, reasons: podUpdateDrift, unasked: true}
			rig := newPodUpdateRigWith(t, opts, implementer, answeredThenSuspended(claim.RoleTester))
			// The sweep's replacement by issue finds the round's pod still in the issue's Sandbox.
			rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
			// The relaunch guard reads the session's last activity report: an idle session that
			// runs nothing of its own (supervise.RelaunchGuard).
			rig.reportsNothingRunning(claim.RoleImplementer)
			rig.intoRoundPod(claim.RoleImplementer)

			token := rig.tokens[claim.RoleImplementer]
			rig.handle(claim.RoleImplementer, supervise.RequestRelaunch{Claim: token, Cause: "Envoy could not reach its session",
				At: rig.clock.Now()})

			if tc.unanswered {
				waiting := rig.awaitClaim(claim.RoleImplementer, "its launch to wait for the retry", func(c supervise.Claim) bool {
					return c.State == supervise.StateLaunching && c.Locator == nil
				})
				if refused.Load() != 1 || waiting.LastRelaunch.IsZero() || waiting.LastRelaunchGeneration != 0 || waiting.Generation != 1 {
					t.Fatalf("after %d refused rechecks the claim records its relaunch at %s, generation %d, at generation %d; want one refused recheck, the relaunch recorded with its launch still to come, at generation 1",
						refused.Load(), waiting.LastRelaunch, waiting.LastRelaunchGeneration, waiting.Generation)
				}
				rig.settleAll()
				for range 3 {
					rig.tick()
				}
				rig.settleAll()
				if asked := rig.byIssueAsks(); len(asked) != 0 {
					t.Fatalf("the sweep asked the runtime to replace the pod by issue %+v while the relaunch's launch waited, want the pod kept", asked)
				}
				if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound {
					t.Fatalf("the implementer is in round %q after the sweeps, want the round kept open over its pod", c.UpdateRound)
				}
				if fired := rig.clock.fire(time.Second); fired == 0 {
					t.Fatalf("no timer at the probe interval to fire, want the relaunch's launch waiting on one")
				}
			}

			c := rig.awaitClaim(claim.RoleImplementer, "its relaunch to record its process", func(c supervise.Claim) bool {
				return c.Locator != nil && c.Generation > 1
			})
			if pod := podOf(c.Locator); pod != podUpdatePod {
				t.Fatalf("the relaunch brought the implementer up in pod %s, want the round's pod %s kept before the round's bound", pod, podUpdatePod)
			}
			if replaced := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 0 {
				t.Fatalf("the relaunch's launch replaced %+v as the round's replacement, want the pod kept", replaced)
			}
			if asked := rig.byIssueAsks(); len(asked) != 0 {
				t.Fatalf("the pod was replaced by issue %+v, want it kept", asked)
			}
			reasons := rig.launchKeptBecause(claim.RoleImplementer)
			if len(reasons) != 1 || !strings.Contains(reasons[0], "on its same session") {
				t.Fatalf("the launch hook kept the pod because %q, want once because of the claim's relaunch on its same session", reasons)
			}
			if c.LastRelaunch.IsZero() || c.LastRelaunchGeneration != c.Generation {
				t.Errorf("the claim records its relaunch at %s, generation %d, want the relaunch recorded with the launch's generation %d",
					c.LastRelaunch, c.LastRelaunchGeneration, c.Generation)
			}
		})
	}
}
