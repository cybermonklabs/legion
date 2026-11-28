package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
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
	t.Parallel()
	for _, tc := range []struct {
		name  string
		view  pods.IdleView
		image bool
		want  IdleHold
		roles []claim.Role
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
			name:  "the project is paused",
			view:  func() pods.IdleView { v := idleChild(); v.Paused = true; return v }(),
			image: true, want: IdleHoldPaused,
		},
		{
			// The resume ended the pause, and no role of the pod has been ready since.
			name:  "a pause holds the pod",
			view:  func() pods.IdleView { v := idleChild(); v.PauseHeld = true; return v }(),
			image: true, want: IdleHoldPauseHeld,
		},
		{
			name: "the project is paused and its pod holds no live claim",
			view: func() pods.IdleView {
				v := idleChild()
				v.Paused, v.Claims[0].State = true, supervise.StateSuspended
				return v
			}(),
			image: true, want: IdleHoldPaused,
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
			name: "the root's person has answered and it waits only for the pass that resumes it",
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
		{
			// The release's check found work that no branch or tag on origin holds, and no role
			// has started a turn or a process since.
			name: "a decline for unpushed work stands",
			view: func() pods.IdleView {
				v := idleChild()
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork}
				return v
			}(),
			image: true, want: IdleHoldUnpushedWork,
		},
		{
			// A turn that started before the decline is the turn the check already read.
			name: "a turn that started before the decline does not end it",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[0].LastTurnStart = idleNow.Add(-40 * time.Minute)
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork}
				return v
			}(),
			image: true, want: IdleHoldUnpushedWork,
		},
		{
			// A live role started a turn after the decline: the turn is what can change the
			// workspace, so the pass marks a release again and its check reads the workspace again.
			name: "a live role's turn after the decline ends it",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[0].LastTurnStart = idleNow.Add(-20 * time.Minute)
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork}
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleImplementer},
		},
		{
			// The suspended tester's turn start is a record of a process that is gone: it changes
			// nothing in the workspace now, so it ends nothing.
			name: "a turn start of a claim that is not live does not end the decline",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[1].LastTurnStart = idleNow.Add(-20 * time.Minute)
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork}
				return v
			}(),
			image: true, want: IdleHoldUnpushedWork,
		},
		{
			// A start row above the decline's is a role process started again, which can run in a
			// pod whose workspace was made again.
			name: "a start row higher than the decline's ends it",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[1].LastStartRow = 6
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork, StartedRow: 5}
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleImplementer},
		},
		{
			name: "a decline for a workspace the check could not read stands inside its retry",
			view: func() pods.IdleView {
				v := idleChild()
				v.Declined = pods.IdleDecline{At: idleNow.Add(-pods.DeclineUnreadRetry + time.Minute), Reason: pods.DeclineWorkspaceUnread}
				return v
			}(),
			image: true, want: IdleHoldWorkspaceUnread,
		},
		{
			// A read that failed can have a cause that clears by itself, so the pod is asked again
			// once the retry has passed, with no turn and no start.
			name: "a decline for a workspace the check could not read ends after its retry",
			view: func() pods.IdleView {
				v := idleChild()
				v.Declined = pods.IdleDecline{At: idleNow.Add(-pods.DeclineUnreadRetry), Reason: pods.DeclineWorkspaceUnread}
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleImplementer},
		},
		{
			// The retry counts from the last check, not from the decline's first moment: a check
			// that could not read the workspace thirty minutes ago is not asked again yet, however
			// long ago the first unread check was.
			name: "a decline for a workspace the check could not read retries from its last check",
			view: func() pods.IdleView {
				v := idleChild()
				v.Declined = pods.IdleDecline{At: idleNow.Add(-2 * pods.DeclineUnreadRetry), CheckedAt: idleNow.Add(-30 * time.Minute),
					Reason: pods.DeclineWorkspaceUnread}
				return v
			}(),
			image: true, want: IdleHoldWorkspaceUnread,
		},
		{
			// A later check read the same work again after the turn, so the turn is one a check
			// has already read: the decline keeps its first moment and still stands.
			name: "a turn after the decline but before its last check does not end it",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[0].LastTurnStart = idleNow.Add(-20 * time.Minute)
				v.Declined = pods.IdleDecline{At: idleNow.Add(-50 * time.Minute), CheckedAt: idleNow.Add(-18 * time.Minute),
					Reason: pods.DeclineUnpushedWork}
				return v
			}(),
			image: true, want: IdleHoldUnpushedWork,
		},
		{
			// Control for the row above: the same turn after the last check is one no check read.
			name: "a turn after the decline's last check ends it",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[0].LastTurnStart = idleNow.Add(-20 * time.Minute)
				v.Declined = pods.IdleDecline{At: idleNow.Add(-50 * time.Minute), CheckedAt: idleNow.Add(-25 * time.Minute),
					Reason: pods.DeclineUnpushedWork}
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleImplementer},
		},
		{
			// A relaunch built the issue's pod again: the disk the check read went with the pod it
			// read, and the work the decline names with it, so the pass asks the new pod.
			name: "a decline read in another pod than the one the runtime holds now ends",
			view: func() pods.IdleView {
				v := idleChild()
				v.PodUID = "pod-b"
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork, PodUID: "pod-a"}
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleImplementer},
		},
		{
			name: "a decline read in the pod the runtime holds now stands",
			view: func() pods.IdleView {
				v := idleChild()
				v.PodUID = "pod-a"
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork, PodUID: "pod-a"}
				return v
			}(),
			image: true, want: IdleHoldUnpushedWork,
		},
		{
			// The runtime could not say which pod it holds: a pass that cannot tell keeps the pod
			// a decline says holds work.
			name: "a decline stands where the runtime names no pod",
			view: func() pods.IdleView {
				v := idleChild()
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork, PodUID: "pod-a"}
				return v
			}(),
			image: true, want: IdleHoldUnpushedWork,
		},
		{
			// The check could not say which pod it read: the same rule keeps the pod.
			name: "a decline that names no pod stands",
			view: func() pods.IdleView {
				v := idleChild()
				v.PodUID = "pod-b"
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork}
				return v
			}(),
			image: true, want: IdleHoldUnpushedWork,
		},
		{
			// The decline is the rule's last condition: a declined pod that recorded something
			// inside the period is held as not idle, which is what it is first.
			name: "a declined pod that recorded something inside the idle period",
			view: func() pods.IdleView {
				v := idleChild()
				v.Claims[0].UpdatedAt = idleNow.Add(-time.Minute)
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork}
				return v
			}(),
			image: true, want: IdleHoldNotIdle,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := IdleRelease(tc.view, idleNow, idlePeriod, tc.image, idleContract)
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

// The idle rule decides a pod from the view the release's own transaction reads
// (store.Store.ReleaseIdleIssue), and that view counts an issue's record only where its project
// is the daemon's. The operator's spawn makes a claim of another project's issue under the
// daemon's token, and a database the daemon shares with that project's daemon holds that
// daemon's record of the issue. Read as this pod's issue, it would let the rule release the pod
// with rows keyed by the other project's issue, which this daemon's outbox never runs, under a
// mark that never clears. It is no record of this daemon's, so the rule holds the pod as one of
// an issue the record does not hold (IdleHoldNoPod), and nothing is marked or queued.
func TestTheIdleRuleReleasesNoPodOfAnIssueAnotherProjectRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := migratedPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store over the test pool: %v", err)
	}
	t.Cleanup(st.Close)
	const issue = "OTHER-8"
	seedIssue(t, pool, record.Issue{Key: issue, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: issue, Project: "OTHER", Title: "OTHER's root", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	token, err := claim.NewToken("legion", issue, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue, Issue: issue,
		Role: claim.RoleArchitect, Generation: 1, State: supervise.StateIdle, PluginContract: idleContract}); err != nil {
		t.Fatalf("seed the operator's spawn of %s: %v", issue, err)
	}
	// The spawn has been quiet for twice the idle period, and its session reported after its last
	// turn that it runs no background work with no reminder armed.
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `update claims set updated_at = $2, activity_at = $2, activity_background = false,
		activity_kinds = '', activity_next_reminder_at = null where token = $1`, string(token), now.Add(-2*idlePeriod)); err != nil {
		t.Fatalf("backdate the spawn: %v", err)
	}

	var decided IdleDecision
	marked, err := st.ReleaseIdleIssue(ctx, "legion", issue, func(view pods.IdleView) (store.IdleReleaseMark, error) {
		decided = IdleRelease(view, now, idlePeriod, true, idleContract)
		if !decided.Release {
			return store.IdleReleaseMark{}, nil
		}
		return store.IdleReleaseMark{Generation: view.Generation, Role: decided.Roles[0], Rows: []store.IdleReleaseRow{
			{Kind: "issue_bundle", Payload: []byte(`{"tree":"OTHER-8","generation":1,"treeGeneration":1,"role":"architect"}`)},
			{Kind: "supervise", Payload: []byte(`{"op":"release","tree":"OTHER-8","role":"architect","generation":1}`), Boundary: true},
			{Kind: "issue_release", Payload: []byte(`{"tree":"OTHER-8","generation":1,"treeGeneration":1}`)},
		}}, nil
	})
	if err != nil {
		t.Fatalf("decide the pod of %s: %v", issue, err)
	}
	if marked || decided.Hold != IdleHoldNoPod {
		t.Fatalf("the idle rule over the pod of %s = marked %t, hold %s; want no release and %s: OTHER's record is no record of this daemon's",
			issue, marked, decided.Hold, IdleHoldNoPod)
	}
	var rows, marks int
	if err := pool.QueryRow(ctx, `select (select count(*) from outbox where issue = $1),
		(select count(*) from issue_releases where issue = $1)`, issue).Scan(&rows, &marks); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || marks != 0 {
		t.Fatalf("after the idle rule's decision %d outbox rows are keyed %s and %d release marks name it, want none of either",
			rows, issue, marks)
	}
}

// Every hold the rule can report has a name of its own, which is what the daemon records of a pod
// it kept: two holds that printed the same would make the record unreadable.
func TestEveryIdleHoldHasANameOfItsOwn(t *testing.T) {
	t.Parallel()
	seen := map[string]IdleHold{}
	for hold := IdleHoldNone; hold <= IdleHoldUnownedWorkspaceUnread; hold++ {
		name := hold.String()
		if name == "unknown" {
			t.Fatalf("hold %d has no name", int(hold))
		}
		if other, taken := seen[name]; taken {
			t.Fatalf("holds %d and %d are both %q", int(other), int(hold), name)
		}
		seen[name] = hold
	}
	if got := IdleHold(int(IdleHoldUnownedWorkspaceUnread) + 1).String(); got != "unknown" {
		t.Fatalf("an unmapped hold prints %q, want unknown", got)
	}
}
