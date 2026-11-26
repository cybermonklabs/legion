package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

func TestIssueSuspensionFencesReadmissionAndWaitsForAllStoredRoles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		issue    string
		change   string
		state    supervise.ClaimState
		start    int64
		wantAct  bool
		wantWait string
	}{
		{name: "every role stopped", state: supervise.StateSuspended, wantAct: true},
		{name: "held turn", state: supervise.StateWorking, wantWait: "stored claim"},
		{name: "initial launch", state: supervise.StateLaunching, wantWait: "stored claim"},
		{name: "uncertain launch", state: supervise.StateLaunchUncertain, wantWait: "stored claim"},
		{name: "root readmitted before a new claim", state: supervise.StateSuspended, change: "update issues set generation=2, linger_until=null where key='LEGION-208'"},
		{name: "newer start already ran", state: supervise.StateWorking, start: 11},
		// A queued start row is not a start yet, and most such rows never become one: the outbox's
		// own start fence declines a start of the run this close ended, and its lingering-tree guard
		// declines one written while the tree lingers. Finishing the suspension on a row like that
		// would release nothing and leave the Sandbox, the pod and its disk standing with no row
		// left to try, so the close acts and the declined row costs the tree nothing.
		{name: "a queued start the outbox will decline", state: supervise.StateSuspended, change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values (11,'supervise','LEGION-208','{"op":"start","tree":"LEGION-208","role":"architect","generation":1}',0,now(),'')`, wantAct: true},
		// A production check's look is the one start a lingering tree still runs, so this row will
		// take the pod the close would delete under it. Its role is the implementer's because a
		// look is only ever a role that works a check (record.SuperviseRequest's own validation
		// refuses a checkLook on any other), where the declined row above it is a plain start of
		// the architect; the fence reads the look and not the role.
		{name: "a queued check look start", state: supervise.StateSuspended, change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values (11,'supervise','LEGION-208','{"op":"start","tree":"LEGION-208","role":"implementer","generation":1,"checkLook":1}',0,now(),'')`},
		{name: "role stop still pending", state: supervise.StateSuspended, change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values (9,'supervise','LEGION-208','{"op":"suspend","tree":"LEGION-208","role":"architect","generation":1}',0,now(),'')`, wantWait: "role stop effects"},
		{name: "issue close still pending", state: supervise.StateRetired, change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values (9,'supervise','LEGION-208','{"op":"issue_close","tree":"LEGION-208","role":"architect","generation":1}',0,now(),'')`, wantWait: "role stop effects"},
		// A stopped task's step stop is a role stop like a suspend (workflow's stopTask), so a close
		// waits for it too, rather than deleting the Sandbox under the stop.
		{name: "a stopped task's step stop still pending", state: supervise.StateSuspended, change: `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values (9,'supervise','LEGION-208','{"op":"step_stop","tree":"LEGION-208","role":"architect","generation":1}',0,now(),'')`, wantWait: "role stop effects"},
		{name: "tree cleanup already reserved", state: supervise.StateSuspended, change: `update tree_lifecycles set cleanup_started = true where tree = 'LEGION-208'`},
		// The close deletes the issue's Sandbox in every case, so the state a stop left a claim in
		// decides nothing beyond whether it may still hold a process: the suspended claim of the
		// first case, a failed one and a retired one each let the close through, since the Sandbox
		// owns nothing any of them still needs — its disk went with the pod, and the issue's next
		// launch writes the role Secrets again.
		{name: "a failed claim does not hold the close", state: supervise.StateFailed, wantAct: true},
		{name: "every claim retired", state: supervise.StateRetired, wantAct: true},
		{name: "a close of a run a newer start superseded", state: supervise.StateRetired, start: 11},
		// A member closed under a lingering root keeps a status the workflow runs in, so the run is
		// over by the root's lingering alone and the restart fence is the last thing between the
		// close and the member's Sandbox: a queued start row the outbox will decline must not hold
		// that Sandbox open either.
		{name: "a member of a lingering tree", issue: childIssue, state: supervise.StateSuspended, change: `insert into issues
			(key, tree, project, title, phase, generation, status, rank, linger_until, last_dispatch_seq)
			values ('LEGION-209', 'LEGION-208', 'LEGION', 'member', 'implementing', 1, 'in_progress', 'B', null, 1);
			insert into outbox (id,kind,issue,payload,attempts,next_at,last_error) values (11,'supervise','LEGION-209','{"op":"start","tree":"LEGION-208","role":"implementer","generation":1}',0,now(),'')`, wantAct: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openLingeringTree(t, st)
			// Every case but the member's closes the root itself, which is the issue the tree is
			// named for and the one whose claim token the table's claim carries.
			issue := tc.issue
			if issue == "" {
				issue = rootIssue
			}
			token, err := claim.NewToken("legion", issue, claim.RoleArchitect)
			if err != nil {
				t.Fatal(err)
			}
			c := supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: issue,
				Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: tc.state, LastStartRow: tc.start}
			if err := st.PutClaim(ctx, c); err != nil {
				t.Fatal(err)
			}
			if tc.change != "" {
				if _, err := st.Pool().Exec(ctx, tc.change); err != nil {
					t.Fatal(err)
				}
			}
			reservedBefore := cleanupStarted(t, st)
			act, err := st.IssueSuspension(ctx, "legion", IssueClose{Issue: issue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: 10}, record.OutOfWorkflow)
			if tc.wantWait != "" {
				if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), tc.wantWait) || act {
					t.Fatalf("suspension = %t, %v; want the wait %q", act, err, tc.wantWait)
				}
			} else if err != nil || act != tc.wantAct {
				t.Fatalf("suspension = %t, %v; want act %t", act, err, tc.wantAct)
			}
			if !reservedBefore && cleanupStarted(t, st) {
				t.Fatal("suspension reserved destructive cleanup")
			}
		})
	}
}

func cleanupStarted(t *testing.T, st *Store) bool {
	t.Helper()
	var started bool
	if err := st.Pool().QueryRow(context.Background(), `select cleanup_started from tree_lifecycles
		where project = 'legion' and tree = $1`, rootIssue).Scan(&started); err != nil {
		t.Fatal(err)
	}
	return started
}

// A start of the retro Legion runs after a close is one more start an issue's Sandbox is kept
// for, as a production check's look is: the retro's agent resumes or launches in the pod the
// close would delete (INFRA-5602). So a suspension that finds such a start queued after its own
// row finishes without acting, which is what a look's start does, and the retro's own end queues
// the next close. The retro start is the implementer's own, and the wake is the root
// architect's, for the finding the retro's agent publishes to its role topic while the tree's
// close holds the architect suspended. A start that names neither is the one the outbox declines,
// which the suspension must not wait on: the control row, which acts.
func TestAnIssueSuspensionWaitsForAQueuedRetroStart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		wantAct bool
	}{
		{name: "a queued retro start of the implementer", payload: `{"op":"start","tree":"LEGION-208","role":"implementer","generation":1,"retroRun":1}`},
		{name: "a queued wake of the root architect for an open retro", payload: `{"op":"start","tree":"LEGION-208","role":"architect","generation":1,"retroWake":true,"wakeKind":"role_message"}`},
		{name: "a queued start that is no retro's", payload: `{"op":"start","tree":"LEGION-208","role":"architect","generation":1}`, wantAct: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openLingeringTree(t, st)
			token, err := claim.NewToken("legion", rootIssue, claim.RoleArchitect)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
				Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: supervise.StateSuspended}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Pool().Exec(ctx, `insert into outbox (id,kind,issue,payload,attempts,next_at,last_error)
				values (11,'supervise','LEGION-208',$1::jsonb,0,now(),'')`, tc.payload); err != nil {
				t.Fatal(err)
			}

			act, err := st.IssueSuspension(ctx, "legion", IssueClose{Issue: rootIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: 10}, record.OutOfWorkflow)
			if err != nil || act != tc.wantAct {
				t.Fatalf("suspension with %s queued after it = %t, %v; want act %t", tc.name, act, err, tc.wantAct)
			}
		})
	}
}

// The tester's production check and the implementer's retro after the close are open together
// (the child's pull request merged before its `done`, and its architect's `done` spared both
// roles: INFRA-5602's c12). When the check closes first, its close queues an issue_suspend for the
// child, and the retro's agent still holds the pod: the suspension waits on that agent's claim for
// as long as the claim may run, the working turn and the idle one after it alike, and acts once
// the retro's end closed the claim or the idle pass released the agent, as it acts on any
// issue's claims that all stand stopped. The tester's claim is retired where the check closed
// (the row "the check closes first") and suspended where a look ended pending and left the check
// waiting for its next look (the row "a pending look", whose endLookPending queues the same
// suspension): neither holds the pod, and neither changes what the retro's agent does.
func TestAnIssueSuspensionWaitsForAnOpenRetroAfterItsCheckCloses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checker supervise.Claim
	}{
		{name: "the check closes first", checker: supervise.Claim{State: supervise.StateRetired}},
		{name: "a pending look", checker: supervise.Claim{State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openLingeringTree(t, st)
			if _, err := st.Pool().Exec(ctx, `insert into issues
				(key, tree, project, title, phase, generation, status, rank, linger_until, last_dispatch_seq)
				values ('LEGION-209', 'LEGION-208', 'LEGION', 'child', 'done', 1, 'done', 'B', null, 1)`); err != nil {
				t.Fatal(err)
			}
			implementer, err := claim.NewToken("legion", childIssue, claim.RoleImplementer)
			if err != nil {
				t.Fatal(err)
			}
			tester, err := claim.NewToken("legion", childIssue, claim.RoleTester)
			if err != nil {
				t.Fatal(err)
			}
			put := func(c supervise.Claim) {
				t.Helper()
				c.Project, c.Tree, c.Issue, c.TreeEpoch, c.Generation = "legion", rootIssue, childIssue, 1, 1
				if err := st.PutClaim(ctx, c); err != nil {
					t.Fatal(err)
				}
			}
			checker := tc.checker
			checker.Token, checker.Role = tester, claim.RoleTester
			put(checker)
			put(supervise.Claim{Token: implementer, Role: claim.RoleImplementer, State: supervise.StateWorking})

			opened := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
			records := record.NewStore()
			if err := Transact(ctx, st.pool, func(ctx context.Context, tx pgx.Tx) error {
				if err := records.MarkRunStarted(ctx, tx, childIssue, 1, rootIssue, opened.Add(-2*time.Hour)); err != nil {
					return err
				}
				if err := records.MarkRetroStarted(ctx, tx, childIssue, 1, rootIssue, opened); err != nil {
					return err
				}
				_, err := records.OpenRetroAfterClose(ctx, tx, childIssue, 1, rootIssue, opened, opened.Add(24*time.Hour), implementer, 0)
				return err
			}); err != nil {
				t.Fatalf("open the retro after the close of %s: %v", childIssue, err)
			}

			suspension := func() (bool, error) {
				return st.IssueSuspension(ctx, "legion", IssueClose{Issue: childIssue, Tree: rootIssue, IssueGeneration: 1, TreeGeneration: 1, Row: 10}, record.OutOfWorkflow)
			}
			for _, step := range []struct {
				name     string
				agent    supervise.Claim
				ends     bool
				wantWait bool
			}{
				{name: "while the retro's turn works", agent: supervise.Claim{State: supervise.StateWorking}, wantWait: true},
				{name: "while the turn has ended and the retro is open", agent: supervise.Claim{State: supervise.StateIdle}, wantWait: true},
				{name: "once the idle pass released the agent", agent: supervise.Claim{State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased}},
				{name: "once the retro ended and its claim closed", agent: supervise.Claim{State: supervise.StateRetired}, ends: true},
			} {
				t.Run(step.name, func(t *testing.T) {
					agent := step.agent
					agent.Token, agent.Role = implementer, claim.RoleImplementer
					put(agent)
					if step.ends {
						if err := Transact(ctx, st.pool, func(ctx context.Context, tx pgx.Tx) error {
							_, _, err := records.EndRetroAfterClose(ctx, tx, childIssue, 1, opened.Add(time.Hour), record.RetroOutcomePassed, record.RetroEndReport)
							return err
						}); err != nil {
							t.Fatalf("end the retro after the close of %s: %v", childIssue, err)
						}
					}
					act, err := suspension()
					if step.wantWait {
						if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), string(implementer)) || act {
							t.Fatalf("suspension = %t, %v; want the wait on the retro's agent, claim %s", act, err, implementer)
						}
						return
					}
					if err != nil || !act {
						t.Fatalf("suspension = %t, %v; want it to act", act, err)
					}
				})
			}
		})
	}
}
