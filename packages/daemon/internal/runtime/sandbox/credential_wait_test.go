package sandbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// A launcher's hello says whether it waits for a start's credential, and the runtime answers it for
// the slot (launchers.credentialWaits): a launcher of an image before that wait answers its pod's
// uid and false, which a re-role into its slot waits out; a launcher that waits answers true; and a
// slot with no launcher connected, or one whose connection closed, answers true, since the launch
// then builds or waits for a pod of this release.
func TestASlotsLauncherSaysWhetherItWaitsForTheCredential(t *testing.T) {
	launchers := testLaunchers()
	if pod, waits := launchers.credentialWaits(workerToken); pod != "" || !waits {
		t.Fatalf("with no launcher connected = %q, %t; want none, waits", pod, waits)
	}
	old := launchers.accept(workerToken, testTree, time.Time{}, shimwire.LauncherHello{PodUID: "pod-1", LauncherID: "old"})
	if pod, waits := launchers.credentialWaits(workerToken); pod != "pod-1" || waits {
		t.Fatalf("a launcher that said no credential wait = %q, %t; want pod-1, no wait", pod, waits)
	}
	launchers.accept(workerToken, testTree, time.Time{}, shimwire.LauncherHello{PodUID: "pod-2", LauncherID: "new", CredentialWait: true})
	if pod, waits := launchers.credentialWaits(workerToken); pod != "pod-2" || !waits {
		t.Fatalf("a launcher that waits = %q, %t; want pod-2, waits", pod, waits)
	}
	if !old.closed {
		t.Fatal("the earlier launcher's connection is still open: the case's premise")
	}
	launchers.accept(workerToken, testTree, time.Time{}, shimwire.LauncherHello{PodUID: "pod-3", LauncherID: "newest"}).close()
	if pod, waits := launchers.credentialWaits(workerToken); pod != "" || !waits {
		t.Fatalf("after the launcher's connection closed = %q, %t; want none, waits", pod, waits)
	}
}

// A launch into a kept pod judges the launcher it reached, whatever the outbox judged before it:
// right after a daemon restart no launcher is connected, so the outbox lets a re-role through, and
// the launch may then reach a launcher of the release before, whose hello said no credential wait
// and which starts a child at once, as whatever App the slot's gh files held. Key a's claim
// `…-implementer` runs as the implementer in the kept pod, then is launched as the tester, a role
// of the review App: with such a launcher the launch is refused, runtime.ErrAwaitsPodUpdate,
// naming the pod and the slot, before anything is written, stopped or sent in the pod. The
// controls are the same launch with a launcher that waits, and a launch as the merger, a role of
// the implement App the slot already holds, with a launcher that does not wait: both start.
func TestALaunchOfAnotherAppIntoASlotWhoseLauncherDoesNotWaitIsRefused(t *testing.T) {
	keyed := claim.Token("legion-legion-legion-208-implementer")
	for _, tc := range []struct {
		name    string
		waits   bool
		role    claim.Role
		refused bool
	}{
		{name: "a launcher that does not wait, the claim on a role of another App", role: claim.RoleTester, refused: true},
		{name: "control: a launcher that waits, the claim on a role of another App", waits: true, role: claim.RoleTester},
		{name: "control: a launcher that does not wait, the claim on a role of the same App", role: claim.RoleMerger},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var options []rigOption
			if !tc.waits {
				options = append(options, withoutCredentialWait())
			}
			g := newRig(t, nil, options...)
			g.spawn(rootSpec(t))
			loc := g.spawn(testSpec(t, keyed, claim.RoleImplementer, testTree))
			g.clearActions()
			sent := len(g.sent(keyed))

			again := testSpec(t, keyed, tc.role, testTree)
			again.Generation++
			_, err := g.r.Spawn(g.ctx, again)
			starts := slices.ContainsFunc(g.sent(keyed)[sent:], func(frame shimwire.Frame) bool {
				start, ok := frame.(shimwire.LauncherStart)
				return ok && start.Generation == again.Generation
			})
			if !tc.refused {
				if err != nil || !starts {
					t.Fatalf("the launch as the %s = %v, sending a start: %t; want it started", tc.role, err, starts)
				}
				return
			}
			if !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
				t.Fatalf("the launch as the %s into a launcher that does not wait = %v, want an error that is runtime.ErrAwaitsPodUpdate", tc.role, err)
			}
			if !strings.Contains(err.Error(), loc.Sandbox.PodUID) || !strings.Contains(err.Error(), "implementer") {
				t.Errorf("the refusal %q, want it to name the pod %s and the slot implementer", err, loc.Sandbox.PodUID)
			}
			if writes := g.writes(); len(writes) > 0 {
				t.Errorf("the refused launch wrote %v, want nothing written", writes)
			}
			if frames := g.sent(keyed)[sent:]; len(frames) > 0 {
				t.Errorf("the refused launch sent its launcher %#v, want nothing", frames)
			}
		})
	}
}

// A read that fails in a launch into a kept pod whose launcher does not wait for the credential is
// an error the launch retries. It is never a slot read as starting another App, which ends the
// launch for the pod's update round (runtime.ErrAwaitsPodUpdate). The claim `…-implementer` runs
// as the checker, a role the deployment defines as the implement App, and is launched again as the
// checker. After the setup, one read fails from one of its asks on, an ask of the launch's check of
// the App its slot holds (slotAppChanges): the runtime's ActsAs at its first ask, the one that
// check makes before anything else of the launch asks it; or the plan reader of a role's App
// (appauth.RoleApps.Plan) at its second, the check's read for the role the slot's last launch
// recorded, since the check asks it for each role of the slot that is none of Legion's. The launch
// answers the read's error, has asked the read exactly that many times, so it stopped at the ask
// that failed, sends no start, and is not runtime.ErrAwaitsPodUpdate. The control: with reads that
// never fail, the same launch starts.
func TestAFailedActsAsAskInALaunchIsRetriedNotHeldForThePodsRound(t *testing.T) {
	keyed := claim.Token("legion-legion-legion-208-implementer")
	failed := errors.New("the record could not be read")
	for _, tc := range []struct {
		name string
		// fails is the read that fails, "actsAs", "plan", or "" for none, at its ask from, counted
		// from the arming, and at each ask after it.
		fails string
		from  int32
	}{
		{name: "an ActsAs that fails at its first ask", fails: "actsAs", from: 1},
		{name: "a plan read that fails at its second ask", fails: "plan", from: 2},
		{name: "control: reads that never fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var armed atomic.Bool
			asked := map[string]*atomic.Int32{"actsAs": new(atomic.Int32), "plan": new(atomic.Int32)}
			// answer is read's answer once armed: failed from its ask from on where read is the one
			// the case fails, and nil otherwise.
			answer := func(read string) error {
				if armed.Load() && asked[read].Add(1) >= tc.from && tc.fails == read {
					return failed
				}
				return nil
			}
			g := newRig(t, nil, withoutCredentialWait(),
				withOptions(func(o *Options) {
					o.RoleApps.Deployed = appauth.DeploymentApps{"checker": appauth.Implement}
					o.ActsAs = func(_ context.Context, token claim.Token) (claim.Role, bool, error) {
						if token != keyed {
							return "", false, nil
						}
						return "", false, answer("actsAs")
					}
				}),
				// The plan reader reaches the runtime's roleApps through Options.RoleApps, which this
				// option sets with the credential that reads it.
				withProjectTokensOf(refusingOwnerTokens{refuse: new(atomic.Bool)}, func(context.Context, string, claim.Role) (appauth.Planned, error) {
					return appauth.Planned{}, answer("plan")
				}))
			g.spawn(rootSpec(t))
			g.spawn(testSpec(t, keyed, "checker", testTree))
			sent := len(g.sent(keyed))

			again := testSpec(t, keyed, "checker", testTree)
			again.Generation++
			armed.Store(true)
			_, err := g.r.Spawn(g.ctx, again)
			starts := slices.ContainsFunc(g.sent(keyed)[sent:], func(frame shimwire.Frame) bool {
				start, ok := frame.(shimwire.LauncherStart)
				return ok && start.Generation == again.Generation
			})
			if tc.fails == "" {
				if err != nil || !starts {
					t.Fatalf("the launch with reads that never fail = %v, sending a start: %t; want it started", err, starts)
				}
				return
			}
			if n := asked[tc.fails].Load(); n != tc.from {
				t.Fatalf("the launch asked the %s read %d times, want %d: it must stop at the ask that failed, in slotAppChanges", tc.fails, n, tc.from)
			}
			if errors.Is(err, runtime.ErrAwaitsPodUpdate) || !errors.Is(err, failed) || starts {
				t.Fatalf("the launch with a %s read that failed = %v, sending a start: %t; want the read's error, which the launch retries, "+
					"no start, and no wait for the pod's update round", tc.fails, err, starts)
			}
		})
	}
}

// Whether a launch into a kept pod starts its slot as another App (slotAppChanges) is judged first
// by the runtime's ActsAs. A claim ActsAs gives a role holds that role's App at every write of its
// slot (runtime.SlotRole), whatever role it is launched for and whatever its slot held before, so
// its launch changes no App, and the slot's Secret is not read for it. Key a's claim
// `…-implementer` runs as the implementer in the kept pod, which records that launch on the slot's
// Secret, and each case asks of a launch of the claim again. An acting claim launched as the
// tester, a role of the review App, answers no change, with no read of its Secret, which fails
// here. A claim that acts as nothing launched as the tester answers a change, and where its Secret
// cannot be read, the read's error, which the launch retries. The controls: a claim that acts as
// nothing launched as the merger, a role of the implement App the slot holds, answers no change, so
// an answer that is always a change fails; and the failing read under a claim that acts as nothing
// is counted, so the acting case's count of reads can fail.
func TestALaunchOfAnActingClaimChangesNoAppAndReadsNoSecret(t *testing.T) {
	keyed := claim.Token("legion-legion-legion-208-implementer")
	failed := errors.New("the Secret could not be read")
	for _, tc := range []struct {
		name string
		// acting is the role ActsAs gives the claim, "" for none, and role the launch's.
		acting, role claim.Role
		// unreadable fails each read of the slot's Secret.
		unreadable bool
		changes    bool
		reads      int32
		err        error
	}{
		{name: "an acting claim whose slot held another App", acting: claim.RoleTester, role: claim.RoleTester, unreadable: true},
		{name: "a claim that acts as nothing, whose slot held another App", role: claim.RoleTester, changes: true, reads: 1},
		{name: "a claim that acts as nothing, whose Secret cannot be read", role: claim.RoleTester, unreadable: true, reads: 1, err: failed},
		{name: "control: a claim that acts as nothing, whose slot held its App", role: claim.RoleMerger, reads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var armed atomic.Bool
			g := newRig(t, nil, withOptions(func(o *Options) {
				o.ActsAs = func(_ context.Context, token claim.Token) (claim.Role, bool, error) {
					if !armed.Load() || token != keyed || tc.acting == "" {
						return "", false, nil
					}
					return tc.acting, true, nil
				}
			}))
			g.spawn(rootSpec(t))
			g.spawn(testSpec(t, keyed, claim.RoleImplementer, testTree))
			pod := g.pod(SandboxName(keyed))
			slot := roleSecretName(pod.Name, claim.RoleImplementer)
			var reads atomic.Int32
			g.kube.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
				if !armed.Load() || a.(k8stesting.GetAction).GetName() != slot {
					return false, nil, nil
				}
				reads.Add(1)
				if tc.unreadable {
					return true, nil, failed
				}
				return false, nil, nil
			})

			armed.Store(true)
			changes, err := g.r.slotAppChanges(g.ctx, pod, testSpec(t, keyed, tc.role, testTree))
			armed.Store(false)
			if changes != tc.changes || !errors.Is(err, tc.err) {
				t.Errorf("a launch as the %s, ActsAs giving %q, answers a change: %t, %v; want %t, %v", tc.role, tc.acting, changes, err, tc.changes, tc.err)
			}
			if n := reads.Load(); n != tc.reads {
				t.Errorf("the launch read the slot's Secret %d times, want %d", n, tc.reads)
			}
		})
	}
}

// A slot whose name no claim token carries is refused, never answered as a slot whose launcher
// waits for the credential: that answer lets a re-role into the slot start with no credential wait
// (the daemon's reroleWaitsForCredential). The control: the slot of an agent's name with no
// launcher connected still answers that the launch waits.
func TestASlotNoTokenCarriesIsRefusedRatherThanLetThrough(t *testing.T) {
	r, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if pod, waits, err := r.SlotWaitsForCredential(testTree, "implementer"); err != nil || pod != "" || !waits {
		t.Fatalf("the slot implementer with no launcher connected = %q, %t, %v; want none, waits", pod, waits, err)
	}
	if pod, waits, err := r.SlotWaitsForCredential(testTree, "Implementer"); err == nil {
		t.Fatalf("the slot Implementer, a name no claim token carries = %q, %t: a re-role into it would start with no credential wait; want a refusal", pod, waits)
	}
}
