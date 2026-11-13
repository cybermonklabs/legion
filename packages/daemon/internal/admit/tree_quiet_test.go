package admit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// quietRoot is the root the measure below is read for, with quietChild, a child the workflow
// still runs, and quietDone, a child the record shows as done: the three shapes a tree's
// membership rule has to tell apart.
const (
	quietRoot  = "LEGION-208"
	quietChild = "LEGION-209"
	quietDone  = "LEGION-210"
)

// A tree's quiet time is measured from the facts of its members and from the steps of their
// claims (record.QuietTrees), and nothing else moves it. It is read here through the admission
// pass's own method (Admission.quietTrees), so the store read behind it
// (record.Store.ProjectClaims) and the measure over it are held to together, exactly as a pass
// takes them.
//
// This test reads the tree's quiet time rather than the issue's record, which is what
// TestTheRecordKeepsWhenAnIssueLastMoved holds to: between them, one says what a fact writes on
// an issue and this one says what a tree's stillness is made of. The clock every measure below
// is read at is the admission's own (Admission.now), set far past every fact, so each quiet time
// is a value only the fact it names could have given.
func TestAQuietTreeIsMeasuredFromItsFactsAndItsSteps(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)

	seedSlotted(t, pool, quietRoot, "U")
	// The two children, written after the root, so each row's own stamp is later than the one
	// before it: the done child's is the newest fact in the table, and the first measure below
	// is not it.
	putIssue(t, pool, record.Issue{Key: quietChild, Tree: quietRoot, Project: testProject, Title: quietChild,
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V",
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
	putIssue(t, pool, record.Issue{Key: quietDone, Tree: quietRoot, Project: testProject, Title: quietDone,
		Phase: phase.Done, Generation: 1, Status: record.DoneStatus, Rank: "W",
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
	if record.InWorkflow(issue(t, pool, quietDone)) {
		t.Fatalf("%s is in the workflow: the fixture's third issue must be one the record shows as done, or it proves nothing about a tree's membership", quietDone)
	}

	// Every fact the test writes is after the newest row's own stamp, because a movement is only
	// ever written forwards (record.Store.MarkIssueMoved): a time behind the stamp would be the
	// monotone no-op that rule makes it, and would prove nothing.
	base := movedAt(t, pool, quietDone).Add(time.Minute)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	measuredAt := at(600)
	admission.now = func() time.Time { return measuredAt }

	records, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer records.Close()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token of %s: %v", testProject, err)
	}
	implementer, err := claim.NewToken(project, quietChild, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("implementer token of %s: %v", quietChild, err)
	}
	putClaim := func(token claim.Token, issue, tree string, role claim.Role, state supervise.ClaimState) {
		t.Helper()
		if err := records.PutClaim(ctx, supervise.Claim{Token: token, Project: project, Tree: tree, Issue: issue,
			Role: role, TreeEpoch: 1, Generation: 1, State: state}); err != nil {
			t.Fatalf("record the %s claim of %q in state %s: %v", role, issue, state, err)
		}
	}
	putClaim(awaitingArchitect(t, quietRoot), quietRoot, quietRoot, claim.RoleArchitect, supervise.StateIdle)
	putClaim(implementer, quietChild, quietRoot, claim.RoleImplementer, supervise.StateIdle)
	// The daemon's own controller holds a claim on no issue and in no tree, and it is at work
	// while every agent of the tree sits idle: the measure must place it in no tree at all, or
	// no tree of this project would ever read as still.
	putClaim(claim.ControllerToken(project), "", "", claim.RoleController, supervise.StateWorking)

	// measure is the tree's quiet time and whether it is at work, read as the admission pass
	// reads it: the records of the tree — the done child among them, as the state's own
	// projection hands the measure every record of the project — and one read of the claims.
	measure := func(what string) record.TreeQuiet {
		t.Helper()
		var trees map[string]record.TreeQuiet
		inTx(t, pool, func(tx pgx.Tx) {
			issues, err := record.NewStore().TreeIssues(ctx, tx, quietRoot)
			if err != nil {
				t.Fatalf("read the issues of %s after %s: %v", quietRoot, what, err)
			}
			if trees, err = admission.quietTrees(ctx, tx, issues, admission.now()); err != nil {
				t.Fatalf("measure the trees after %s: %v", what, err)
			}
		})
		quiet, measured := trees[quietRoot]
		if !measured {
			t.Fatalf("the measure after %s names no tree %s, only %v", what, quietRoot, trees)
		}
		return quiet
	}
	assertQuiet := func(what string, got record.TreeQuiet, movement time.Time, atWork bool) {
		t.Helper()
		if !got.MovedAt.Equal(movement) || got.Quiet != measuredAt.Sub(movement) || got.AtWork != atWork {
			t.Fatalf("after %s the tree %s last moved at %s and was quiet for %s, at work = %t; want the movement %s, the quiet %s and at work = %t",
				what, quietRoot, got.MovedAt.Format(time.RFC3339Nano), got.Quiet, got.AtWork,
				movement.Format(time.RFC3339Nano), measuredAt.Sub(movement), atWork)
		}
	}

	// With nothing stepped, the tree is measured from the records of its members: the child's
	// own stamp, which is the newer of the two. The done child's stamp is newer than both and is
	// no part of it, and the controller's working claim puts no tree at work.
	recorded := movedAt(t, pool, quietChild)
	assertQuiet("the records alone", measure("the records alone"), recorded, false)

	// A claim's session activity report says a session is up. It moves neither half of the
	// measure: it is not a step, and a session that reports while its agent waits to be prompted
	// is the very thing a stopped tree looks like.
	reported := claim.SessionActivity{At: at(1), BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"subagent"}}}
	if known, err := records.PutClaimActivity(ctx, implementer, 1, reported); err != nil || !known {
		t.Fatalf("report the activity of the implementer claim of %s = %t, %v", quietChild, known, err)
	}
	if landed := activityAt(t, pool, implementer); !landed.After(recorded) {
		t.Fatalf("the activity report of %s landed at %s, which is not after the tree's movement %s: the report must be the newest fact of the tree for this to prove anything",
			quietChild, landed.Format(time.RFC3339Nano), recorded.Format(time.RFC3339Nano))
	}
	assertQuiet("a claim's session activity report", measure("a claim's session activity report"), recorded, false)

	// A step of a claim moves the tree, although the issue's own recorded time stands still: an
	// agent working through a long phase writes nothing to its record for hours, and its tree
	// has not stopped.
	stepped := supervise.StepRecord{At: at(3), TurnStartedAt: at(2), Tool: "read", CallID: "call-7"}
	if known, err := records.PutClaimStep(ctx, implementer, stepped); err != nil || !known {
		t.Fatalf("record the step of the implementer claim of %s = %t, %v", quietChild, known, err)
	}
	assertQuiet("a claim's step", measure("a claim's step"), at(3), false)
	if got := movedAt(t, pool, quietChild); !got.Equal(recorded) {
		t.Fatalf("the record of %s says it last moved at %s after its claim stepped, want %s unchanged: a step writes the claim and never the issue",
			quietChild, got.Format(time.RFC3339Nano), recorded.Format(time.RFC3339Nano))
	}

	// A claim moved from idle to working and back, with no step and no fact of its issue, moves
	// the tree nowhere: each write stamps the claim row (putClaim's updated_at) and neither the
	// state nor that stamp is a movement. Only the second half of the measure answers it.
	putClaim(implementer, quietChild, quietRoot, claim.RoleImplementer, supervise.StateWorking)
	assertQuiet("a claim moved to working", measure("a claim moved to working"), at(3), true)
	putClaim(implementer, quietChild, quietRoot, claim.RoleImplementer, supervise.StateIdle)
	assertQuiet("a claim moved back to idle", measure("a claim moved back to idle"), at(3), false)

	// A GitHub event on the done child's pull request — a comment, a late check run, a
	// deployment — moves that child's record. The tree is measured from its root and the issues
	// the workflow still runs, so its quiet time stays where it was: a merged pull request
	// nobody has finished writing on would otherwise keep a stopped tree looking busy forever.
	markMoved(t, pool, quietDone, at(5))
	assertQuiet("a GitHub event on the done child's pull request", measure("a GitHub event on the done child's pull request"), at(3), false)

	// A tree whose one issue moves is quiet for no time at all.
	markMoved(t, pool, quietChild, measuredAt)
	moved := measure("the child's own movement")
	assertQuiet("the child's own movement", moved, measuredAt, false)
	if moved.Quiet != 0 {
		t.Fatalf("the tree %s is quiet for %s at the moment its child moved, want no time at all", quietRoot, moved.Quiet)
	}
}

// activityAt is when the claims table says the session of token last reported, read from the
// column the report writes: what the measure must not read.
func activityAt(t *testing.T, pool *pgxpool.Pool, token claim.Token) time.Time {
	t.Helper()
	var at *time.Time
	if err := pool.QueryRow(context.Background(), "select activity_at from claims where token = $1", string(token)).Scan(&at); err != nil {
		t.Fatalf("read the activity of claim %s: %v", token, err)
	}
	if at == nil {
		t.Fatalf("claim %s holds no activity report", token)
	}
	return *at
}

// markMoved records a fact of key at the moment it was made, as every write site in the workflow
// records one (record.Store.MarkIssueMoved), and holds the write to having landed: the rule is
// monotone, so a time behind the row's own would leave it where it stands and the assertion that
// follows would be answered by the old value.
func markMoved(t *testing.T, pool *pgxpool.Pool, key string, at time.Time) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().MarkIssueMoved(context.Background(), tx, key, at); err != nil {
			t.Fatalf("mark %s moved at %s: %v", key, at.Format(time.RFC3339Nano), err)
		}
	})
	if got := movedAt(t, pool, key); !got.Equal(at) {
		t.Fatalf("the record of %s says it last moved at %s, want %s, the moment of the fact",
			key, got.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
	}
}
