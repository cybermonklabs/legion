package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// idleNow is the moment every row of the table decides at, and idlePeriod the idle period it
// decides against: a pod that has recorded nothing for ten minutes is released. idlePod is the
// pod the issue's roles run in, which an update round over that pod is named for.
var (
	idleNow      = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	idlePeriod   = 10 * time.Minute
	idleContract = 47
	idlePod      = "9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22"
)

// idleChild is a child issue's pod that every condition of the rule holds for: in the workflow
// under a live root, on the configured image, one live implementer on the daemon's contract that
// finished its turn twenty minutes ago and reported five minutes later that it runs no background
// work with no reminder armed, a tester the daemon suspended beside it, and nothing queued for
// either. Each row of the table below changes exactly one thing about it.
func idleChild() pods.IdleView {
	return pods.IdleView{
		Issue: "LEGION-209", Tree: "LEGION-208", Found: true,
		Phase: phase.Implementing, Status: "in_progress", Generation: 3,
		Root: pods.IdleRoot{Phase: phase.Admitted, Status: "in_progress", Generation: 2},
		Claims: []pods.IdleClaim{
			{
				Role:  claim.RoleImplementer,
				State: supervise.StateIdle, PluginContract: idleContract, ServingGeneration: 3,
				LastTurnAt: idleNow.Add(-20 * time.Minute),
				ActivityAt: idleNow.Add(-15 * time.Minute), UpdatedAt: idleNow.Add(-15 * time.Minute),
			},
			{
				Role: claim.RoleTester, State: supervise.StateSuspended,
				PluginContract: 11, UpdatedAt: idleNow.Add(-40 * time.Minute),
			},
		},
	}
}

// idleRoot is the same pod as its tree's root, which is the only shape a root's own wait on a
// person holds.
func idleRoot() pods.IdleView {
	view := idleChild()
	view.Issue, view.Tree, view.Phase = "LEGION-208", "LEGION-208", phase.Admitted
	view.Generation = 2
	view.Claims = view.Claims[:1]
	view.Claims[0].Role = claim.RoleArchitect
	view.Claims[0].ServingGeneration = 2
	return view
}

// Every condition of the idle rule is one row here: it holds the pod and names the hold the
// daemon records, and the pod with every condition met is released with its live roles named.
// The rule is pure, so each row is the view, the clock, the idle period, the image verdict and
// the daemon's plugin contract and nothing else.
//
// The condition this branch left out until sjawhar/legion#2538 landed — "no INFRA-484 update
// round open on the pod" — is the two rows below the plugin contract's: #2538 is on main now,
// and the rule reads supervise.Claim.UpdateRound with supervise.IsPodRound.
func TestTheIdleRuleNamesTheOneConditionThatHeldEachPod(t *testing.T) {
	for _, tc := range []struct {
		name string
		view pods.IdleView
		// image is whether the pod runs the configured image, and drifted whether it was built
		// from a repository the issue's record has left. A row that sets neither reads a pod on
		// the configured image and on the repository the record holds, which is every pod of a
		// deployment nothing has changed under.
		image   bool
		drifted bool
		want    IdleHold
		roles   []claim.Role
	}{
		{
			name: "every condition holds", view: idleChild(), image: true, want: IdleHoldNone,
			roles: []claim.Role{claim.RoleImplementer},
		},
		{
			name:  "the record holds no such issue",
			view:  func() pods.IdleView { v := idleChild(); v.Found = false; return v }(),
			image: true, want: IdleHoldNoPod,
		},
		{
			name: "the pod has no live claim",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[0].State = supervise.StateRetired
				return v
			}(),
			image: true, want: IdleHoldNoLiveClaim,
		},
		{
			name:  "a release of the pod is already in progress",
			view:  func() pods.IdleView { v := idleChild(); v.Releasing = true; return v }(),
			image: true, want: IdleHoldReleasing,
		},
		{
			name:  "the issue has left the workflow",
			view:  func() pods.IdleView { v := idleChild(); v.Status = "backlog"; return v }(),
			image: true, want: IdleHoldTreeNotLive,
		},
		{
			name:  "the tree root lingers",
			view:  func() pods.IdleView { v := idleChild(); v.Root.LingerUntil = &idleNow; return v }(),
			image: true, want: IdleHoldTreeNotLive,
		},
		{
			name:  "a bundle of the pod's work is still waiting to be restored",
			view:  func() pods.IdleView { v := idleChild(); v.BundleUnrestored = true; return v }(),
			image: true, want: IdleHoldBundleUnrestored,
		},
		{
			name: "the root waits on a person who has not answered",
			view: func() pods.IdleView {
				v := idleRoot()
				v.Phase, v.Root.Phase, v.Root.AwaitingPerson = phase.AwaitingPerson, phase.AwaitingPerson, true
				return v
			}(),
			image: true, want: IdleHoldAwaitingPerson,
		},
		{
			name: "the root's person has answered and it waits only for a slot",
			view: func() pods.IdleView {
				v := idleRoot()
				v.Phase, v.Root.Phase = phase.AwaitingPerson, phase.AwaitingPerson
				v.Root.AwaitingPerson, v.Root.Answered = true, true
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleArchitect},
		},
		{
			name: "a child of a root that waits on a person",
			view: func() pods.IdleView {
				v := idleChild()
				v.Root.Phase, v.Root.AwaitingPerson = phase.AwaitingPerson, true
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleImplementer},
		},
		{
			name: "the pod does not run the configured image", view: idleChild(), image: false, want: IdleHoldImage,
		},
		{
			// INFRA-3136 R3: Dispatch moved the issue, the daemon's record followed it, and the
			// pod still running is a clone of the repository the issue has left. The pod is the
			// update round's to replace: a release of it would save work no pod of this issue
			// can ever restore.
			name: "the pod was built from a repository the issue has left", view: idleChild(),
			image: true, drifted: true, want: IdleHoldRepository,
		},
		{
			// The image is read first, as the rule reads it: a pod behind on both is one hold
			// and the first one named.
			name: "a pod behind on its image and on its repository", view: idleChild(),
			image: false, drifted: true, want: IdleHoldImage,
		},
		{
			name:  "a live claim speaks an older plugin contract",
			view:  func() pods.IdleView { v := idleChild(); v.Claims[0].PluginContract = idleContract - 1; return v }(),
			image: true, want: IdleHoldPluginContract,
		},
		{
			name: "an update round of the pod is open on a live claim",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[0].UpdateRound = supervise.PodRound(idlePod)
				return v
			}(),
			image: true, want: IdleHoldUpdateRound,
		},
		{
			// A round the suspended tester still records is the round of a pod it is no longer
			// in: it holds nothing, exactly as its state and its contract hold nothing.
			name: "only a suspended claim records an update round",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[1].UpdateRound = supervise.PodRound(idlePod)
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleImplementer},
		},
		{
			name:  "a role is in a turn",
			view:  func() pods.IdleView { v := idleChild(); v.Claims[0].State = supervise.StateWorking; return v }(),
			image: true, want: IdleHoldInTurn,
		},
		{
			name:  "a role's launch is not yet known",
			view:  func() pods.IdleView { v := idleChild(); v.Claims[0].State = supervise.StateLaunchUncertain; return v }(),
			image: true, want: IdleHoldInTurn,
		},
		{
			name: "a task waits for delivery to a role of the pod",
			view: func() pods.IdleView {
				v := idleChild()
				v.Waiting = true
				return v
			}(),
			image: true, want: IdleHoldDelivery,
		},
		{
			name: "a live role's session reported background work of its own",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[0].ActivityBackground = true
				v.Claims[0].ActivityKinds = []string{"async-job"}
				return v
			}(),
			image: true, want: IdleHoldBackgroundWork,
		},
		{
			name:  "a live role's session has reported nothing",
			view:  func() pods.IdleView { v := idleChild(); v.Claims[0].ActivityAt = time.Time{}; return v }(),
			image: true, want: IdleHoldUnreported,
		},
		{
			name: "a live role's report predates its last turn",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[0].ActivityAt = v.Claims[0].LastTurnAt.Add(-time.Minute)
				return v
			}(),
			image: true, want: IdleHoldUnreported,
		},
		{
			name: "an open phase's reminder is due inside the period",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[0].ActivityNextReminderAt = idleNow.Add(-time.Minute)
				return v
			}(),
			image: true, want: IdleHoldReminder,
		},
		{
			name: "an open phase's reminder is due after the period",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[0].ActivityNextReminderAt = idleNow.Add(5 * time.Minute)
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleImplementer},
		},
		{
			name: "a reminder of a worker whose phase is not open",
			view: func() pods.IdleView {
				v := idleChild()
				v.Phase = phase.Reviewing
				v.Claims[0].ActivityNextReminderAt = idleNow.Add(-time.Minute)
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleImplementer},
		},
		{
			name: "a reminder of a worker serving an earlier generation",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[0].ServingGeneration = 2
				v.Claims[0].ActivityNextReminderAt = idleNow.Add(-time.Minute)
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleImplementer},
		},
		{
			name: "the pod recorded something inside the idle period",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[0].UpdatedAt = idleNow.Add(-time.Minute)
				return v
			}(),
			image: true, want: IdleHoldNotIdle,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := IdleRelease(tc.view, idleNow, idlePeriod, tc.image, tc.drifted, idleContract)
			if got.Hold != tc.want {
				t.Fatalf("hold = %s, want %s", got.Hold, tc.want)
			}
			if want := tc.want == IdleHoldNone; got.Release != want {
				t.Fatalf("release = %v, want %v (hold %s)", got.Release, want, got.Hold)
			}
			if len(got.Roles) != len(tc.roles) {
				t.Fatalf("roles = %v, want %v", got.Roles, tc.roles)
			}
			for i, role := range tc.roles {
				if got.Roles[i] != role {
					t.Fatalf("roles = %v, want %v", got.Roles, tc.roles)
				}
			}
		})
	}
}

// unownedIssue is a root issue of a live tree that no claim of it can use any more: an architect
// that failed, a planner the daemon retired and an implementer the daemon's own close suspended,
// none of the three recording a process, the newest of their claim writes forty minutes old,
// nothing queued for a role of it, its Sandbox there and no release of it under way. Every
// condition of the rule for an issue with no live claim holds for it, and each row of the table
// below changes exactly one thing about it.
func unownedIssue() pods.IdleView {
	return pods.IdleView{
		Issue: "LEGION-310", Tree: "LEGION-310", Found: true,
		Phase: phase.Implementing, Status: "in_progress", Generation: 4,
		Root: pods.IdleRoot{Phase: phase.Implementing, Status: "in_progress", Generation: 4},
		Claims: []pods.IdleClaim{
			{
				Role: claim.RoleArchitect, State: supervise.StateFailed,
				UpdatedAt: idleNow.Add(-90 * time.Minute),
			},
			{
				Role: claim.RolePlanner, State: supervise.StateRetired,
				UpdatedAt: idleNow.Add(-60 * time.Minute),
			},
			{
				Role: claim.RoleImplementer, State: supervise.StateSuspended,
				SuspendedBy: supervise.SuspendedByDaemon,
				UpdatedAt:   idleNow.Add(-40 * time.Minute),
			},
		},
	}
}

// The pod of an issue every claim of which has stopped is released, and each condition that
// keeps such a pod standing is one row here, holding it under a hold of its own. The rows below
// the holds prove the three states this rule is deliberately not held by: a bundle the last
// release could not restore, which this release takes none of, and the two suspensions that are
// the daemon's own rather than an operator's. The rule is pure, so each row is the view, the
// clock, the idle period and whether the runtime holds the issue's Sandbox, and nothing else.
func TestTheRuleForAnIssueWithNoLiveClaim(t *testing.T) {
	for _, tc := range []struct {
		name    string
		view    pods.IdleView
		sandbox bool
		want    IdleHold
	}{
		{
			name: "every condition holds", view: unownedIssue(), sandbox: true, want: IdleHoldNone,
		},
		{
			name: "a claim of the issue is live",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Claims[2].State, v.Claims[2].SuspendedBy = supervise.StateIdle, ""
				return v
			}(),
			sandbox: true, want: IdleHoldUnownedClaimLive,
		},
		{
			name: "a claim is suspended by an operator",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Claims[2].SuspendedBy = supervise.SuspendedByOperator
				return v
			}(),
			sandbox: true, want: IdleHoldUnownedOperatorSuspended,
		},
		{
			name: "the issue's Sandbox is gone", view: unownedIssue(), sandbox: false,
			want: IdleHoldUnownedSandboxGone,
		},
		{
			name: "a suspended claim still records a process",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Claims[2].HasProcess = true
				return v
			}(),
			sandbox: true, want: IdleHoldUnownedClaimProcess,
		},
		{
			name:    "a delivery waits for a role of the issue",
			view:    func() pods.IdleView { v := unownedIssue(); v.Waiting = true; return v }(),
			sandbox: true, want: IdleHoldUnownedDelivery,
		},
		{
			name:    "the issue has left the workflow",
			view:    func() pods.IdleView { v := unownedIssue(); v.Status = "backlog"; return v }(),
			sandbox: true, want: IdleHoldUnownedTreeNotLive,
		},
		{
			name:    "the tree lingers after its close",
			view:    func() pods.IdleView { v := unownedIssue(); v.Root.LingerUntil = &idleNow; return v }(),
			sandbox: true, want: IdleHoldUnownedTreeNotLive,
		},
		{
			name:    "a release of that Sandbox is already under way",
			view:    func() pods.IdleView { v := unownedIssue(); v.Releasing = true; return v }(),
			sandbox: true, want: IdleHoldUnownedReleasing,
		},
		{
			// The newest write of any claim decides, so moving the oldest claim's write inside
			// the period is what holds the pod, although two claims are older than it.
			name: "the newest claim write is inside the idle period",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Claims[0].UpdatedAt = idleNow.Add(-time.Minute)
				return v
			}(),
			sandbox: true, want: IdleHoldUnownedNotIdle,
		},
		{
			name:    "the record holds no claim of the issue",
			view:    func() pods.IdleView { v := unownedIssue(); v.Claims = nil; return v }(),
			sandbox: true, want: IdleHoldUnownedNoClaim,
		},
		{
			name:    "the record does not hold the issue at all",
			view:    func() pods.IdleView { v := unownedIssue(); v.Found = false; return v }(),
			sandbox: true, want: IdleHoldUnownedNoClaim,
		},
		{
			// The idle rule keeps such a pod, because its own release would store a bundle over
			// the unrestored one. This release stores nothing, so the bundle holds nothing.
			name:    "a bundle the last release could not restore",
			view:    func() pods.IdleView { v := unownedIssue(); v.BundleUnrestored = true; return v }(),
			sandbox: true, want: IdleHoldNone,
		},
		{
			// The ordinary case this rule meets: an earlier idle release suspended the role and
			// no wake brought it back.
			name: "a claim an earlier release suspended",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Claims[2].SuspendedBy = supervise.SuspendedByReleased
				return v
			}(),
			sandbox: true, want: IdleHoldNone,
		},
		{
			// Migration 0038's backfill of every suspension written before the column existed,
			// most of which are the daemon's own: holding on it would keep those pods for ever.
			name: "a claim suspended before the source was recorded",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Claims[2].SuspendedBy = supervise.SuspendedByUnknown
				return v
			}(),
			sandbox: true, want: IdleHoldNone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := UnownedPodRelease(tc.view, idleNow, idlePeriod, tc.sandbox)
			if got.Hold != tc.want {
				t.Fatalf("hold = %s, want %s", got.Hold, tc.want)
			}
			if want := tc.want == IdleHoldNone; got.Release != want {
				t.Fatalf("release = %v, want %v (hold %s)", got.Release, want, got.Hold)
			}
			if len(got.Roles) != 0 {
				t.Fatalf("roles = %v, want none: this release suspends no role", got.Roles)
			}
		})
	}
}

// Every hold the rule can report has a name of its own, which is what the daemon records of a pod
// it kept: two holds that printed the same would make the record unreadable.
func TestEveryIdleHoldHasANameOfItsOwn(t *testing.T) {
	seen := map[string]IdleHold{}
	for hold := IdleHoldNone; hold <= IdleHoldUnownedNotIdle; hold++ {
		name := hold.String()
		if name == "unknown" {
			t.Fatalf("hold %d has no name", int(hold))
		}
		if other, taken := seen[name]; taken {
			t.Fatalf("holds %d and %d are both %q", int(other), int(hold), name)
		}
		seen[name] = hold
	}
	if got := IdleHold(int(IdleHoldUnownedNotIdle) + 1).String(); got != "unknown" {
		t.Fatalf("an unmapped hold prints %q, want unknown", got)
	}
}
