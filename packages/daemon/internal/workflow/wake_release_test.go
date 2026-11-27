package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// These two tests are a check of their own (dispatch://INFRA-3589, check l2), and its negative
// control runs them at the commit before that change. So they name only what that commit has:
// the retry's delay and limit are literal durations here, and the log lines string literals.

// releaseStore is the store over the rig's pool, for the release's own writes: the mark
// (ReleaseIdleIssue), a role stop's record (ReleaseSuspendedClaim) and the cancel
// (CancelIssueRelease).
func releaseStore(t *testing.T, pool *pgxpool.Pool) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store over the test pool: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// markRelease marks an idle release of LEGION-208's pod as the idle tick does, with the
// implementer's stop as its boundary and the teardown after it, and answers the boundary row: the
// row the stop that suspends the implementer runs as.
func markRelease(t *testing.T, st *store.Store) int64 {
	t.Helper()
	ctx := context.Background()
	project := wakeProjectToken(t)
	marked, err := st.ReleaseIdleIssue(ctx, project, "LEGION-208", func(pods.IdleView) (store.IdleReleaseMark, error) {
		return store.IdleReleaseMark{Generation: wakeIssueGeneration, Role: claim.RoleImplementer, Rows: []store.IdleReleaseRow{
			{Kind: "supervise", Payload: []byte(`{"op":"release","tree":"LEGION-208","role":"implementer","generation":7}`), Boundary: true},
			{Kind: "issue_release", Payload: []byte(`{"tree":"LEGION-208","generation":7,"treeGeneration":7}`)},
		}}, nil
	})
	if err != nil || !marked {
		t.Fatalf("ReleaseIdleIssue = %t, %v; want the release marked", marked, err)
	}
	var boundary int64
	if err := st.Pool().QueryRow(ctx, `select boundary_row from issue_releases where project = $1 and issue = 'LEGION-208'`,
		project).Scan(&boundary); err != nil {
		t.Fatalf("read the release's mark: %v", err)
	}
	return boundary
}

// markTwoRoleRelease marks an idle release of LEGION-208's pod that stops two roles: the
// implementer, whose stop is the boundary, and then the architect, with the teardown after both.
// It answers the two stops' rows and the teardown's.
func markTwoRoleRelease(t *testing.T, st *store.Store) (implementer, architect, teardown int64) {
	t.Helper()
	ctx := context.Background()
	project := wakeProjectToken(t)
	marked, err := st.ReleaseIdleIssue(ctx, project, "LEGION-208", func(pods.IdleView) (store.IdleReleaseMark, error) {
		return store.IdleReleaseMark{Generation: wakeIssueGeneration, Role: claim.RoleImplementer, Rows: []store.IdleReleaseRow{
			{Kind: "supervise", Payload: []byte(`{"op":"release","tree":"LEGION-208","role":"implementer","generation":7}`), Boundary: true},
			{Kind: "supervise", Payload: []byte(`{"op":"release","tree":"LEGION-208","role":"architect","generation":7}`)},
			{Kind: "issue_release", Payload: []byte(`{"tree":"LEGION-208","generation":7,"treeGeneration":7}`)},
		}}, nil
	})
	if err != nil || !marked {
		t.Fatalf("ReleaseIdleIssue = %t, %v; want the release marked", marked, err)
	}
	if err := st.Pool().QueryRow(ctx, `select boundary_row, last_row from issue_releases where project = $1 and issue = 'LEGION-208'`,
		project).Scan(&implementer, &teardown); err != nil {
		t.Fatalf("read the release's mark: %v", err)
	}
	if err := st.Pool().QueryRow(ctx, `select id from outbox where issue = 'LEGION-208' and kind = 'supervise'
		and payload->>'op' = 'release' and payload->>'role' = 'architect'`).Scan(&architect); err != nil {
		t.Fatalf("read the architect's stop: %v", err)
	}
	return implementer, architect, teardown
}

// stoppedByTheRelease is one role stop of the release run to its end, as the outbox runs it: the
// claim of role suspended as `released`, the stop's record of it on the mark, and the stop's row
// gone from the outbox, as a finished row is.
func stoppedByTheRelease(t *testing.T, pool *pgxpool.Pool, st *store.Store, role claim.Role, row int64) {
	t.Helper()
	setClaimState(t, pool, role, "suspended", "released")
	if err := st.ReleaseSuspendedClaim(context.Background(), wakeProjectToken(t), "LEGION-208", role, row); err != nil {
		t.Fatalf("record the %s claim the release suspended: %v", role, err)
	}
	if _, err := pool.Exec(context.Background(), `delete from outbox where id = $1`, row); err != nil {
		t.Fatalf("finish the %s's stop: %v", role, err)
	}
}

// startDue is whether a `start` of LEGION-208 is among the rows the issue's lifecycle lane may run
// now (record.Postgres.DueRows), which is what decides whether the outbox runs it.
func startDue(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	due, err := record.NewStore().DueRows(ctx, tx, "LEGION", "LEGION-208", record.LaneKinds(record.LaneLifecycle), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("list the lifecycle lane's due rows: %v", err)
	}
	for _, row := range due {
		var request record.SuperviseRequest
		if row.Kind == record.OutboxKindSupervise && json.Unmarshal(row.Payload, &request) == nil && request.Op == "start" {
			return true
		}
	}
	return false
}

// setClaimState puts the claim of LEGION-208's role in state, suspended by source ("" for a
// claim that is not suspended).
func setClaimState(t *testing.T, pool *pgxpool.Pool, role claim.Role, state, source string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `update claims set state = $2, suspended_by = $3 where token = $1`,
		string(wakeToken(t, role)), state, source); err != nil {
		t.Fatalf("set the %s claim %s: %v", role, state, err)
	}
}

// applyWake applies one listener report as the intake does, and answers its error: the rig's own
// apply fails the test on an error, and a retry is an error.
func applyWake(rig wakeRig, id string, fact intake.Fact) error {
	_, err := intake.ApplyFact(context.Background(), rig.pool, "api", id, fact, rig.engine, admissionStub{})
	return err
}

// processedEvent is whether the intake recorded event id as applied.
func processedEvent(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var processed bool
	if err := pool.QueryRow(context.Background(), `select exists (select 1 from processed_events where event_id = $1)`,
		id).Scan(&processed); err != nil {
		t.Fatalf("read processed_events: %v", err)
	}
	return processed
}

// loggedLines is every line the engine logged at level that holds message.
func loggedLines(rig wakeRig, level, message string) []string {
	var lines []string
	for line := range strings.SplitSeq(rig.logged.String(), "\n") {
		if strings.Contains(line, "level="+level) && strings.Contains(line, message) {
			lines = append(lines, line)
		}
	}
	return lines
}

// R3: a wake report that comes while a release has stopped the role's process and has not yet
// recorded the claim as released is applied again every 30 seconds until the release ends, for
// at most 15 minutes after the report. A cancelled release's queued start brings the role back
// with no wake of its own, and a wake for a claim in no release is applied once. A wake taken
// minutes after its report records the take's time as the resume's time, so the stuck rule does
// not count the wait.
func TestAWakeDuringAReleaseIsAppliedAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		kind string
		// fact is the report, and name what the woken role's task must name.
		fact func(t *testing.T) (intake.ListenerWake, string)
	}{
		{name: "(a) an agent wake before the release records the claim", kind: pods.WakeAgentMessage,
			fact: func(*testing.T) (intake.ListenerWake, string) {
				return intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-agent"}, "session-implementer"
			}},
		{name: "(b) a role wake before the release records the claim", kind: pods.WakeRoleMessage,
			fact: func(t *testing.T) (intake.ListenerWake, string) {
				token := string(wakeToken(t, claim.RoleImplementer))
				return intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: token, At: wakeAt, Report: "report-role"}, token
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWakeRig(t)
			st := releaseStore(t, rig.pool)
			fact, name := tc.fact(t)
			// The claim stands live while its stop runs: the release has stopped the process and
			// has not recorded the claim as released.
			setClaimState(t, rig.pool, claim.RoleImplementer, "idle", "")
			boundary := markRelease(t, st)

			err := applyWake(rig, "wake", fact)
			var later *intake.RetryLater
			if !errors.As(err, &later) || later.After != 30*time.Second {
				t.Fatalf("applying the wake during the release = %v, want a retry after 30s: the wake finds no released claim yet, and dropping it leaves the pod released", err)
			}
			if processedEvent(t, rig.pool, "wake") {
				t.Fatal("the retried wake is recorded as applied, want it unrecorded so its redelivery applies")
			}
			if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
				t.Fatalf("supervise start rows = %+v, want none while the wake waits", starts)
			}
			if resumes := wakeResumes(t, rig.pool); len(resumes) != 0 {
				t.Fatalf("pod resumes = %v, want none while the wake waits", resumes)
			}

			// The release records the claim as released, and the wake is delivered again.
			setClaimState(t, rig.pool, claim.RoleImplementer, "suspended", "released")
			if err := st.ReleaseSuspendedClaim(context.Background(), wakeProjectToken(t), "LEGION-208", claim.RoleImplementer, boundary); err != nil {
				t.Fatalf("record the claim the release suspended: %v", err)
			}
			if err := applyWake(rig, "wake", fact); err != nil {
				t.Fatalf("applying the redelivered wake = %v, want it applied", err)
			}
			oneWake(t, rig.pool, claim.RoleImplementer, tc.kind, name)
		})
	}

	t.Run("(c) a wake after a cancelled release takes nothing", func(t *testing.T) {
		rig := newWakeRig(t)
		st := releaseStore(t, rig.pool)
		ctx := context.Background()
		project := wakeProjectToken(t)
		// The release has recorded the implementer as released, and is then cancelled before the
		// start its cancel queued runs.
		boundary := markRelease(t, st)
		if err := st.ReleaseSuspendedClaim(ctx, project, "LEGION-208", claim.RoleImplementer, boundary); err != nil {
			t.Fatalf("record the claim the release suspended: %v", err)
		}
		resume := func(role claim.Role, generation uint64, _ store.DroppedWake) (store.IdleReleaseRow, error) {
			row, err := record.NewOutboxRow("LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208",
				Role: role, Generation: generation}, time.Now())
			if err != nil {
				return store.IdleReleaseRow{}, err
			}
			return store.IdleReleaseRow{Kind: string(row.Kind), Payload: row.Payload}, nil
		}
		cancelled, err := st.CancelIssueRelease(ctx, project, "LEGION-208", resume, record.OutOfWorkflow, store.ReleaseCancelPause{At: time.Now()})
		if resumed := cancelled.Resumed; err != nil || !slices.Equal(resumed, []claim.Role{claim.RoleImplementer}) {
			t.Fatalf("CancelIssueRelease = %v, %v; want the implementer's resume queued", resumed, err)
		}
		var state, source string
		if err := rig.pool.QueryRow(ctx, `select state, suspended_by from claims where token = $1`,
			string(wakeToken(t, claim.RoleImplementer))).Scan(&state, &source); err != nil {
			t.Fatalf("read the implementer's claim: %v", err)
		}
		if state != "suspended" || source != "released" {
			t.Fatalf("the implementer's claim after the cancel = %s by %q, want suspended by released until its start runs", state, source)
		}

		fact := intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-agent"}
		if err := applyWake(rig, "wake", fact); err != nil {
			t.Fatalf("applying the wake after the cancel = %v, want it applied", err)
		}
		starts := wakeStarts(t, rig.pool)
		if len(starts) != 1 || starts[0] != (wakeStart{Role: claim.RoleImplementer}) {
			t.Fatalf("supervise start rows = %+v, want the cancel's one start of the implementer with no task", starts)
		}
		if resumes := wakeResumes(t, rig.pool); len(resumes) != 0 {
			t.Fatalf("pod resumes = %v, want none: the pod never went away", resumes)
		}
		const coming = "wake: the role is coming back on a start already queued; this wake takes nothing"
		if lines := loggedLines(rig, "INFO", coming); len(lines) != 1 || !strings.Contains(lines[0], "issue=LEGION-208") {
			t.Fatalf("INFO lines %q = %q, want one naming LEGION-208; log:\n%s", coming, lines, rig.logged.String())
		}
	})

	t.Run("(d) a release that has not ended after 15 minutes drops the wake", func(t *testing.T) {
		rig := newWakeRig(t)
		st := releaseStore(t, rig.pool)
		setClaimState(t, rig.pool, claim.RoleImplementer, "idle", "")
		markRelease(t, st)

		at := wakeAt.Add(-(15*time.Minute + time.Second))
		fact := intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: at, Report: "report-agent"}
		if err := applyWake(rig, "wake", fact); err != nil {
			t.Fatalf("applying the wake 15 minutes and 1 second after its report = %v, want it dropped with no retry", err)
		}
		const dropped = "wake: a wake that came during a release was dropped: the release did not end within 15 minutes of the report"
		if lines := loggedLines(rig, "WARN", dropped); len(lines) != 1 || !strings.Contains(lines[0], "issue=LEGION-208") {
			t.Fatalf("WARN lines %q = %q, want one naming LEGION-208; log:\n%s", dropped, lines, rig.logged.String())
		}
		if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
			t.Fatalf("supervise start rows = %+v, want none for a dropped wake", starts)
		}
		if resumes := wakeResumes(t, rig.pool); len(resumes) != 0 {
			t.Fatalf("pod resumes = %v, want none for a dropped wake", resumes)
		}
	})

	t.Run("(e) a wake for a claim in no release does nothing once", func(t *testing.T) {
		rig := newWakeRig(t)
		setClaimState(t, rig.pool, claim.RoleImplementer, "idle", "")

		fact := intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-agent"}
		if err := applyWake(rig, "wake", fact); err != nil {
			t.Fatalf("applying a wake for a live claim in no release = %v, want it applied once with no retry", err)
		}
		if !processedEvent(t, rig.pool, "wake") {
			t.Fatal("the wake is not recorded as applied, want it applied once")
		}
		if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
			t.Fatalf("supervise start rows = %+v, want none for a live claim", starts)
		}
		if resumes := wakeResumes(t, rig.pool); len(resumes) != 0 {
			t.Fatalf("pod resumes = %v, want none for a live claim", resumes)
		}
	})

	// (k) A release whose workspace check declines it ends at the decline, which cancels it before
	// any role stops (store.Store.DeclineIssueRelease). A wake report held while that release stood
	// is applied at its next delivery, which comes at the bound, where a release still standing
	// drops it: it finds the claim live in no release and does nothing more, as (e) does, with no
	// retry and no warning that drops it. Its control is the first delivery, made while the release
	// stands, which is retried.
	t.Run("(k) a declined release ends, and the wake it held is applied", func(t *testing.T) {
		rig := newWakeRig(t)
		st := releaseStore(t, rig.pool)
		ctx := context.Background()
		project := wakeProjectToken(t)
		setClaimState(t, rig.pool, claim.RoleImplementer, "idle", "")
		markRelease(t, st)

		fact := intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-agent"}
		var later *intake.RetryLater
		if err := applyWake(rig, "wake", fact); !errors.As(err, &later) {
			t.Fatalf("applying the wake while the release stands = %v, want a retry", err)
		}
		resume := func(role claim.Role, generation uint64, _ store.DroppedWake) (store.IdleReleaseRow, error) {
			row, err := record.NewOutboxRow("LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208",
				Role: role, Generation: generation}, time.Now())
			if err != nil {
				return store.IdleReleaseRow{}, err
			}
			return store.IdleReleaseRow{Kind: string(row.Kind), Payload: row.Payload}, nil
		}
		declined, err := st.DeclineIssueRelease(ctx, project, "LEGION-208", store.IdleReleaseDecline{At: time.Now(),
			Role: claim.RoleImplementer, Reason: pods.DeclineUnpushedWork, Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", Commits: 1},
			resume, record.OutOfWorkflow, store.ReleaseCancelPause{At: time.Now()})
		if err != nil || !declined.Declined || len(declined.Resumed) != 0 {
			t.Fatalf("DeclineIssueRelease = %+v, %v; want the decline recorded and no resume: no role had stopped", declined, err)
		}

		// The next delivery comes at the bound, where a release still standing drops the wake: only
		// the decline's end of the release keeps it.
		rig.clock.now = wakeAt.Add(WakeHeldForRelease)
		if err := applyWake(rig, "wake", fact); err != nil {
			t.Fatalf("applying the held wake after the decline = %v, want it applied with no retry", err)
		}
		if !processedEvent(t, rig.pool, "wake") {
			t.Fatal("the held wake is not recorded as applied after the decline, want it applied once")
		}
		if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
			t.Fatalf("supervise start rows = %+v, want none: the declined release stopped no role", starts)
		}
		if resumes := wakeResumes(t, rig.pool); len(resumes) != 0 {
			t.Fatalf("pod resumes = %v, want none: the pod never went away", resumes)
		}
		const dropped = "wake: a wake that came during a release was dropped"
		if lines := loggedLines(rig, "WARN", dropped); len(lines) != 0 {
			t.Fatalf("WARN lines %q = %q, want none: the decline ended the release, so the wake was not dropped", dropped, lines)
		}
	})

	// (k2) The workspace check's first attempt fails, and the role stop of the same pass runs: the
	// implementer's claim is suspended as `released` and recorded on the mark. A later attempt of
	// the check declines the release, and the decline queues the stopped role's resume, as a
	// cancel does (store.Store.DeclineIssueRelease). A wake report held while the release stood is
	// applied at its next delivery after the decline: it counts the role as coming back on that
	// queued start and takes no second start, as (c) does. One start in all. Its controls are the
	// first delivery, made while the release stands, which is retried, and the first attempt's
	// decline, which fails and leaves the mark standing, so the role stop still acts.
	t.Run("(k2) a decline after a failed first check resumes the stopped role, and the wake it held takes no second start", func(t *testing.T) {
		rig := newWakeRig(t)
		st := releaseStore(t, rig.pool)
		ctx := context.Background()
		project := wakeProjectToken(t)
		setClaimState(t, rig.pool, claim.RoleImplementer, "idle", "")
		boundary := markRelease(t, st)

		fact := intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-agent"}
		var later *intake.RetryLater
		if err := applyWake(rig, "wake", fact); !errors.As(err, &later) {
			t.Fatalf("applying the wake while the release stands = %v, want a retry", err)
		}
		resume := func(role claim.Role, generation uint64, _ store.DroppedWake) (store.IdleReleaseRow, error) {
			row, err := record.NewOutboxRow("LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208",
				Role: role, Generation: generation}, time.Now())
			if err != nil {
				return store.IdleReleaseRow{}, err
			}
			return store.IdleReleaseRow{Kind: string(row.Kind), Payload: row.Payload}, nil
		}
		decline := store.IdleReleaseDecline{At: time.Now(), Role: claim.RoleImplementer, Reason: pods.DeclineUnpushedWork,
			Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", Commits: 1}

		// The check's first attempt fails: its decline's transaction does not begin, so it writes
		// nothing and the mark stands.
		ended, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := st.DeclineIssueRelease(ended, project, "LEGION-208", decline, resume, record.OutOfWorkflow,
			store.ReleaseCancelPause{At: time.Now()}); err == nil {
			t.Fatal("the first attempt's DeclineIssueRelease under an ended context = nil, want it to fail")
		}
		stop, err := st.IssueReleaseStop(ctx, project, "LEGION-208", boundary)
		if err != nil || stop.Act != store.ReleaseActs {
			t.Fatalf("the implementer's stop after the check's failed attempt = %s for %s, %v; want it to act: the mark stands",
				stop.Act, stop.Cause, err)
		}
		stoppedByTheRelease(t, rig.pool, st, claim.RoleImplementer, boundary)

		declined, err := st.DeclineIssueRelease(ctx, project, "LEGION-208", decline, resume, record.OutOfWorkflow,
			store.ReleaseCancelPause{At: time.Now()})
		if err != nil || !declined.Declined || !slices.Equal(declined.Resumed, []claim.Role{claim.RoleImplementer}) {
			t.Fatalf("the later attempt's DeclineIssueRelease = %+v, %v; want the decline recorded and the stopped implementer's resume queued",
				declined, err)
		}
		if starts := wakeStarts(t, rig.pool); len(starts) != 1 || starts[0] != (wakeStart{Role: claim.RoleImplementer}) {
			t.Fatalf("supervise start rows after the decline = %+v, want the decline's one start of the implementer with no task", starts)
		}

		if err := applyWake(rig, "wake", fact); err != nil {
			t.Fatalf("applying the held wake after the decline = %v, want it applied with no retry", err)
		}
		if !processedEvent(t, rig.pool, "wake") {
			t.Fatal("the held wake is not recorded as applied after the decline, want it applied once")
		}
		if starts := wakeStarts(t, rig.pool); len(starts) != 1 || starts[0] != (wakeStart{Role: claim.RoleImplementer}) {
			t.Fatalf("supervise start rows = %+v, want one start in all: the decline's start of the implementer with no task, and none of the wake", starts)
		}
		if resumes := wakeResumes(t, rig.pool); len(resumes) != 0 {
			t.Fatalf("pod resumes = %v, want none: the pod never went away", resumes)
		}
		const coming = "wake: the role is coming back on a start already queued; this wake takes nothing"
		if lines := loggedLines(rig, "INFO", coming); len(lines) != 1 || !strings.Contains(lines[0], "issue=LEGION-208") {
			t.Fatalf("INFO lines %q = %q, want one naming LEGION-208; log:\n%s", coming, lines, rig.logged.String())
		}
	})

	// (f) Fix B: a wake taken after the release recorded the implementer's claim as released,
	// and before the release's teardown, does not cancel the release. The architect's stop and
	// the teardown go on, the wake's start waits for the teardown in the issue's lifecycle lane,
	// and the role starts once, in the new pod, with a task that says truly that its pod was
	// released and brought back. Its one resume is recorded against that release. Each wake the
	// workflow takes inside a fact's transaction can land in this window.
	t.Run("(f) a wake taken after the release records the claim waits for the teardown", func(t *testing.T) {
		for _, wake := range []struct {
			name string
			kind string
			// take applies the wake as the intake does and answers the detail its task names.
			take func(t *testing.T, rig wakeRig) string
		}{
			{name: "an agent wake", kind: pods.WakeAgentMessage, take: func(t *testing.T, rig wakeRig) string {
				fact := intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-agent"}
				if err := applyWake(rig, "wake", fact); err != nil {
					t.Fatalf("applying the wake after the release recorded the claim = %v, want it applied", err)
				}
				return "session-implementer"
			}},
			{name: "a CI settlement", kind: pods.WakeChecks, take: func(t *testing.T, rig wakeRig) string {
				fact := settlement("head1", "build")
				rig.apply("settled", fact)
				return settlementDetail(fact)
			}},
			{name: "the backstop", kind: pods.WakeBackstop, take: func(t *testing.T, rig wakeRig) string {
				id := armBackstop(t, rig.pool, claim.RoleImplementer, wakeAt.Add(-time.Minute))
				rig.apply("backstop", intake.PodBackstopDue{ID: id, Issue: "LEGION-208", Role: claim.RoleImplementer})
				return "the backstop armed for LEGION-208's implementer came due"
			}},
		} {
			t.Run(wake.name, func(t *testing.T) {
				rig := newWakeRig(t)
				st := releaseStore(t, rig.pool)
				ctx := context.Background()
				project := wakeProjectToken(t)
				setClaimState(t, rig.pool, claim.RoleImplementer, "idle", "")
				setClaimState(t, rig.pool, claim.RoleArchitect, "idle", "")
				implementer, architect, teardown := markTwoRoleRelease(t, st)
				stoppedByTheRelease(t, rig.pool, st, claim.RoleImplementer, implementer)

				detail := wake.take(t, rig)

				stop, err := st.IssueReleaseStop(ctx, project, "LEGION-208", architect)
				if err != nil || stop.Act != store.ReleaseActs {
					t.Fatalf("the architect's stop after the wake = %s for %s, %v; want it to act: the wake is of a role the release recorded",
						stop.Act, stop.Cause, err)
				}
				stoppedByTheRelease(t, rig.pool, st, claim.RoleArchitect, architect)
				decision, err := st.IssueRelease(ctx, project, store.IssueReleased{Issue: "LEGION-208", Tree: "LEGION-208",
					IssueGeneration: wakeIssueGeneration, TreeGeneration: wakeIssueGeneration, Row: teardown}, record.OutOfWorkflow)
				if err != nil || decision.Act != store.ReleaseActs {
					t.Fatalf("the teardown after the wake = %s for %s, %v; want it to act: the wake is of a role the release recorded",
						decision.Act, decision.Cause, err)
				}
				if startDue(t, rig.pool) {
					t.Fatal("the wake's start is due while the teardown row is in the outbox, want it held: it would start the role in the pod the teardown takes")
				}

				// The release finishes as the outbox's teardown finishes it (releaseIssue): the
				// release recorded with the roles the mark suspended, then the mark cleared, then
				// the row finished.
				roles := make([]string, 0, len(decision.Suspended))
				for _, role := range decision.Suspended {
					roles = append(roles, string(role))
				}
				slices.Sort(roles)
				releasedAt := wakeAt.Add(5 * time.Minute)
				released, _, err := st.RecordPodRelease(ctx, store.PodRelease{Project: project, Issue: "LEGION-208",
					Generation: wakeIssueGeneration, Roles: roles, At: releasedAt})
				if err != nil {
					t.Fatalf("record the release: %v", err)
				}
				if err := st.ClearIssueRelease(ctx, project, "LEGION-208"); err != nil {
					t.Fatalf("clear the release's mark: %v", err)
				}
				if _, err := rig.pool.Exec(ctx, `delete from outbox where id = $1`, teardown); err != nil {
					t.Fatalf("finish the teardown: %v", err)
				}
				if !startDue(t, rig.pool) {
					t.Fatal("the wake's start is not due once the teardown has finished, want it due: it starts the role in the new pod")
				}

				starts := wakeStarts(t, rig.pool)
				if want := WakeTask(wake.kind, detail, StoppedByRelease); len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].Task != want {
					t.Fatalf("supervise start rows = %+v, want the one start of the implementer with the task %q", starts, want)
				}
				const preamble = "Your issue's pod had been released while it was idle, and this wake brought it back. "
				if !strings.HasPrefix(starts[0].Task, preamble) {
					t.Fatalf("the implementer's task = %q, want it to begin %q: its pod was released", starts[0].Task, preamble)
				}
				var resumes int
				var release, seen int64
				var at time.Time
				if err := rig.pool.QueryRow(ctx, `select count(*) over (), release_id, seen_release, at from pod_resumes
					where issue = 'LEGION-208'`).Scan(&resumes, &release, &seen, &at); err != nil {
					t.Fatalf("read the pod resumes: %v", err)
				}
				if resumes != 1 || release != released || seen != released || at.Before(releasedAt) {
					t.Fatalf("pod resumes = %d, the resume's release %d, seen release %d and time %s; want one resume of release %d, "+
						"timed no earlier than that release's %s", resumes, release, seen, at.UTC().Format(time.RFC3339),
						released, releasedAt.Format(time.RFC3339))
				}
			})
		}
	})

	t.Run("(j) a wake taken minutes after its report counts its resume from the take", func(t *testing.T) {
		rig := newWakeRig(t)
		st := releaseStore(t, rig.pool)
		ctx := context.Background()
		project := wakeProjectToken(t)
		setClaimState(t, rig.pool, claim.RoleImplementer, "idle", "")
		boundary := markRelease(t, st)

		// The report was made 9 minutes and 30 seconds before the engine's clock (wakeAt), and
		// it waited for the release in that time.
		report := wakeAt.Add(-(9*time.Minute + 30*time.Second))
		fact := intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: report, Report: "report-agent"}
		var later *intake.RetryLater
		if err := applyWake(rig, "wake", fact); !errors.As(err, &later) {
			t.Fatalf("applying the wake during the release = %v, want a retry: the release has not recorded the claim", err)
		}
		setClaimState(t, rig.pool, claim.RoleImplementer, "suspended", "released")
		if err := st.ReleaseSuspendedClaim(ctx, project, "LEGION-208", claim.RoleImplementer, boundary); err != nil {
			t.Fatalf("record the claim the release suspended: %v", err)
		}
		if err := applyWake(rig, "wake", fact); err != nil {
			t.Fatalf("applying the redelivered wake = %v, want it applied", err)
		}
		oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeAgentMessage, "session-implementer")

		var at time.Time
		if err := rig.pool.QueryRow(ctx, `select at from pod_resumes where issue = 'LEGION-208'`).Scan(&at); err != nil {
			t.Fatalf("read the resume's time: %v", err)
		}
		if !at.Equal(wakeAt) {
			t.Fatalf("the resume's time = %s, want the take's time %s, not the report's time %s",
				at.UTC().Format(time.RFC3339), wakeAt.Format(time.RFC3339), report.Format(time.RFC3339))
		}
		// A stuck pass at a moment reads each resume taken StrandedReleaseWake or more before that
		// moment (internal/daemon's warnStuckResumes). One minute after the take, the pod is still
		// being built, so the resume is not stuck. Counted from the report, it would be stuck.
		if stuck, err := st.StuckPodResumes(ctx, project, wakeAt.Add(time.Minute-StrandedReleaseWake)); err != nil || len(stuck) != 0 {
			t.Fatalf("the stuck resumes one minute after the take = %+v, %v; want none: the wait for the release is not the pod's", stuck, err)
		}
		if stuck, err := st.StuckPodResumes(ctx, project, wakeAt); err != nil || len(stuck) != 1 {
			t.Fatalf("the stuck resumes %s after the take = %+v, %v; want the one resume", StrandedReleaseWake, stuck, err)
		}
	})
}

// R4: a woken role reads a text that is true whether or not Envoy stored the message that woke
// it. The CI settlement and backstop texts do not change. The release's preamble says what the
// role finds of its workspace in the words of WorkspaceAfterRelease: the release took the pod only
// because the workspace held nothing that origin does not hold, and it kept no copy of any work.
func TestAWokenRoleReadsATrueWakeText(t *testing.T) {
	t.Parallel()
	preamble := "Your issue's pod had been released while it was idle, and this wake brought it back. " +
		WorkspaceAfterRelease + " Your session is as you left it. "

	wokenTask := func(t *testing.T, fact intake.ListenerWake) string {
		t.Helper()
		rig := newWakeRig(t)
		rig.apply("wake", fact)
		starts := wakeStarts(t, rig.pool)
		if len(starts) != 1 {
			t.Fatalf("supervise start rows = %+v, want the one wake", starts)
		}
		return starts[0].Task
	}
	holds := func(t *testing.T, task string, sentences ...string) {
		t.Helper()
		for _, sentence := range sentences {
			if !strings.Contains(task, sentence) {
				t.Errorf("the woken role's task = %q, want it to say %q", task, sentence)
			}
		}
	}

	t.Run("(f) an agent message wake", func(t *testing.T) {
		task := wokenTask(t, intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-agent"})
		holds(t, task,
			"The wake is a message somebody sent to your session (session-implementer) while your pod was gone.",
			"A message that Envoy or Dispatch stored for your session reaches this session, for 72 hours after it was stored.",
			"A message that neither stored, such as a direct send that Envoy refused, does not reach you, and its sender must send it again.")
	})

	t.Run("(g) a role message wake", func(t *testing.T) {
		token := string(wakeToken(t, claim.RoleImplementer))
		task := wokenTask(t, intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: token, At: wakeAt, Report: "report-role"})
		holds(t, task,
			"The wake is a message somebody published to your role topic ("+token+") while your pod was gone.",
			"A message that Envoy kept for your role reaches this session, for one hour after Envoy kept it.",
			"When that hour ends before a session takes the message, Envoy drops it and tells its sender.",
			"A message that Envoy did not keep, such as one sent with keep: false, does not reach you, and its sender must send it again.")
	})

	t.Run("(h) each message wake's whole text, under each stop", func(t *testing.T) {
		const pausePreamble = "Your project was paused, and its pause stopped your session. This wake started it again. " +
			"Your pod, your workspace and any work you had not pushed are as you left them. "
		for _, tc := range []struct {
			stop           WakeStop
			preamble, away string
		}{
			{stop: StoppedByRelease, preamble: preamble, away: "while your pod was gone"},
			{stop: StoppedByPause, preamble: pausePreamble, away: "while your session was stopped"},
		} {
			for _, wake := range []struct{ kind, detail, want string }{
				{kind: pods.WakeAgentMessage, detail: "session-implementer",
					want: tc.preamble + "The wake is a message somebody sent to your session (session-implementer) " + tc.away + ". " +
						"A message that Envoy or Dispatch stored for your session reaches this session, for 72 hours after it was stored. " +
						"A message that neither stored, such as a direct send that Envoy refused, does not reach you, and its sender must send it again."},
				{kind: pods.WakeRoleMessage, detail: "legion-LEGION-208-implementer",
					want: tc.preamble + "The wake is a message somebody published to your role topic (legion-LEGION-208-implementer) " + tc.away + ". " +
						"A message that Envoy kept for your role reaches this session, for one hour after Envoy kept it. " +
						"It comes as an Envoy card that shows kept_at, possibly before this task. " +
						"When that hour ends before a session takes the message, Envoy drops it and tells its sender. " +
						"A message that Envoy did not keep, such as one sent with keep: false, does not reach you, and its sender must send it again."},
			} {
				if got := WakeTask(wake.kind, wake.detail, tc.stop); got != wake.want {
					t.Errorf("WakeTask(%s, stop %d) = %q, want %q", wake.kind, tc.stop, got, wake.want)
				}
			}
		}
	})

	t.Run("(i) the CI settlement and backstop texts are unchanged", func(t *testing.T) {
		for _, tc := range []struct{ kind, detail, want string }{
			{kind: pods.WakeChecks, detail: "head1 failed build",
				want: preamble + "The wake is a CI settlement on your pull request's head: head1 failed build. Read the checks before you decide anything."},
			{kind: pods.WakeBackstop, detail: "the backstop armed for LEGION-208's implementer came due",
				want: preamble + "The wake is the release's backstop running out: the backstop armed for LEGION-208's implementer came due. " +
					"Nothing else asked for you, so take up your phase where you left it."},
		} {
			if got := WakeTask(tc.kind, tc.detail, StoppedByRelease); got != tc.want {
				t.Errorf("WakeTask(%s) = %q, want %q", tc.kind, got, tc.want)
			}
		}
	})
}
