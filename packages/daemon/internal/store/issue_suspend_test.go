package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

func TestIssueSuspensionFencesReadmissionAndWaitsForAllStoredRoles(t *testing.T) {
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
