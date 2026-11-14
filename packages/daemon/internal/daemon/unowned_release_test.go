package daemon

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// stoppedImplementerFailures is the launch budget the failure spent. The release of a pod no
// role can use changes no claim, so it is read back unchanged after the pod is gone.
const stoppedImplementerFailures = 3

// stoppedImplementer leaves the rig's one live claim as the supervisor leaves a claim whose
// launch budget ran out: failed, keeping its session, the budget that failure spent and the task
// it was given, and holding no process in the pod. The pod then has no live claim at all, which
// is the whole of what the second release rule reads (workflow.UnownedPodRelease).
//
// keepsProcess leaves the locator on the row instead, the one shape in which a claim's state and
// its process disagree: a claim the supervisor has already failed whose process the runtime
// never confirmed gone (pods.IdleClaim.HasProcess).
//
// The write is backdated with the rest of the pod, since claims.updated_at is what the quiet
// period is measured from (pods.IdleView.QuietSince) and a row stamped now would make the pod
// busy again.
func (r *idleRig) stoppedImplementer(t *testing.T, keepsProcess bool) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), `update claims set state = 'failed',
		launch_failures = $2, locator = case when $3 then locator else null end,
		updated_at = now() - interval '30 minutes' where token = $1`,
		string(r.token), stoppedImplementerFailures, keepsProcess); err != nil {
		t.Fatalf("fail the implementer: %v", err)
	}
}

// wrote leaves one claim's row written minutes minutes ago. claims.updated_at is the pod's own
// record of when it last did anything, and the newest of its claims' is where the quiet period
// is measured from (pods.IdleView.QuietSince).
func (r *idleRig) wrote(t *testing.T, token claim.Token, minutes int) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), `update claims
		set updated_at = now() - make_interval(mins => $2) where token = $1`,
		string(token), minutes); err != nil {
		t.Fatalf("backdate %s: %v", token, err)
	}
}

// releaseMark is the kind of release the pod's mark records and the role it names
// (issue_releases): `unowned` with no role at all is the release of a pod no role can use, which
// has no live claim to stop and no role whose shim a bundle could be asked of.
func (r *idleRig) releaseMark(t *testing.T) (kind, role string) {
	t.Helper()
	if err := r.pool.QueryRow(context.Background(), `select kind, role from issue_releases where issue = $1`,
		r.issue.Key).Scan(&kind, &role); err != nil {
		t.Fatalf("read the release's mark: %v", err)
	}
	return kind, role
}

// storedClaim is one claim as the database holds it, which is where the budget a failure spent
// and the task a claim still holds are read back.
func (r *idleRig) storedClaim(t *testing.T, token claim.Token) supervise.Claim {
	t.Helper()
	claims, err := r.st.Claims(context.Background())
	if err != nil {
		t.Fatalf("read the stored claims: %v", err)
	}
	for _, c := range claims {
		if c.Token == token {
			return c
		}
	}
	t.Fatalf("no stored claim for %s", token)
	return supervise.Claim{}
}

// awaitedWake leaves the issue what an earlier release of its pod left behind: the backstop that
// release armed for the pod's phase worker, still open, and the mark of a wake that pod has
// already taken. Both belong to that release and to the pod it took.
func (r *idleRig) awaitedWake(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, `insert into pod_wake_backstops (project, issue, role, due_at)
		values ('legion', $1, 'implementer', now() + interval '55 minutes')`, r.issue.Key); err != nil {
		t.Fatalf("arm the earlier release's backstop: %v", err)
	}
	if _, err := r.pool.Exec(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at)
		values (0, 'legion', $1, 'implementer', $2, now())`,
		r.issue.Key, pods.WakeChecks); err != nil {
		t.Fatalf("seed the open resume the earlier release's wake left: %v", err)
	}
}

// podWakes is every open backstop of the issue and every resume no first turn has stamped and
// nothing has closed, each as one line, so a test can compare what a release found with what it
// left. A teardown that armed a backstop would have cancelled the open ones of the issue first
// (outbox.armBackstops), and the open resume is what tells the daemon a pod is already coming
// back for that role (migration 20261105183200, one open resume per role).
func (r *idleRig) podWakes(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	var lines []string
	backstops, err := r.pool.Query(ctx, `select id, role, fired from pod_wake_backstops
		where issue = $1 order by id`, r.issue.Key)
	if err != nil {
		t.Fatalf("read the pod's backstops: %v", err)
	}
	for backstops.Next() {
		var id int64
		var role string
		var fired bool
		if err := backstops.Scan(&id, &role, &fired); err != nil {
			backstops.Close()
			t.Fatalf("read the pod's backstops: %v", err)
		}
		lines = append(lines, fmt.Sprintf("backstop:%d:%s:fired=%t", id, role, fired))
	}
	backstops.Close()
	if err := backstops.Err(); err != nil {
		t.Fatalf("read the pod's backstops: %v", err)
	}
	resumes, err := r.pool.Query(ctx, `select role, kind from pod_resumes
		where issue = $1 and first_turn_at is null and closed_at is null order by role, kind`, r.issue.Key)
	if err != nil {
		t.Fatalf("read the pod's open resumes: %v", err)
	}
	defer resumes.Close()
	for resumes.Next() {
		var role, kind string
		if err := resumes.Scan(&role, &kind); err != nil {
			t.Fatalf("read the pod's open resumes: %v", err)
		}
		lines = append(lines, fmt.Sprintf("resume:%s:%s", role, kind))
	}
	if err := resumes.Err(); err != nil {
		t.Fatalf("read the pod's open resumes: %v", err)
	}
	return lines
}

// closedIssuesWithStoppedClaims seeds count issues of the project as a deployment's finished ones
// stand: closed, and each holding the retired claim of the role that worked it. An issue the
// workflow is done with keeps its claims for ever and every one of them is stopped, so the claim
// states alone would answer each of these as a pod no claim owns. They are seeded through the
// record store and the claim store the rig's own issue is seeded through, because what the
// candidate answer reads of them is exactly what those two write.
func (r *idleRig) closedIssuesWithStoppedClaims(t *testing.T, count int) {
	t.Helper()
	ctx := context.Background()
	for i := range count {
		key := fmt.Sprintf("LEGION-9%03d", i)
		putOutboxIssue(t, r.pool, r.issues, record.Issue{Key: key, Repo: "acme/widgets",
			DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: key,
			Title: "a finished issue", Phase: phase.Done, Generation: 1, Status: "done",
			Rank: "U", LastDispatchSeq: 1})
		token := mustClaimToken(t, key, claim.RoleImplementer)
		if err := r.st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: key,
			Issue: key, Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
			State: supervise.StateRetired, Session: "ses-" + key, SessionFile: "/tmp/" + key + ".jsonl",
			PluginContract: api.DaemonAPIVersion}); err != nil {
			t.Fatalf("seed the retired claim of %s: %v", key, err)
		}
	}
}

// countedSandboxReads is the rig's runtime with a counter on the one read the pass makes of it
// before it decides anything. The pass asks the runtime for the issue's Sandbox once per
// candidate and opens its store.ReleaseIdleIssue transaction only for a candidate whose Sandbox
// is still there (workflowRuntime.releaseUnownedPod), so this counts the candidates the pass
// decided. The teardown of a release that suspends no role reads the name once more, for the
// line it logs (outbox.issueSandboxName); that read belongs to a release already running, and
// none happens in a pass that releases nothing.
type countedSandboxReads struct {
	issuePods
	reads int
}

func (s *countedSandboxReads) IssueSandbox(issue string) (string, bool) {
	s.reads++
	return s.issuePods.IssueSandbox(issue)
}

// An issue whose every claim is failed, retired or suspended holds a Kubernetes pod of 3 CPU and
// 19 GiB that no role can take a turn in, and the second release rule takes it
// (workflow.UnownedPodRelease). That release is one act and one row, the teardown: it suspends no
// role, saves no bundle and changes no claim, because there is no live claim to stop, no shim
// left to build a bundle and no role coming back to a workspace that would need one.
//
// The four pods below are the four answers the rule gives a pod with no live claim in it: the one
// it takes, the one that is already gone, the one a process still runs in, and the one that holds
// a claim an earlier release left `released`.
func TestAnIssueWithNoLiveClaimHasItsSandboxReleased(t *testing.T) {
	ctx := context.Background()

	t.Run("the pod of an issue whose one claim failed", func(t *testing.T) {
		rig := newIdleRig(t)
		rig.stoppedImplementer(t, false)

		rig.w.releaseUnownedPods(ctx)

		if got := rig.rows(t); len(got) != 1 || got[0] != "issue_release" {
			t.Fatalf("queued rows = %v, want the teardown alone: this release saves no bundle and stops no role", got)
		}
		// The mark says which rule wrote it, and that kind is what the teardown and the cancel
		// read to tell what the release did rather than guess it from a missing role.
		if kind, role := rig.releaseMark(t); kind != "unowned" || role != "" {
			t.Fatalf("the mark = kind %q for role %q, want the unowned kind naming no role", kind, role)
		}

		rig.drain(t)

		// The claim is exactly as the failure left it: this release suspends nothing, so nothing
		// of the claim is the release's to write, and the task it holds is still owed to the role
		// that will run in whatever pod the issue is given next.
		stored := rig.storedClaim(t, rig.token)
		if stored.State != supervise.StateFailed || stored.Locator != nil {
			t.Fatalf("the claim = %s with locator %+v, want it left failed and holding no process",
				stored.State, stored.Locator)
		}
		if stored.Budgets.LaunchFailures != stoppedImplementerFailures {
			t.Fatalf("the claim's launch failures = %d, want the %d the failure spent",
				stored.Budgets.LaunchFailures, stoppedImplementerFailures)
		}
		if stored.Pending == nil || stored.Pending.ID != "outbox:7" || stored.Pending.Phase != phase.Implementing {
			t.Fatalf("the claim's task = %+v, want the task it was holding kept", stored.Pending)
		}
		bundled, suspended := rig.rt.reads()
		if suspended != 1 {
			t.Fatalf("Sandbox teardowns = %d, want the one this release is made of", suspended)
		}
		if len(bundled) != 0 {
			t.Fatalf("the bundle was asked of %v, want no role: no shim is left to build one", bundled)
		}
		if rig.bundled(t) {
			t.Fatal("the release saved a bundle, which no role of this pod is coming back for")
		}
		if rig.marked(t) {
			t.Fatal("the finished release left its mark behind")
		}
		if got := rig.rows(t); len(got) != 0 {
			t.Fatalf("rows left after the release = %v, want none", got)
		}
	})

	t.Run("the pod the runtime no longer holds a Sandbox for", func(t *testing.T) {
		rig := newIdleRig(t)
		rig.stoppedImplementer(t, false)
		// The pod is already gone, so nothing of it is reserved and there is nothing to release.
		rig.rt.sandboxGone = true

		// This rule changes no claim, so an issue it decides stands in the candidate answer for
		// ever and every later tick decides it again. The second pass must write no more than the
		// first: a release of a pod that is gone would be recorded once a minute for ever.
		rig.w.releaseUnownedPods(ctx)
		rig.w.releaseUnownedPods(ctx)
		rig.drain(t)

		if got := rig.rows(t); len(got) != 0 {
			t.Fatalf("queued rows = %v, want none: there is no pod to tear down", got)
		}
		if rig.marked(t) {
			t.Fatal("a pod the runtime does not hold was marked for release")
		}
		records := rig.records(t)
		if len(records.Releases) != 0 {
			t.Fatalf("releases recorded = %+v, want none: nothing was released", records.Releases)
		}
		if len(records.Holds) != 0 {
			t.Fatalf("holds recorded = %+v, want none: a pod that is gone is no pod this rule kept",
				records.Holds)
		}
		if _, suspended := rig.rt.reads(); suspended != 0 {
			t.Fatalf("Sandbox teardowns = %d, want none", suspended)
		}
	})

	t.Run("the pod a stopped claim still records a process in", func(t *testing.T) {
		rig := newIdleRig(t)
		// The supervisor has failed the claim and the row still carries the locator of the
		// process it was last launched as. The teardown's own fence refuses such a claim
		// (claimMayRun), so a release marked here would stand while its teardown waited for a
		// process nothing is going to stop.
		rig.stoppedImplementer(t, true)

		rig.w.releaseUnownedPods(ctx)

		if got := rig.rows(t); len(got) != 0 {
			t.Fatalf("queued rows = %v, want none: the pod still runs a process the daemon started", got)
		}
		if rig.marked(t) {
			t.Fatal("a pod a stopped claim still records a process in was marked for release")
		}
		// The locator reaches the rule through the store's own read of the claims row
		// (pods.IdleClaim.HasProcess), and the hold is recorded because such a pod stands past
		// the quiet period with 3 CPU and 19 GiB held by something that is not work.
		holds := rig.records(t).Holds
		if len(holds) != 1 || holds[0].Reason != workflow.IdleHoldUnownedClaimProcess.String() {
			t.Fatalf("holds = %+v, want the one %s", holds, workflow.IdleHoldUnownedClaimProcess)
		}
	})

	t.Run("the pod holding a claim an earlier release left released", func(t *testing.T) {
		rig := newIdleRig(t)
		architect := rig.releasedArchitect(t)
		rig.stoppedImplementer(t, false)
		// What that earlier release left owed: the backstop it armed over the pod it took, and
		// the open resume of the wake that pod is already coming back for. This teardown arms no
		// backstop of its own, which is the one act of the idle release it must not make — arming
		// cancels the issue's open backstops first, and the pod this teardown deletes is not the
		// pod that released claim is waiting to come back to.
		rig.awaitedWake(t)
		owed := rig.podWakes(t)
		if len(owed) != 2 {
			t.Fatalf("the pod was left owing %v, want the earlier release's backstop and open resume", owed)
		}

		rig.w.releaseUnownedPods(ctx)
		if got := rig.rows(t); len(got) != 1 || got[0] != "issue_release" {
			t.Fatalf("queued rows = %v, want the teardown alone: the released architect is already stopped", got)
		}

		rig.drain(t)

		if _, suspended := rig.rt.reads(); suspended != 1 {
			t.Fatalf("Sandbox teardowns = %d, want the one this release is made of", suspended)
		}
		if got := rig.podWakes(t); !slices.Equal(got, owed) {
			t.Fatalf("the pod's wakes after the release = %v, want the earlier release's %v left as they were", got, owed)
		}
		if got := architect.Claim(); !got.Released() {
			t.Fatalf("the architect = %s by %s, want the release that touched no claim to have left it released",
				got.State, got.SuspendedBy)
		}
	})
}

// The quiet period is the last conjunct of the rule, and it reads the newest write of any claim
// of the pod (pods.IdleView.QuietSince): a pod that recorded something inside the period is one
// this rule is not yet entitled to, and the pass neither releases it nor records a hold of it,
// since nothing was kept — it is simply not this rule's pod yet.
//
// The four rows run on one rig, and in this order because the last of them writes the release's
// mark: a mark standing is a state no later row could read around, so the three readings that
// keep the pod come first and the release that takes it is last.
func TestTheQuietPeriodBeforeAnIssueWithNoLiveClaimIsReleased(t *testing.T) {
	ctx := context.Background()
	rig := newIdleRig(t)
	rig.stoppedImplementer(t, false)
	rig.releasedArchitect(t)
	architect := mustClaimToken(t, rig.issue.Key, claim.RoleArchitect)

	quiet := func(t *testing.T, why string) {
		t.Helper()
		if got := rig.rows(t); len(got) != 0 {
			t.Fatalf("queued rows = %v, want none: %s", got, why)
		}
		if rig.marked(t) {
			t.Fatalf("the pod was marked for release: %s", why)
		}
	}

	t.Run("the pod recorded something inside the quiet period", func(t *testing.T) {
		rig.wrote(t, rig.token, 2)
		rig.wrote(t, architect, 2)

		rig.w.releaseUnownedPods(ctx)

		quiet(t, "the pod recorded something 2 minutes ago and the period is 10")
		if holds := rig.records(t).Holds; len(holds) != 0 {
			t.Fatalf("holds = %+v, want none: a pod inside the period is not one this rule kept", holds)
		}
	})

	t.Run("the newest of two claim writes is inside the period and the older is past it", func(t *testing.T) {
		rig.wrote(t, architect, 20)
		rig.wrote(t, rig.token, 2)

		rig.w.releaseUnownedPods(ctx)

		quiet(t, "the newest write of any claim of the pod decides, and that one is 2 minutes old")
		if holds := rig.records(t).Holds; len(holds) != 0 {
			t.Fatalf("holds = %+v, want none: a pod inside the period is not one this rule kept", holds)
		}
	})

	t.Run("release is off", func(t *testing.T) {
		rig.wrote(t, rig.token, 11)
		rig.wrote(t, architect, 11)
		// `idle_minutes: 0` is how a deployment turns release off, and one period carries both
		// rules: the tick runs no ticker at all, so neither pass ever decides this pod, which the
		// row below shows the rule would otherwise release.
		period := rig.w.idlePeriod
		rig.w.idlePeriod = 0
		defer func() { rig.w.idlePeriod = period }()

		done := make(chan struct{})
		go func() { defer close(done); rig.w.tickIdle(ctx) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("tickIdle kept running with release off")
		}

		quiet(t, "release is off")
	})

	t.Run("the whole pod has been quiet for the period", func(t *testing.T) {
		rig.wrote(t, rig.token, 11)
		rig.wrote(t, architect, 11)

		rig.w.releaseUnownedPods(ctx)

		if got := rig.rows(t); len(got) != 1 || got[0] != "issue_release" {
			t.Fatalf("queued rows = %v, want the teardown: the pod has recorded nothing for 11 minutes", got)
		}
		if !rig.marked(t) {
			t.Fatal("the release left no mark")
		}
	})
}

// The candidate answer is what bounds the work of the pass: it opens one store.ReleaseIdleIssue
// transaction for each issue it names, and that transaction begins by taking the daemon's global
// fact serializer — the lock the intake takes for every fact — so a tick of candidates is a tick
// of facts not applied. The claim states alone would name every issue of the deployment whose
// roles ever stopped, which is nearly every issue it has ever closed; the answer names only an
// issue whose own row and whose tree root's row both still stand in the workflow.
//
// The pass's own economy rests on the same reading twice over. It asks the runtime for the
// issue's Sandbox before the transaction, exactly as the idle pass reads the pod's image before
// its own, and a Sandbox the runtime no longer holds ends the pass there: this rule changes no
// claim, so an issue it has decided stands in the answer for ever, and deciding a gone pod inside
// a transaction would cost one of them a minute for ever.
func TestThePassDecidesOnlyTheIssuesTheRuleCouldRelease(t *testing.T) {
	ctx := context.Background()
	rig := newIdleRig(t)
	rig.stoppedImplementer(t, false)
	const closed = 200
	rig.closedIssuesWithStoppedClaims(t, closed)

	candidates, err := rig.st.UnownedPods(ctx, rig.w.dispatchProject, rig.w.projectID, record.OutOfWorkflowStatuses)
	if err != nil {
		t.Fatalf("read the pods of the project no claim owns: %v", err)
	}
	if len(candidates) != 1 || candidates[0] != rig.issue.Key {
		t.Fatalf("candidates = %d issues, beginning %v; want the one issue of %d whose pod the rule could release",
			len(candidates), candidates[:min(len(candidates), 3)], closed+1)
	}

	// The second reading, on that one candidate: its Sandbox is gone, so the pass decides it
	// outside any transaction. The issue is releasable in every other respect, and the rule inside
	// the transaction takes the Sandbox read as answered — it is handed `true` rather than asking
	// the runtime again under the transaction's lock — so a transaction opened here would have
	// marked the release and queued its teardown. No mark, no row and no record is that
	// transaction never opening, and the counter below is the pass reaching that candidate at all.
	reads := &countedSandboxReads{issuePods: rig.rt}
	rig.w.idleRuntime = reads
	rig.rt.sandboxGone = true

	rig.w.releaseUnownedPods(ctx)

	if reads.reads != 1 {
		t.Fatalf("the runtime was asked for %d Sandboxes, want the one candidate's", reads.reads)
	}
	if got := rig.rows(t); len(got) != 0 {
		t.Fatalf("queued rows = %v, want none: no transaction was opened for a pod that is gone", got)
	}
	if rig.marked(t) {
		t.Fatal("a release was marked for a pod the runtime does not hold, so the pass decided it in a transaction")
	}
	records := rig.records(t)
	if len(records.Releases) != 0 || len(records.Holds) != 0 {
		t.Fatalf("records = %+v / %+v, want neither: the pass wrote nothing for a pod that is gone",
			records.Releases, records.Holds)
	}
}
