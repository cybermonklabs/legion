package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// seedRunningRoot is the state an idle release binds to, and the opposite of a close's: the
// root's issue row in the workflow at generation, not lingering.
func seedRunningRoot(t *testing.T, store *Store, generation int) {
	t.Helper()
	_, err := store.Pool().Exec(context.Background(), `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ($1, $1, 'LEGION', 'root', 'implementing', $2, 'in_progress', 'A', 1)
		on conflict (key) do update set generation = excluded.generation, status = excluded.status,
			phase = excluded.phase, linger_until = null`, rootIssue, generation)
	if err != nil {
		t.Fatalf("seed running root: %v", err)
	}
}

// openRunningTree is a workflow tree admitted at epoch one whose root is still running: the one
// shape a release authorizes and a close does not.
func openRunningTree(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.OpenTreeLifecycle(context.Background(), "legion", rootIssue, "workflow"); err != nil {
		t.Fatalf("open lifecycle of %s: %v", rootIssue, err)
	}
	seedRunningRoot(t, store, 1)
}

// markRelease is the mark ReleaseIdleIssue leaves, written by hand so a fence can be tested on
// its own: the ids of the release's first row, of its boundary and of its last, the newest
// start row the pod's claims held when the release was marked, and the roles whose claims the
// release's own stops have suspended so far.
func markRelease(t *testing.T, store *Store, first, boundary, last, started int64, suspended ...claim.Role) {
	t.Helper()
	markReleaseOf(t, store, rootIssue, first, boundary, last, started, suspended...)
}

// markReleaseOf is markRelease for the pod of issue.
func markReleaseOf(t *testing.T, store *Store, issue string, first, boundary, last, started int64, suspended ...claim.Role) {
	t.Helper()
	roles := make([]string, 0, len(suspended))
	for _, role := range suspended {
		roles = append(roles, string(role))
	}
	_, err := store.Pool().Exec(context.Background(), `insert into issue_releases
		(project, issue, generation, first_row, boundary_row, last_row, started_row, role, suspended, marked_at)
		values ('legion', $1, 1, $2, $3, $4, $5, 'architect', $6, now())`, issue, first, boundary, last, started, roles)
	if err != nil {
		t.Fatalf("mark the release of %s: %v", issue, err)
	}
}

// IssueRelease authorizes the pod of an issue that is IN the workflow — the mirror of
// IssueSuspension, which authorizes one that has left it. It refuses or waits on the same fences
// in the same order: generations unmoved, the pod not taken up again, the tree's cleanup
// unreserved, a wait while one of the release's own role stops is unrun or a stored claim may
// still hold a process. It then makes the release-only wait while its own workspace check is queued.
func TestIssueReleaseAuthorizesAnIssueInTheWorkflowAndWaitsForAllStoredRoles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		change    string
		state     supervise.ClaimState
		lastStart int64
		noMark    bool
		want      ReleaseAct
		wantCause ReleaseCause
		wantWait  string
	}{
		{name: "every role released", state: supervise.StateSuspended, want: ReleaseActs},
		{name: "no mark: the release was cancelled or is over", state: supervise.StateSuspended, noMark: true, want: ReleaseOver},
		// A claim holding a process is the pod being used again, which the teardown must not
		// take: it is cancelled rather than waited on, since the check's read of the workspace is
		// already older than whatever that process is doing.
		{name: "held turn", state: supervise.StateWorking, want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "uncertain launch", state: supervise.StateLaunchUncertain, want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "a role resumed into the pod", state: supervise.StateIdle, wantWait: "stored claim"},
		{
			name: "the issue left the workflow", state: supervise.StateSuspended, want: ReleaseCancel,
			wantCause: ReleaseCauseUnauthorized,
			change:    "update issues set status='backlog' where key='LEGION-208'",
		},
		{
			name: "the issue is done", state: supervise.StateSuspended, want: ReleaseCancel,
			wantCause: ReleaseCauseUnauthorized,
			change:    "update issues set phase='done', status='done' where key='LEGION-208'",
		},
		{
			name: "the tree lingers", state: supervise.StateSuspended, want: ReleaseCancel,
			wantCause: ReleaseCauseUnauthorized,
			change:    "update issues set linger_until = now() + interval '1 hour' where key='LEGION-208'",
		},
		{
			name: "a re-admission moved the generation", state: supervise.StateSuspended, want: ReleaseCancel,
			wantCause: ReleaseCauseUnauthorized,
			change:    "update issues set generation=2 where key='LEGION-208'",
		},
		{
			name: "the tree's cleanup is reserved", state: supervise.StateSuspended, want: ReleaseCancel,
			wantCause: ReleaseCauseUnauthorized,
			change:    "update tree_lifecycles set cleanup_started = true where tree = 'LEGION-208'",
		},
		{
			// R4's boundary never saw this one: the start ran before the role stop that would
			// have caught it, so the claim is in the pod again and the teardown would take it.
			// The cause is the one a role stop reads for the same start, so the records of a
			// deployment carry one reason and not two.
			name: "a role started again since the mark", state: supervise.StateSuspended,
			lastStart: 7, want: ReleaseCancel, wantCause: ReleaseCauseOvertaken,
		},
		{
			name: "a start of a role of the pod is queued above the release's rows", state: supervise.StateSuspended,
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken,
			change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values
				(30,'supervise','LEGION-208','{"op":"start","tree":"LEGION-208","role":"architect","generation":1}',0,now(),'')`,
		},
		{
			name: "a role stop of the release is still unrun", state: supervise.StateSuspended, wantWait: "role stop effects",
			change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values
				(9,'supervise','LEGION-208','{"op":"release","tree":"LEGION-208","role":"architect","generation":1}',0,now(),'')`,
		},
		{
			name: "a close's suspend of the same run is still unrun", state: supervise.StateSuspended, wantWait: "role stop effects",
			change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values
				(9,'supervise','LEGION-208','{"op":"suspend","tree":"LEGION-208","role":"architect","generation":1}',0,now(),'')`,
		},
		{
			// The release's own workspace check, row 5 of its range, still in the outbox after
			// failed attempts and under a lease that has expired: whatever its attempts, retry time
			// or lease, nobody has read the workspace the teardown would delete.
			name: "the release's own workspace check is still queued", state: supervise.StateSuspended, wantWait: "workspace check",
			change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error,lease_token,lease_until) values
				(5,'issue_bundle','LEGION-208','{"tree":"LEGION-208","generation":1,"treeGeneration":1,"role":"architect"}',
				3,now() + interval '1 hour','the workspace could not be read','expired',now() - interval '1 minute')`,
		},
		{
			// A check below the mark's range is an earlier release's, and holds no pod of this one.
			name: "an earlier release's check is still queued", state: supervise.StateSuspended, want: ReleaseActs,
			change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values
				(4,'issue_bundle','LEGION-208','{"tree":"LEGION-208","generation":1,"treeGeneration":1,"role":"architect"}',
				2,now() + interval '1 hour','the workspace could not be read')`,
		},
		{
			// A pending check is a wait, and a wait never stands in for a cancel the store decides
			// first: a reserved cleanup deletes the Sandbox itself.
			name: "the tree's cleanup is reserved while the check is queued", state: supervise.StateSuspended, want: ReleaseCancel,
			wantCause: ReleaseCauseUnauthorized,
			change: `update tree_lifecycles set cleanup_started = true where tree = 'LEGION-208';
				insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values
				(5,'issue_bundle','LEGION-208','{"tree":"LEGION-208","generation":1,"treeGeneration":1,"role":"architect"}',
				1,now() + interval '1 hour','the workspace could not be read')`,
		},
		{
			name: "a role started again while the check is queued", state: supervise.StateSuspended,
			lastStart: 7, want: ReleaseCancel, wantCause: ReleaseCauseOvertaken,
			change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values
				(5,'issue_bundle','LEGION-208','{"tree":"LEGION-208","generation":1,"treeGeneration":1,"role":"architect"}',
				1,now() + interval '1 hour','the workspace could not be read')`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			if !tc.noMark {
				markRelease(t, st, 5, 6, 10, 0)
			}
			c := supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion", Tree: rootIssue,
				Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: tc.state,
				LastStartRow: tc.lastStart, SuspendedBy: supervise.SuspendedByReleased}
			if err := st.PutClaim(ctx, c); err != nil {
				t.Fatal(err)
			}
			if tc.change != "" {
				if _, err := st.Pool().Exec(ctx, tc.change); err != nil {
					t.Fatal(err)
				}
			}
			release := IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: 10}
			decision, err := st.IssueRelease(ctx, "legion", release, record.OutOfWorkflow)
			if tc.wantWait != "" {
				if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), tc.wantWait) || decision.Act == ReleaseActs {
					t.Fatalf("release = %s, %v; want the wait %q", decision.Act, err, tc.wantWait)
				}
				return
			}
			if err != nil || decision.Act != tc.want {
				t.Fatalf("release = %s, %v; want %s", decision.Act, err, tc.want)
			}
			if decision.Cause != tc.wantCause {
				t.Fatalf("the cancel's cause = %s, want %s", decision.Cause, tc.wantCause)
			}
		})
	}
}

// The teardown reads a start queued above the release's rows by the one rule the idle hold and
// the boundary read (idleWaitingRow). A start queued for news alone does not cancel it: the start
// wakes the claim the release suspended, as any start wakes one, and the role comes back with its
// news in the pod that replaces this one. A phase start queued after it is the pod being used
// again, and cancels the release as overtaken, as it does at a role stop.
func TestTheTeardownIsCancelledByAPhaseStartQueuedAfterTheReleaseAndNotByAStartOfNewsAlone(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	markRelease(t, st, 5, 6, 10, 0)
	if err := st.PutClaim(ctx, supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion", Tree: rootIssue,
		Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: supervise.StateSuspended,
		SuspendedBy: supervise.SuspendedByReleased}); err != nil {
		t.Fatal(err)
	}
	queue := func(id int64, payload string) {
		t.Helper()
		if _, err := st.Pool().Exec(ctx, `insert into outbox (id, kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'supervise', $2, $3, 0, now(), '')`, id, rootIssue, payload); err != nil {
			t.Fatal(err)
		}
	}
	release := IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: 10}

	queue(30, `{"op":"start","tree":"LEGION-208","role":"architect","generation":1,"news":true}`)
	if decision, err := st.IssueRelease(ctx, "legion", release, record.OutOfWorkflow); err != nil || decision.Act != ReleaseActs {
		t.Fatalf("the teardown with a start of news alone queued after the release's rows = %s (%s), %v; want %s",
			decision.Act, decision.Cause, err, ReleaseActs)
	}

	queue(31, `{"op":"start","tree":"LEGION-208","role":"tester","generation":1,"phase":"testing"}`)
	decision, err := st.IssueRelease(ctx, "legion", release, record.OutOfWorkflow)
	if err != nil || decision.Act != ReleaseCancel || decision.Cause != ReleaseCauseOvertaken {
		t.Fatalf("the teardown with a phase start queued after the release's rows = %s (%s), %v; want %s for %s",
			decision.Act, decision.Cause, err, ReleaseCancel, ReleaseCauseOvertaken)
	}
}

// A close and a release order against each other: each waits on the other's role stops, which is
// what adding 'release' to both op fences buys. A release row below a close's own row makes the
// close wait, exactly as one of its own suspends would.
func TestACloseWaitsForAReleasesRoleStops(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openLingeringTree(t, st)
	c := supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion", Tree: rootIssue,
		Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: supervise.StateSuspended}
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values
		(9,'supervise','LEGION-208','{"op":"release","tree":"LEGION-208","role":"architect","generation":1}',0,now(),'')`); err != nil {
		t.Fatal(err)
	}
	act, err := st.IssueSuspension(ctx, "legion", IssueClose{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: 10}, record.OutOfWorkflow)
	if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), "role stop effects") || act {
		t.Fatalf("suspension = %t, %v; want it to wait for the release's role stop", act, err)
	}
}

// The release's boundary is the row that stops the first role process of the pod, and
// IssueReleaseStop is the one place it is read. Every row the release queued lies between the
// mark's first_row and its last_row, so a task, notice, message or start of the issue outside
// that range — queued by a transaction already in flight when the mark was written, or queued
// once the mark stood — reached a pod every role of which was still running: it cancels the
// release at the boundary. A claim started since the mark, or one holding a process, cancels it
// at any of the release's role stops. A row no mark holds does nothing.
//
// The mark stands through every one of those answers: the cancel is carried out by
// CancelIssueRelease, which queues the resumes and deletes the mark together, and a mark
// deleted here would leave a cancel that did not finish unreadable by the row's own retry.
func TestTheReleasesBoundaryCancelsOnWorkThePodWasGivenAfterTheMark(t *testing.T) {
	t.Parallel()
	const first, boundary, last = 18, 20, 22
	for _, tc := range []struct {
		name      string
		row       int64
		queued    int64
		kind      string
		state     supervise.ClaimState
		started   int64
		lastStart int64
		noMark    bool
		want      ReleaseAct
		wantCause ReleaseCause
	}{
		{name: "nothing queued", row: boundary, want: ReleaseActs},
		{name: "a start queued below the release's rows", row: boundary, queued: first - 1, kind: "supervise",
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "a notice queued below them", row: boundary, queued: first - 1, kind: "notice",
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "a merge-forward message queued below them", row: boundary, queued: first - 1, kind: "merge_forward",
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "a board write queued below them", row: boundary, queued: first - 1, kind: "dispatch_status", want: ReleaseActs},
		// The release's own rows are inside the range, so they are not work the pod was given.
		{name: "a role stop of the release itself", row: boundary, queued: boundary + 1, kind: "supervise", want: ReleaseActs},
		{name: "a start queued above the release's rows", row: boundary, queued: last + 1, kind: "supervise",
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "a notice queued above them", row: boundary, queued: last + 1, kind: "notice",
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		// A later role stop reads the claims and not the queue: the first role process is
		// already stopped, so a delivery waits behind the release rather than cancelling it.
		{name: "a later role stop with a delivery queued", row: boundary + 1, queued: last + 1, kind: "notice", want: ReleaseActs},
		{name: "a start the lane already ran", row: boundary, started: 5, lastStart: 19,
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "a start the lane already ran, at a later role stop", row: boundary + 1, started: 5, lastStart: 19,
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "the start the mark was taken at", row: boundary, started: 19, lastStart: 19, want: ReleaseActs},
		{name: "a claim in a turn", row: boundary, state: supervise.StateWorking,
			want: ReleaseCancel, wantCause: ReleaseCauseOvertaken},
		{name: "the release was already cancelled", row: boundary, noMark: true, want: ReleaseOver},
		{name: "a row of a release this mark is not", row: last + 5, want: ReleaseOver},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			if !tc.noMark {
				markRelease(t, st, first, boundary, last, tc.started)
			}
			if tc.queued != 0 {
				payload := `{"op":"start","tree":"LEGION-208","role":"architect","generation":1}`
				if tc.kind != "supervise" {
					payload = `{"kind":"todo","reason":"r"}`
				}
				if tc.kind == "dispatch_status" {
					payload = `{"status":"in_progress","observedStatus":"todo"}`
				}
				if _, err := st.Pool().Exec(ctx, `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error)
					values ($1,$2,'LEGION-208',$3,0,now(),'')`, tc.queued, tc.kind, payload); err != nil {
					t.Fatal(err)
				}
			}
			if tc.lastStart != 0 || tc.state != "" {
				state := tc.state
				if state == "" {
					state = supervise.StateIdle
				}
				if err := st.PutClaim(ctx, supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"),
					Project: "legion", Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1,
					Generation: 1, State: state, LastStartRow: tc.lastStart}); err != nil {
					t.Fatal(err)
				}
			}
			decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, tc.row)
			if err != nil {
				t.Fatalf("IssueReleaseStop: %v", err)
			}
			if decision.Act != tc.want || decision.Cause != tc.wantCause {
				t.Fatalf("the role stop = %s for %s, want %s for %s", decision.Act, decision.Cause, tc.want, tc.wantCause)
			}
			var marked bool
			if err := st.Pool().QueryRow(ctx, `select exists (select 1 from issue_releases where issue = $1)`, rootIssue).Scan(&marked); err != nil {
				t.Fatal(err)
			}
			if marked != !tc.noMark {
				t.Fatalf("the release's mark stands = %t, want %t: only the cancel itself deletes it", marked, !tc.noMark)
			}
		})
	}
}

// releaseThroughTheStore marks a release of issue's pod under project legion the way the idle
// tick does — the three rows in one transaction with the mark that records them
// (ReleaseIdleIssue) — and answers the ids the mark holds: the workspace check, the role stop that
// is its boundary, and the teardown.
func releaseThroughTheStore(t *testing.T, st *Store, issue string) (first, boundary, last int64) {
	t.Helper()
	ctx := context.Background()
	marked, err := st.ReleaseIdleIssue(ctx, "legion", issue, func(pods.IdleView) (IdleReleaseMark, error) {
		return IdleReleaseMark{Generation: 1, Role: claim.RoleArchitect, Rows: []IdleReleaseRow{
			{Kind: "issue_bundle", Payload: []byte(fmt.Sprintf(`{"tree":%q,"generation":1,"treeGeneration":1,"role":"architect"}`, issue))},
			{Kind: "supervise", Payload: []byte(fmt.Sprintf(`{"op":"release","tree":%q,"role":"architect","generation":1}`, issue)), Boundary: true},
			{Kind: "issue_release", Payload: []byte(fmt.Sprintf(`{"tree":%q,"generation":1,"treeGeneration":1}`, issue))},
		}}, nil
	})
	if err != nil || !marked {
		t.Fatalf("ReleaseIdleIssue of %s = %t, %v; want the release marked", issue, marked, err)
	}
	if err := st.Pool().QueryRow(ctx, `select first_row, boundary_row, last_row from issue_releases
		where project = 'legion' and issue = $1`, issue).Scan(&first, &boundary, &last); err != nil {
		t.Fatalf("read the mark of the release of %s: %v", issue, err)
	}
	return first, boundary, last
}

// R4's boundary, driven the way production drives it: the mark and the release's rows are
// written in one transaction, so every row the release queued has an id above the boundary and
// a delivery that reaches the pod after the mark has one above them all. Until the first role
// process of the pod is stopped, such a delivery — and a turn that started without one, which
// shows as a claim holding a process — cancels the whole release, because the check's read of the
// workspace is older than what the pod has done since.
func TestWorkThePodIsGivenAfterTheMarkCancelsTheReleaseAtItsBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		kind    string
		payload string
		state   supervise.ClaimState
	}{
		{name: "a task queued for a role of the pod", kind: "supervise",
			payload: `{"op":"start","tree":"LEGION-208","role":"tester","generation":1}`},
		{name: "an architect's notice", kind: "notice", payload: `{"kind":"todo","reason":"r"}`},
		{name: "a merge-forward message", kind: "merge_forward",
			payload: `{"role":"implementer","step":"ask","head":"c0de","base":"main","phase":"testing","pullRequest":42,"why":"required-check-not-green"}`},
		{name: "a turn a message started", state: supervise.StateWorking},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			token := claim.Token("legion-legion-legion-208-architect")
			live := supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
				Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: supervise.StateIdle}
			if err := st.PutClaim(ctx, live); err != nil {
				t.Fatal(err)
			}
			_, boundary, _ := releaseThroughTheStore(t, st, rootIssue)

			// The delivery takes its id from the outbox's own sequence, as every writer of the
			// outbox does, so it lands above every row the release queued.
			if tc.kind != "" {
				if _, err := st.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
					values ($1, $2, $3, 0, now(), '')`, tc.kind, rootIssue, tc.payload); err != nil {
					t.Fatal(err)
				}
			}
			if tc.state != "" {
				live.State = tc.state
				if err := st.PutClaim(ctx, live); err != nil {
					t.Fatal(err)
				}
			}

			decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
			if err != nil {
				t.Fatalf("IssueReleaseStop: %v", err)
			}
			if decision.Act != ReleaseCancel || decision.Cause != ReleaseCauseOvertaken {
				t.Fatalf("the boundary = %s for %s, want %s for %s: the pod was given work after the release was marked",
					decision.Act, decision.Cause, ReleaseCancel, ReleaseCauseOvertaken)
			}
			// The mark stands until the cancel is carried out, and goes with it.
			if !releaseMarked(t, st) {
				t.Fatal("the cancel was decided and the mark was already gone; a cancel that fails would be unreadable")
			}
			if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()}); err != nil {
				t.Fatalf("carry the cancel out: %v", err)
			}
			if releaseMarked(t, st) {
				t.Fatal("the cancelled release left its mark behind")
			}
		})
	}
}

// A whole turn between the mark and a role's stop — a message the role answered after the check
// read the workspace and before its stop ran, its turn over by then — leaves no claim in a turn
// and no start row, and can have left work the check did not read. The mark records the newest
// turn its claims had when it was taken (issue_releases.turn_start, read through the idle view), so
// a claim whose last turn started after it cancels the release at the stop, as a turn in progress
// does (releaseOvertaken). The control keeps the turn the mark was taken at, and the stop acts.
func TestATurnBetweenTheMarkAndAStopCancelsTheRelease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		later time.Duration
		want  ReleaseAct
		cause ReleaseCause
	}{
		{name: "a turn that started after the mark", later: time.Second, want: ReleaseCancel, cause: ReleaseCauseOvertaken},
		{name: "control: the turn the mark was taken at", want: ReleaseActs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			if err := st.PutClaim(ctx, supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion",
				Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1,
				State: supervise.StateIdle}); err != nil {
				t.Fatal(err)
			}
			turnAt := func(at time.Time) {
				t.Helper()
				if _, err := st.Pool().Exec(ctx, `update claims set last_turn_start = $2 where issue = $1`, rootIssue, at); err != nil {
					t.Fatalf("stamp the architect's turn: %v", err)
				}
			}
			turned := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
			turnAt(turned)
			_, boundary, _ := releaseThroughTheStore(t, st, rootIssue)
			var marked time.Time
			if err := st.Pool().QueryRow(ctx, `select turn_start from issue_releases where issue = $1`, rootIssue).Scan(&marked); err != nil || !marked.Equal(turned) {
				t.Fatalf("the mark's turn = %s, %v; want the architect's turn %s, read through the view", marked, err, turned)
			}

			// The turn is over by the stop: the claim is idle again, as it was at the mark.
			turnAt(turned.Add(tc.later))
			decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
			if err != nil {
				t.Fatalf("IssueReleaseStop: %v", err)
			}
			if decision.Act != tc.want || decision.Cause != tc.cause {
				t.Fatalf("the boundary = %s for %s, want %s for %s", decision.Act, decision.Cause, tc.want, tc.cause)
			}
		})
	}
}

// The workspace check is the release's first row, and the pod can be given work between the mark
// and the check's read just as between the mark and a role's stop: a whole turn there leaves no
// claim in a turn and no start row, and a decline taken over it would hold the pod for work that
// turn may have pushed. The check reads the mark's turn (issue_releases.turn_start) as the stops do,
// and a claim whose last turn started after it cancels the release before any read
// (releaseOvertaken). The control keeps the turn the mark was taken at, and the check acts.
func TestAWorkspaceCheckOfAReleaseGivenWorkSinceItsMarkIsCancelled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		later time.Duration
		want  ReleaseAct
		cause ReleaseCause
	}{
		{name: "a turn that started after the mark", later: time.Second, want: ReleaseCancel, cause: ReleaseCauseOvertaken},
		{name: "control: the turn the mark was taken at", want: ReleaseActs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			if err := st.PutClaim(ctx, supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion",
				Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1,
				State: supervise.StateIdle}); err != nil {
				t.Fatal(err)
			}
			turnAt := func(at time.Time) {
				t.Helper()
				if _, err := st.Pool().Exec(ctx, `update claims set last_turn_start = $2 where issue = $1`, rootIssue, at); err != nil {
					t.Fatalf("stamp the architect's turn: %v", err)
				}
			}
			turned := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
			turnAt(turned)
			first, _, _ := releaseThroughTheStore(t, st, rootIssue)

			// The turn is over by the check: the claim is idle again, as it was at the mark.
			turnAt(turned.Add(tc.later))
			decision, err := st.IssueReleaseCheck(ctx, "legion", rootIssue, first)
			if err != nil {
				t.Fatalf("IssueReleaseCheck: %v", err)
			}
			if decision.Act != tc.want || decision.Cause != tc.cause {
				t.Fatalf("the check = %s for %s, want %s for %s", decision.Act, decision.Cause, tc.want, tc.cause)
			}
		})
	}
}

// A release's rows are still on the lane after it is cancelled, and the pod can be released
// again before they run. Such a row names a release the mark is not, and acting on it would
// suspend a claim of the live release, or tear its pod down with the workspace on its disk. The
// mark holds the ids of its own rows, and a row outside them does nothing at all.
func TestARowOfACancelledReleaseNeitherActsNorCancelsTheReleaseRunningNow(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	token := claim.Token("legion-legion-legion-208-architect")
	if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: supervise.StateIdle}); err != nil {
		t.Fatal(err)
	}

	firstA, boundaryA, lastA := releaseThroughTheStore(t, st, rootIssue)
	if _, err := st.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('notice', $1, '{"kind":"todo","reason":"r"}', 0, now(), '')`, rootIssue); err != nil {
		t.Fatal(err)
	}
	if decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundaryA); err != nil || decision.Act != ReleaseCancel {
		t.Fatalf("the first release's boundary = %s, %v; want it cancelled", decision.Act, err)
	}
	if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()}); err != nil {
		t.Fatalf("carry the first release's cancel out: %v", err)
	}
	// The notice is delivered and the pod goes quiet again, so a second release is marked. Its
	// rows are a second set, and the first release's are still queued behind them.
	if _, err := st.Pool().Exec(ctx, `delete from outbox where issue = $1`, rootIssue); err != nil {
		t.Fatal(err)
	}
	firstB, boundaryB, lastB := releaseThroughTheStore(t, st, rootIssue)
	if firstB <= lastA {
		t.Fatalf("the second release's rows are %d..%d, want them above the first's %d..%d", firstB, lastB, firstA, lastA)
	}

	if decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundaryA); err != nil || decision.Act != ReleaseOver {
		t.Fatalf("the cancelled release's role stop = %s, %v; want %s", decision.Act, err, ReleaseOver)
	}
	stale := IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: lastA}
	if decision, err := st.IssueRelease(ctx, "legion", stale, record.OutOfWorkflow); err != nil || decision.Act != ReleaseOver {
		t.Fatalf("the cancelled release's teardown = %s, %v; want %s", decision.Act, err, ReleaseOver)
	}
	var boundary int64
	if err := st.Pool().QueryRow(ctx, `select boundary_row from issue_releases where issue = $1`, rootIssue).Scan(&boundary); err != nil {
		t.Fatalf("read the mark after the stale rows ran: %v", err)
	}
	if boundary != boundaryB {
		t.Fatalf("the mark names boundary %d, want the running release's %d", boundary, boundaryB)
	}
	// The running release's own rows still act.
	if decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundaryB); err != nil || decision.Act != ReleaseActs {
		t.Fatalf("the running release's boundary = %s, %v; want %s", decision.Act, err, ReleaseActs)
	}
}

// releaseMarked is whether a release of the root issue is marked in flight.
func releaseMarked(t *testing.T, st *Store) bool {
	t.Helper()
	var marked bool
	if err := st.Pool().QueryRow(context.Background(),
		`select exists (select 1 from issue_releases where issue = $1)`, rootIssue).Scan(&marked); err != nil {
		t.Fatalf("read whether %s is being released: %v", rootIssue, err)
	}
	return marked
}

// resumeRow is the row a cancel queues to bring one claim back, as the daemon's own builder
// writes it (outbox.releaseResume): the `start` of that claim at the generation the cancel read.
func resumeRow(token claim.Token, role claim.Role, generation uint64) (IdleReleaseRow, error) {
	return IdleReleaseRow{Kind: "supervise", Payload: []byte(fmt.Sprintf(
		`{"op":"start","tree":%q,"role":%q,"claim":%q,"generation":%d}`, rootIssue, role, token, generation))}, nil
}

// releasedClaim is one claim of the root issue the daemon's release left suspended as
// `released`, which is the state a cancel resumes from.
func releasedClaim(t *testing.T, st *Store, role claim.Role) {
	t.Helper()
	if err := st.PutClaim(context.Background(), supervise.Claim{
		Token: claim.Token("legion-legion-legion-208-" + string(role)), Project: "legion", Tree: rootIssue,
		Issue: rootIssue, Role: role, TreeEpoch: 1, Generation: 1, State: supervise.StateSuspended,
		SuspendedBy: supervise.SuspendedByReleased}); err != nil {
		t.Fatalf("seed the released %s: %v", role, err)
	}
}

// startRows are the roles of the `start` rows queued for the root issue, in id order: what a
// cancel left behind to bring the pod's roles back.
func startRows(t *testing.T, st *Store) []string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `select payload->>'role' from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, rootIssue)
	if err != nil {
		t.Fatalf("read the queued starts of %s: %v", rootIssue, err)
	}
	defer rows.Close()
	var roles []string
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			t.Fatalf("read the queued starts of %s: %v", rootIssue, err)
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the queued starts of %s: %v", rootIssue, err)
	}
	return roles
}

// A cancel decided at a role stop is not carried out by that decision: the mark stands until
// the resumes of the claims the release suspended are queued, and the two happen in one
// transaction. A mark deleted at the decision left the row's retry reading a release that is
// over — nothing to resume, nothing to undo — with the roles the release had already suspended
// `released` in a pod that still stands, which is what a resume that fails, or a daemon that
// stops between the two, produces.
func TestACancelKeepsTheMarkUntilItsResumesAreQueued(t *testing.T) {
	t.Parallel()
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	markRelease(t, st, first, boundary, last, 0, claim.RoleArchitect)
	releasedClaim(t, st, claim.RoleArchitect)
	// A notice the pod must answer, queued above the release's own rows: the cancel at the
	// boundary that comes of it is the one every other cancel is shaped like.
	if _, err := st.Pool().Exec(ctx, `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error)
		values ($1,'notice',$2,'{"kind":"todo","reason":"r"}',0,now(),'')`, last+1, rootIssue); err != nil {
		t.Fatal(err)
	}

	decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
	if err != nil || decision.Act != ReleaseCancel {
		t.Fatalf("the boundary = %s, %v; want it cancelled", decision.Act, err)
	}
	if !releaseMarked(t, st) {
		t.Fatal("the decision deleted the mark; the cancel it decided had not been carried out")
	}
	// The row is retried — its resume failed, or the daemon stopped — and reads the same cancel.
	retried, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
	if err != nil || retried.Act != ReleaseCancel {
		t.Fatalf("the retried stop = %s, %v; want the cancel read again", retried.Act, err)
	}

	refused := errors.New("the resume could not be encoded")
	if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, func(claim.Token, claim.Role, uint64) (IdleReleaseRow, error) {
		return IdleReleaseRow{}, refused
	}, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()}); !errors.Is(err, refused) {
		t.Fatalf("a cancel whose resume could not be built = %v, want %v", err, refused)
	}
	if !releaseMarked(t, st) {
		t.Fatal("a cancel that queued no resume still deleted the mark")
	}
	if queued := startRows(t, st); len(queued) != 0 {
		t.Fatalf("starts queued by the refused cancel = %v, want none", queued)
	}

	cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()})
	if err != nil {
		t.Fatalf("carry the cancel out: %v", err)
	}
	if resumed := cancelled.Resumed; len(resumed) != 1 || resumed[0] != claim.RoleArchitect {
		t.Fatalf("resumed = %v, want the one claim the release suspended", resumed)
	}
	if queued := startRows(t, st); len(queued) != 1 || queued[0] != string(claim.RoleArchitect) {
		t.Fatalf("starts queued by the cancel = %v, want the architect's", queued)
	}
	if releaseMarked(t, st) {
		t.Fatal("the cancel queued its resumes and left the mark standing")
	}
}

// A decline is the cancel and two writes more, all in the cancel's one transaction: the decline
// row the idle rule reads (idle_release_declines, with the newest start row of the issue's claims)
// and the one message that tells the role the release waits for its push. One transaction is what
// makes "one declined release, one message" hold. A resume the store cannot queue leaves the mark
// standing and writes neither, so the row's retry runs the whole decline again. And a decline that
// finds no mark — the check of a release that is over, or the lane's retry of a check whose decline
// already committed — writes nothing and answers Declined false, so no second row and no second
// message is ever written for one release.
func TestADeclineWritesItsRowAndItsMessageOnceWithTheCancel(t *testing.T) {
	t.Parallel()
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	markRelease(t, st, first, boundary, last, 0, claim.RoleArchitect)
	releasedClaim(t, st, claim.RoleArchitect)
	if _, err := st.Pool().Exec(ctx, `update claims set last_start_row = 9 where issue = $1`, rootIssue); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	message := IdleReleaseRow{Kind: "role_message", Payload: []byte(`{"role":"legion-legion-legion-208-architect","body":"push your work"}`)}
	decline := IdleReleaseDecline{At: at, Role: claim.RoleArchitect, Reason: pods.DeclineUnpushedWork,
		Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", Commits: 1, Message: &message}
	counts := func() (declines, messages int) {
		t.Helper()
		if err := st.Pool().QueryRow(ctx, `select (select count(*) from idle_release_declines where issue = $1),
			(select count(*) from outbox where issue = $1 and kind = 'role_message')`, rootIssue).Scan(&declines, &messages); err != nil {
			t.Fatalf("count the decline rows of %s: %v", rootIssue, err)
		}
		return declines, messages
	}

	refused := errors.New("the resume could not be encoded")
	if _, err := st.DeclineIssueRelease(ctx, "legion", rootIssue, decline, func(claim.Token, claim.Role, uint64) (IdleReleaseRow, error) {
		return IdleReleaseRow{}, refused
	}, record.OutOfWorkflow, ReleaseCancelPause{At: at}); !errors.Is(err, refused) {
		t.Fatalf("a decline whose resume could not be built = %v, want %v", err, refused)
	}
	if !releaseMarked(t, st) {
		t.Fatal("a decline that queued no resume still deleted the mark")
	}
	if declines, messages := counts(); declines != 0 || messages != 0 {
		t.Fatalf("a decline that failed wrote %d decline rows and %d messages, want none", declines, messages)
	}

	declined, err := st.DeclineIssueRelease(ctx, "legion", rootIssue, decline, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: at})
	if err != nil || !declined.Declined {
		t.Fatalf("decline the release = %+v, %v; want it declined", declined, err)
	}
	if resumed := declined.Resumed; len(resumed) != 1 || resumed[0] != claim.RoleArchitect {
		t.Fatalf("resumed = %v, want the one claim the release suspended", resumed)
	}
	if releaseMarked(t, st) {
		t.Fatal("the decline left the release's mark standing")
	}
	if declines, messages := counts(); declines != 1 || messages != 1 {
		t.Fatalf("after the decline the store holds %d decline rows and %d messages, want one of each", declines, messages)
	}
	var row struct {
		at              time.Time
		role, reason    string
		head            string
		commits         int
		changed         bool
		started         int64
		messageRole     string
		messageBodyRead string
	}
	if err := st.Pool().QueryRow(ctx, `select d.declined_at, d.role, d.reason, d.head, d.commits, d.changed, d.started_row,
		o.payload->>'role', o.payload->>'body'
		from idle_release_declines d, outbox o where d.issue = $1 and o.issue = $1 and o.kind = 'role_message'`, rootIssue).Scan(
		&row.at, &row.role, &row.reason, &row.head, &row.commits, &row.changed, &row.started, &row.messageRole, &row.messageBodyRead); err != nil {
		t.Fatalf("read the decline of %s: %v", rootIssue, err)
	}
	if !row.at.Equal(at) || row.role != "architect" || row.reason != pods.DeclineUnpushedWork || row.head != decline.Head ||
		row.commits != 1 || row.changed || row.started != 9 {
		t.Fatalf("decline row = %+v, want the check's own findings at %s with the claims' newest start row 9", row, at)
	}
	if row.messageRole != "legion-legion-legion-208-architect" || row.messageBodyRead != "push your work" {
		t.Fatalf("message row = %q to %q, want the decline's own message to the architect", row.messageBodyRead, row.messageRole)
	}

	// The lane runs the check again — its row's first attempt committed the decline and failed to
	// report it — and the mark is gone: nothing is written a second time.
	again, err := st.DeclineIssueRelease(ctx, "legion", rootIssue, decline, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: at.Add(time.Minute)})
	if err != nil || again.Declined || len(again.Resumed) != 0 {
		t.Fatalf("a decline of a release that is over = %+v, %v; want nothing declined and nothing resumed", again, err)
	}
	if declines, messages := counts(); declines != 1 || messages != 1 {
		t.Fatalf("after the second decline the store holds %d decline rows and %d messages, want the first decline's one of each", declines, messages)
	}
	if queued := startRows(t, st); len(queued) != 1 {
		t.Fatalf("starts queued = %v, want the first decline's one resume", queued)
	}
}

// A check that reads the same work the decline on record names — the same working-copy commit,
// the same count of commits and the same change — is a read the role was already told of. The
// message of the first decline starts a turn, the turn ends the decline, and the next pass's check
// finds the same work: a second message would start the next turn, and so on for as long as the
// work stays unpushed. So that decline keeps its first moment, moves only what the last check read
// (checked_at, started_row, pod_uid) and tells nobody. A check that finds other work replaces the
// row at its own moment and tells the role again.
func TestADeclineOfTheSameWorkKeepsItsMomentAndTellsNobody(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	message := IdleReleaseRow{Kind: "role_message", Payload: []byte(`{"role":"legion-legion-legion-208-architect","body":"push your work"}`)}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	decline := IdleReleaseDecline{At: at, Role: claim.RoleArchitect, Reason: pods.DeclineUnpushedWork,
		Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", Commits: 1, PodUID: "pod-a", Message: &message}
	// declineAt marks a release whose stop has suspended the architect, with the architect's newest
	// start row at started, and declines it.
	declineAt := func(first, started int64, d IdleReleaseDecline) ReleaseCancelled {
		t.Helper()
		markRelease(t, st, first, first+2, first+4, started, claim.RoleArchitect)
		releasedClaim(t, st, claim.RoleArchitect)
		if _, err := st.Pool().Exec(ctx, `update claims set last_start_row = $2 where issue = $1`, rootIssue, started); err != nil {
			t.Fatal(err)
		}
		declined, err := st.DeclineIssueRelease(ctx, "legion", rootIssue, d, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: d.At})
		if err != nil || !declined.Declined {
			t.Fatalf("decline the release at %s = %+v, %v; want it declined", d.At, declined, err)
		}
		return declined
	}
	type declineRow struct {
		declined, checked time.Time
		commits           int
		started           int64
		pod               string
		messages          int
	}
	read := func() declineRow {
		t.Helper()
		var row declineRow
		if err := st.Pool().QueryRow(ctx, `select declined_at, checked_at, commits, started_row, pod_uid,
			(select count(*) from outbox where issue = $1 and kind = 'role_message')
			from idle_release_declines where issue = $1`, rootIssue).Scan(
			&row.declined, &row.checked, &row.commits, &row.started, &row.pod, &row.messages); err != nil {
			t.Fatalf("read the decline of %s: %v", rootIssue, err)
		}
		return row
	}

	if first := declineAt(18, 9, decline); !first.Told {
		t.Fatalf("the first decline = %+v, want its message queued", first)
	}
	if row := read(); !row.declined.Equal(at) || !row.checked.Equal(at) || row.started != 9 || row.pod != "pod-a" || row.messages != 1 {
		t.Fatalf("after the first decline the row = %+v, want it declined and checked at %s in pod-a at start row 9, with one message", row, at)
	}

	// The role's turn ended the decline, and the next release's check read the same work again, in
	// the pod a relaunch made, after a start row.
	again := decline
	again.At, again.PodUID = at.Add(20*time.Minute), "pod-b"
	if second := declineAt(30, 11, again); second.Told {
		t.Fatalf("the decline of the same work = %+v, want nobody told", second)
	}
	if row := read(); !row.declined.Equal(at) || !row.checked.Equal(again.At) || row.started != 11 || row.pod != "pod-b" || row.messages != 1 {
		t.Fatalf("after the decline of the same work the row = %+v, want it declined at %s, checked at %s in pod-b at start row 11, and still one message",
			row, at, again.At)
	}

	// The role committed again without pushing: other work, so the role is told of it.
	other := decline
	other.At, other.Commits = at.Add(40*time.Minute), 2
	if third := declineAt(40, 11, other); !third.Told {
		t.Fatalf("the decline of other work = %+v, want its message queued", third)
	}
	if row := read(); !row.declined.Equal(other.At) || !row.checked.Equal(other.At) || row.commits != 2 || row.messages != 2 {
		t.Fatalf("after the decline of other work the row = %+v, want it declined and checked at %s with 2 commits, and a second message",
			row, other.At)
	}
}

// The close of a pod whose issue left the workflow marks no release (workflow.LeftPodClose), so
// a check that declines it has nothing to cancel: DeclineLeftPodClose writes the decline row and
// its message and touches no mark, under the same rule for the same work as a release's decline.
func TestALeftPodCloseDeclineRecordsWithNoMark(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	if err := st.PutClaim(ctx, supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion",
		Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1,
		State: supervise.StateIdle, LastStartRow: 7}); err != nil {
		t.Fatal(err)
	}
	message := IdleReleaseRow{Kind: "role_message", Payload: []byte(`{"role":"legion-legion-legion-208-architect","body":"push your work"}`)}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	decline := IdleReleaseDecline{At: at, Role: claim.RoleArchitect, Reason: pods.DeclineUnpushedWork,
		Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", Commits: 1, PodUID: "pod-a", Message: &message}
	read := func() (declined, checked time.Time, started int64, messages, marks int) {
		t.Helper()
		if err := st.Pool().QueryRow(ctx, `select declined_at, checked_at, started_row,
			(select count(*) from outbox where issue = $1 and kind = 'role_message'),
			(select count(*) from issue_releases where issue = $1)
			from idle_release_declines where issue = $1`, rootIssue).Scan(&declined, &checked, &started, &messages, &marks); err != nil {
			t.Fatalf("read the decline of %s: %v", rootIssue, err)
		}
		return declined, checked, started, messages, marks
	}

	if _, err := st.DeclineLeftPodClose(ctx, "", rootIssue, decline); err == nil {
		t.Fatal("a decline with no project was taken")
	}
	if told, err := st.DeclineLeftPodClose(ctx, "legion", rootIssue, decline); err != nil || !told {
		t.Fatalf("decline the close = %t, %v; want the role told", told, err)
	}
	if declined, checked, started, messages, marks := read(); !declined.Equal(at) || !checked.Equal(at) || started != 7 || messages != 1 || marks != 0 {
		t.Fatalf("after the decline: declined %s, checked %s, start row %d, %d messages, %d marks; want %s, %s, 7, one message and no mark",
			declined, checked, started, messages, marks, at, at)
	}

	again := decline
	again.At = at.Add(20 * time.Minute)
	if told, err := st.DeclineLeftPodClose(ctx, "legion", rootIssue, again); err != nil || told {
		t.Fatalf("decline the close over the same work = %t, %v; want nobody told", told, err)
	}
	if declined, checked, _, messages, _ := read(); !declined.Equal(at) || !checked.Equal(again.At) || messages != 1 {
		t.Fatalf("after the same work: declined %s, checked %s, %d messages; want %s, %s and still one message",
			declined, checked, messages, at, again.At)
	}
}

// A decline names the role it told, and the same work read through another role is news to that
// role: the decline replaces the row at its own moment and tells the role, whatever the other role
// was told. A read that failed says nothing of the disk: it moves the decline's reason, its last
// check, its start row and its pod, and keeps what the last read that could read found — the
// working-copy commit, the commits, the change, the role told and the moment it was told — so the
// idle view still reads work on the disk (pods.IdleDecline.Work), and it tells nobody. The next read
// that finds that same work through that same role is no news: it keeps the moment, puts the
// reason back and tells nobody.
func TestADeclineToAnotherRoleIsToldAndAFailedReadKeepsTheWork(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	if err := st.PutClaim(ctx, supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion",
		Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: supervise.StateIdle}); err != nil {
		t.Fatal(err)
	}
	messageTo := func(role claim.Role) *IdleReleaseRow {
		return &IdleReleaseRow{Kind: "role_message", Payload: []byte(fmt.Sprintf(`{"role":"legion-legion-legion-208-%s","body":"push your work"}`, role))}
	}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	work := IdleReleaseDecline{At: at, Role: claim.RoleArchitect, Reason: pods.DeclineUnpushedWork,
		Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", Commits: 1, PodUID: "pod-a", Message: messageTo(claim.RoleArchitect)}
	type declineRow struct {
		declined, checked time.Time
		role, reason      string
		head              string
		commits           int
		changed           bool
		messages          int
	}
	read := func() declineRow {
		t.Helper()
		var row declineRow
		if err := st.Pool().QueryRow(ctx, `select declined_at, checked_at, role, reason, head, commits, changed,
			(select count(*) from outbox where issue = $1 and kind = 'role_message')
			from idle_release_declines where issue = $1`, rootIssue).Scan(
			&row.declined, &row.checked, &row.role, &row.reason, &row.head, &row.commits, &row.changed, &row.messages); err != nil {
			t.Fatalf("read the decline of %s: %v", rootIssue, err)
		}
		return row
	}
	decline := func(d IdleReleaseDecline) bool {
		t.Helper()
		told, err := st.DeclineLeftPodClose(ctx, "legion", rootIssue, d)
		if err != nil {
			t.Fatalf("decline at %s through the %s: %v", d.At, d.Role, err)
		}
		return told
	}

	if !decline(work) {
		t.Fatal("the first decline told nobody")
	}

	// The same work, read through the implementer, who was never told of it.
	toImplementer := work
	toImplementer.At, toImplementer.Role, toImplementer.Message = at.Add(10*time.Minute), claim.RoleImplementer, messageTo(claim.RoleImplementer)
	if !decline(toImplementer) {
		t.Fatal("the same work read through the implementer told nobody; the implementer was never told of it")
	}
	if row := read(); row.role != "implementer" || !row.declined.Equal(toImplementer.At) || row.messages != 2 {
		t.Fatalf("after the read through the implementer the row = %+v, want it the implementer's, declined at %s, with a second message",
			row, toImplementer.At)
	}

	// A read through the architect that failed: the work on record and the role it told stand.
	unread := IdleReleaseDecline{At: at.Add(20 * time.Minute), Role: claim.RoleArchitect, Reason: pods.DeclineWorkspaceUnread, PodUID: "pod-a"}
	if decline(unread) {
		t.Fatal("a read that failed told a role")
	}
	row := read()
	if row.reason != pods.DeclineWorkspaceUnread || !row.checked.Equal(unread.At) || !row.declined.Equal(toImplementer.At) ||
		row.role != "implementer" || row.head != work.Head || row.commits != 1 || row.changed || row.messages != 2 {
		t.Fatalf("after the failed read the row = %+v, want the reason %s and the check at %s, and the work, the role and the moment of the read before",
			row, pods.DeclineWorkspaceUnread, unread.At)
	}
	if view := idleViewOf(t, st, rootIssue); !view.Declined.Work || view.Declined.Reason != pods.DeclineWorkspaceUnread {
		t.Fatalf("the idle view's decline after the failed read = %+v, want the work the read before found", view.Declined)
	}

	// The same work through the implementer again: no news.
	again := toImplementer
	again.At = at.Add(30 * time.Minute)
	if decline(again) {
		t.Fatal("the same work read again through the role told of it told it again")
	}
	if row := read(); row.reason != pods.DeclineUnpushedWork || !row.declined.Equal(toImplementer.At) || !row.checked.Equal(again.At) || row.messages != 2 {
		t.Fatalf("after the same work again the row = %+v, want the reason back at %s, declined at %s, checked at %s and no third message",
			row, pods.DeclineUnpushedWork, toImplementer.At, again.At)
	}
}

// A failed read keeps the work the read before it found only where both reads were made in the
// same pod. A read that failed in another pod than the row's was made after a relaunch built the
// issue's pod again, and the work the row names was found on a disk that pod replaced: the row
// keeps no work, so the idle view reads none (pods.IdleDecline.Work) and no rule holds the new pod
// for it. A failed read in the same pod, or in a pod the runtime could not name, keeps the work.
func TestAFailedReadInAnotherPodKeepsNoWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		pod  string
		work bool
	}{
		{name: "another pod", pod: "pod-b"},
		{name: "the same pod", pod: "pod-a", work: true},
		{name: "a pod the runtime could not name", pod: "", work: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			if err := st.PutClaim(ctx, supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion",
				Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: supervise.StateIdle}); err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
			message := IdleReleaseRow{Kind: "role_message", Payload: []byte(`{"role":"legion-legion-legion-208-architect","body":"push your work"}`)}
			if _, err := st.DeclineLeftPodClose(ctx, "legion", rootIssue, IdleReleaseDecline{At: at, Role: claim.RoleArchitect,
				Reason: pods.DeclineUnpushedWork, Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", Commits: 1, PodUID: "pod-a",
				Message: &message}); err != nil {
				t.Fatalf("decline for the work in pod-a: %v", err)
			}
			if _, err := st.DeclineLeftPodClose(ctx, "legion", rootIssue, IdleReleaseDecline{At: at.Add(10 * time.Minute),
				Role: claim.RoleArchitect, Reason: pods.DeclineWorkspaceUnread, PodUID: tc.pod}); err != nil {
				t.Fatalf("decline for a read that failed in %q: %v", tc.pod, err)
			}
			var commits int
			if err := st.Pool().QueryRow(ctx, `select commits from idle_release_declines where issue = $1`, rootIssue).Scan(&commits); err != nil {
				t.Fatalf("read the decline: %v", err)
			}
			view := idleViewOf(t, st, rootIssue)
			if view.Declined.Work != tc.work || (commits == 1) != tc.work || view.Declined.Reason != pods.DeclineWorkspaceUnread {
				t.Fatalf("after a failed read in %q the decline = %+v with %d commits, want work on record %t", tc.pod, view.Declined, commits, tc.work)
			}
		})
	}
}

// A claim this release suspended that something resumed is the pod being used again, and no
// start row says so: a wake resumes a released claim by rule (supervise's RequestRuleResume),
// which moves no last_start_row and leaves the claim ready. Without reading the release's own
// suspended set the teardown would wait on that claim for good — its stored claim may hold a
// process, so the fence waits, and the mark it waits under keeps every idle tick off the pod.
func TestAClaimTheReleaseSuspendedThatSomethingResumedOvertakesIt(t *testing.T) {
	t.Parallel()
	const first, boundary, last = 18, 20, 22
	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
		by    supervise.SuspendSource
		want  ReleaseAct
		cause ReleaseCause
	}{
		{name: "still released", state: supervise.StateSuspended, by: supervise.SuspendedByReleased, want: ReleaseActs},
		{name: "resumed by rule, ready again", state: supervise.StateReady, want: ReleaseCancel, cause: ReleaseCauseOvertaken},
		{name: "resumed by rule, idle again", state: supervise.StateIdle, want: ReleaseCancel, cause: ReleaseCauseOvertaken},
		// A close, a linger or an operator that took the claim over re-stamps the source: that
		// suspension is theirs to end, and the release is over either way.
		{name: "taken over by another path", state: supervise.StateSuspended, by: supervise.SuspendedByDaemon,
			want: ReleaseCancel, cause: ReleaseCauseOvertaken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			markRelease(t, st, first, boundary, last, 0, claim.RoleArchitect)
			if err := st.PutClaim(ctx, supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"),
				Project: "legion", Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, TreeEpoch: 1,
				Generation: 1, State: tc.state, SuspendedBy: tc.by}); err != nil {
				t.Fatal(err)
			}

			// A later role stop of the same release, where the resumed claim is all there is to read.
			stop, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary+1)
			if err != nil {
				t.Fatalf("IssueReleaseStop: %v", err)
			}
			if stop.Act != tc.want || stop.Cause != tc.cause {
				t.Fatalf("the role stop = %s for %s, want %s for %s", stop.Act, stop.Cause, tc.want, tc.cause)
			}

			release := IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: last}
			teardown, err := st.IssueRelease(ctx, "legion", release, record.OutOfWorkflow)
			if tc.want == ReleaseActs {
				if err != nil || teardown.Act != ReleaseActs {
					t.Fatalf("the teardown = %s, %v; want it to act", teardown.Act, err)
				}
				return
			}
			if err != nil || teardown.Act != ReleaseCancel || teardown.Cause != ReleaseCauseOvertaken {
				t.Fatalf("the teardown = %s for %s, %v; want %s for %s", teardown.Act, teardown.Cause, err,
					ReleaseCancel, ReleaseCauseOvertaken)
			}
		})
	}
}

// A cancel resumes the claims its own release suspended and no others. A release that finishes
// leaves its claims `released` until a wake brings them back, so an issue being released again
// can hold claims an earlier release left: resuming those puts a role back into a pod that has
// no work for it, with no wake, no task and no record of why it is there.
func TestACancelResumesNoClaimAnEarlierReleaseSuspended(t *testing.T) {
	t.Parallel()
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	// The implementer was released by an earlier release and never woken; this release was
	// marked over the architect alone, which is the one role its stop suspended.
	releasedClaim(t, st, claim.RoleImplementer)
	releasedClaim(t, st, claim.RoleArchitect)
	markRelease(t, st, first, boundary, last, 0, claim.RoleArchitect)

	cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()})
	if err != nil {
		t.Fatalf("cancel the release: %v", err)
	}
	if resumed := cancelled.Resumed; len(resumed) != 1 || resumed[0] != claim.RoleArchitect {
		t.Fatalf("resumed = %v, want the architect alone: the implementer belongs to the release before this one", resumed)
	}
	if queued := startRows(t, st); len(queued) != 1 || queued[0] != string(claim.RoleArchitect) {
		t.Fatalf("starts queued by the cancel = %v, want the architect's alone", queued)
	}
}

// The mark's suspended set is written by the stops themselves, once each has suspended its
// claim, and a row of a release that is over writes nothing: it is the record of what this
// release did, which the cancel and the release's own record are read from.
func TestAReleasesMarkRecordsTheClaimsItsStopsSuspended(t *testing.T) {
	t.Parallel()
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	markRelease(t, st, first, boundary, last, 0)

	if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, claim.RoleArchitect, boundary); err != nil {
		t.Fatalf("record the architect's suspension: %v", err)
	}
	// The row ran twice: one claim, one entry.
	if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, claim.RoleArchitect, boundary); err != nil {
		t.Fatalf("record the architect's suspension again: %v", err)
	}
	if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, claim.RoleImplementer, boundary+1); err != nil {
		t.Fatalf("record the implementer's suspension: %v", err)
	}
	// A row of a release this mark is not records nothing.
	if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, claim.RoleTester, last+5); err != nil {
		t.Fatalf("record a stale row's suspension: %v", err)
	}

	decision, err := st.IssueReleaseCheck(ctx, "legion", rootIssue, first)
	if err != nil {
		t.Fatalf("read the release's mark through its workspace check: %v", err)
	}
	if len(decision.Suspended) != 2 || decision.Suspended[0] != claim.RoleArchitect || decision.Suspended[1] != claim.RoleImplementer {
		t.Fatalf("the mark's suspended roles = %v, want the architect and the implementer in the order their stops ran", decision.Suspended)
	}
	if decision.Release <= 0 {
		t.Fatalf("the decision's release id = %d, want the id the mark carries", decision.Release)
	}
}

// A wake of the pod taken after the release was marked cancels the release, whichever claim it
// was for. The wake is the fact read (one pod_resumes row above the mark's own), because what a
// wake does to a claim says too little: the outbox's wake of a delivery it holds resumes its
// role by rule (supervise's RequestRuleResume), which queues no start row and moves no
// last_start_row, and a wake for a claim an earlier release left `released` touches no claim of
// this release's suspended set. Here the architect is that claim: R1 released it, this release
// stopped the implementer alone, and a notice for the architect wakes the pod. Without the wake
// read, every claim of the pod reads as standing still, the teardown takes the pod, and the
// implementer stays suspended in a pod that is coming back for its architect.
//
// The wake taken before the mark is the mark's own high-water, so a release is never cancelled
// by a wake that was over before it began.
func TestAWakeTakenAfterTheMarkCancelsTheRelease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// woken is a wake of the architect taken after this release's mark; late holds this
		// release's own pod_releases row back until after that wake, as the lane's order does,
		// and unrecorded leaves it unwritten, as a failed record does.
		woken      bool
		late       bool
		unrecorded bool
		want       ReleaseAct
		cause      ReleaseCause
	}{
		{name: "no wake since the mark", want: ReleaseActs},
		{name: "a wake for a claim an earlier release left released", woken: true,
			want: ReleaseCancel, cause: ReleaseCauseOvertaken},
		// The lane records a release's own row last, after the roles are stopped and the Sandbox
		// suspended, and a write that fails is only logged. So a wake between the stop and that
		// row, and a wake of a release whose row the lane never wrote, reach a record where the
		// newest release is the one before this one - the shape that refused every wake of a role
		// the previous release had woken.
		{name: "a wake before the release's own row is recorded", woken: true, late: true,
			want: ReleaseCancel, cause: ReleaseCauseOvertaken},
		{name: "a wake of a release whose record the lane never wrote", woken: true, unrecorded: true,
			want: ReleaseCancel, cause: ReleaseCauseOvertaken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			releasedClaim(t, st, claim.RoleArchitect)
			releasedClaim(t, st, claim.RoleImplementer)
			at := time.Now().UTC()
			// The pod was woken once before this release: that wake is spent. Each release
			// records its own row as the lane does (outbox.recordPodRelease), because a resume
			// is recorded against the release it ends and a wake reads the newest one.
			recordRelease(t, st, at.Add(-2*time.Hour))
			if taken, err := st.TakePodWake(ctx, "legion", rootIssue, "legion-legion-legion-208-architect", claim.RoleArchitect,
				pods.WakeDelivery, at.Add(-time.Hour), 0); err != nil || !taken {
				t.Fatalf("take the wake before the mark = %t, %v; want it taken", taken, err)
			}

			first, boundary, last := releaseThroughTheStore(t, st, rootIssue)
			if !tc.late && !tc.unrecorded {
				recordRelease(t, st, at)
			}
			if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, claim.RoleImplementer, boundary); err != nil {
				t.Fatalf("record the implementer's suspension: %v", err)
			}
			if tc.woken {
				if taken, err := st.TakePodWake(ctx, "legion", rootIssue, "legion-legion-legion-208-architect", claim.RoleArchitect,
					pods.WakeDelivery, at, 0); err != nil || !taken {
					t.Fatalf("take the wake after the mark = %t, %v; want it taken", taken, err)
				}
			}
			if tc.late {
				recordRelease(t, st, at)
			}

			stop, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
			if err != nil {
				t.Fatalf("IssueReleaseStop: %v", err)
			}
			if stop.Act != tc.want || stop.Cause != tc.cause {
				t.Fatalf("the role stop = %s for %s, want %s for %s", stop.Act, stop.Cause, tc.want, tc.cause)
			}
			// The lane deleted the check and the role stop as it ran them, so the teardown is the
			// release's one row left.
			if _, err := st.Pool().Exec(ctx, `delete from outbox where issue = $1 and id between $2 and $3`,
				rootIssue, first, boundary); err != nil {
				t.Fatalf("finish the release's run rows: %v", err)
			}
			release := IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: last}
			teardown, err := st.IssueRelease(ctx, "legion", release, record.OutOfWorkflow)
			if err != nil {
				t.Fatalf("IssueRelease: %v", err)
			}
			if teardown.Act != tc.want || teardown.Cause != tc.cause {
				t.Fatalf("the teardown = %s for %s, want %s for %s", teardown.Act, teardown.Cause, tc.want, tc.cause)
			}
		})
	}
}

// A release cancelled because its issue is no longer the workflow's queues no resume at all, and
// clears its mark all the same: those claims are the close's to end, and the close is already
// running. A resume queued for them lands above the close's own rows and supersedes them — the
// close's suspension then finishes without acting, its Sandbox is never deleted, and the roles
// of an issue that has left run on in it with their launcher tokens and `gh` credentials.
//
// The sequence is the one production runs: the release's stops have suspended the pod's claims,
// the root is parked while the check runs so the linger's rows are queued above the release's,
// and the release's teardown is the next row of the lane.
func TestACancelOfAnIssueOutOfTheWorkflowQueuesNoResumeSoItsCloseActs(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	releasedClaim(t, st, claim.RoleImplementer)
	first, boundary, last := releaseThroughTheStore(t, st, rootIssue)
	if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, claim.RoleImplementer, boundary); err != nil {
		t.Fatalf("record the implementer's suspension: %v", err)
	}

	// The root is parked: its tree lingers, its phase is done, and its close queues its own rows
	// above the release's (workflow's beginLinger).
	if _, err := st.Pool().Exec(ctx, `update issues set phase = 'done', status = 'backlog',
		linger_until = now() + interval '1 hour' where key = $1`, rootIssue); err != nil {
		t.Fatalf("park the root: %v", err)
	}
	var closeRow int64
	if err := st.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ('issue_suspend', $1, '{"tree":"LEGION-208","generation":1,"treeGeneration":1}', 0, now(), '', '')
		returning id`, rootIssue).Scan(&closeRow); err != nil {
		t.Fatalf("queue the close's own row: %v", err)
	}

	release := IssueReleased{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: last}
	teardown, err := st.IssueRelease(ctx, "legion", release, record.OutOfWorkflow)
	if err != nil || teardown.Act != ReleaseCancel || teardown.Cause != ReleaseCauseUnauthorized {
		t.Fatalf("the teardown = %s for %s, %v; want %s for %s", teardown.Act, teardown.Cause, err,
			ReleaseCancel, ReleaseCauseUnauthorized)
	}
	cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()})
	if err != nil {
		t.Fatalf("carry the cancel out: %v", err)
	}
	if len(cancelled.Resumed) != 0 {
		t.Fatalf("resumed = %v, want none: the claims of an issue that has left are its close's", cancelled.Resumed)
	}
	if queued := startRows(t, st); len(queued) != 0 {
		t.Fatalf("starts queued by the cancel = %v, want none", queued)
	}
	if releaseMarked(t, st) {
		t.Fatal("the cancel queued no resume and left its mark behind; every later decision would read a release that is over")
	}

	// The lane deleted the release's own rows as it ran them, so the only rows left below the
	// close's are its own.
	if _, err := st.Pool().Exec(ctx, `delete from outbox where issue = $1 and id between $2 and $3`,
		rootIssue, first, last); err != nil {
		t.Fatalf("finish the release's rows: %v", err)
	}
	act, err := st.IssueSuspension(ctx, "legion",
		IssueClose{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: closeRow},
		record.OutOfWorkflow)
	if err != nil || !act {
		t.Fatalf("the close = %t, %v; want it to act: its Sandbox deletion is what takes the pod's role Secrets with it", act, err)
	}
}

// recordRelease records one release of the tree's pod, as the outbox lane does when a release
// runs: the row a wake's resume is recorded against.
func recordRelease(t *testing.T, st *Store, at time.Time) {
	t.Helper()
	if _, _, err := st.RecordPodRelease(context.Background(), PodRelease{Project: "legion", Issue: rootIssue,
		Generation: 1, Roles: []string{string(claim.RoleArchitect)}, At: at}); err != nil {
		t.Fatalf("record the pod's release: %v", err)
	}
}
