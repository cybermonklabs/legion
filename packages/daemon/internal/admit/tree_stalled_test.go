package admit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// minimalDeployment is the smallest legion.yaml that loads, under this package's own project
// key. The tests below read `stalled_tree_notice_hours` out of a file through the daemon's own
// loader rather than naming a duration of their own, so the number the pass runs under is the
// number an operator writes, and the default is the loader's own.
const minimalDeployment = `project: LEGION
state_dir: /var/lib/legion
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
dispatch_url: http://127.0.0.1:8080
dispatch_token_file: /var/run/legion/DISPATCH_TOKEN
projects:
  LEGION: {}
gates:
  design: root-issues
github_apps:
  implement:
    app_id: "1"
    private_key: implement-test-key
  review:
    app_id: "2"
    private_key: review-test-key
nats_urls: [nats://127.0.0.1:4222]
`

// deploymentBound is `stalled_tree_notice_hours` as a deployment gives it: lines is what the
// file names beyond the smallest one that loads, so "" is a deployment that sets no bound of
// its own and takes the loader's default.
func deploymentBound(t *testing.T, lines string) time.Duration {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legion.yaml")
	if err := os.WriteFile(path, []byte(minimalDeployment+lines), 0o600); err != nil {
		t.Fatalf("write the deployment file: %v", err)
	}
	cfg, err := config.LoadForValidation(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("load the deployment file: %v", err)
	}
	return cfg.StalledTreeNotice
}

// quietRootRecord is the record of a root of a tree this daemon runs, standing in standing. A
// held root carries the phase it was held from, and a root waiting on a person the ask it waits
// on, because the stored shape check requires each of them.
func quietRootRecord(key, rank string, standing phase.Phase) record.Issue {
	issue := record.Issue{Key: key, Tree: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: key, Phase: standing, Generation: 1, Status: "in_progress",
		DispatchStatus: "in_progress", Rank: rank, HandedOver: true}
	switch standing {
	case phase.Held:
		issue.Hold = &record.Hold{From: phase.Implementing, Reason: record.HoldEscalated}
	case phase.AwaitingPerson:
		issue.Awaiting = &record.Awaiting{Ask: "ask-" + key, Since: record.EventMark{Seq: 9}}
	}
	return issue
}

// seedUnslottedRoot records key as the root of a tree this daemon runs, standing in standing:
// its record and its tree lifecycle open under the workflow, and no admission slot at all —
// the shape of an issue whose worker waits for the cap to free a slot.
func seedUnslottedRoot(t *testing.T, pool *pgxpool.Pool, key, rank string, standing phase.Phase) record.Issue {
	t.Helper()
	issue := quietRootRecord(key, rank, standing)
	putIssue(t, pool, issue)
	seedLifecycle(t, pool, key)
	return issue
}

// seedQuietRoot is seedUnslottedRoot with the admission slot the root's phase keeps
// (record.KeepsSlot), so no fixture here says a retro or an issue awaiting its merge holds one.
// The two write the same record and the same lifecycle, in that order, because one is the other.
func seedQuietRoot(t *testing.T, pool *pgxpool.Pool, key, rank string, standing phase.Phase) record.Issue {
	t.Helper()
	issue := seedUnslottedRoot(t, pool, key, rank, standing)
	if record.KeepsSlot(issue) {
		takeSlot(t, pool, key)
	}
	return issue
}

// seedQuietChild records key as a child of root standing in standing at the Dispatch status
// given. It takes no admission slot: a child that never started holds none, and a child past
// its review holds none either.
func seedQuietChild(t *testing.T, pool *pgxpool.Pool, root, key, rank string, standing phase.Phase, status string) record.Issue {
	t.Helper()
	issue := record.Issue{Key: key, Tree: root, Parent: new(root), Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: key, Phase: standing, Generation: 1,
		Status: status, DispatchStatus: status, Rank: rank}
	putIssue(t, pool, issue)
	return issue
}

// passFacts is the facts and the slots an admission pass judges from, read exactly as the pass
// reads them (blockers.JudgeLine over record.ReadLineFacts): what a Dispatch blocker holds,
// what waits for its own children, and what each issue owes its worker. A test that says a
// tree is left alone for one of those reasons reads the reason here rather than assuming it.
func passFacts(t *testing.T, pool *pgxpool.Pool) (record.LineFacts, []record.Slot) {
	t.Helper()
	var facts record.LineFacts
	var slots []record.Slot
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		issues, err := records.LiveIssues(ctx, tx, testProject)
		if err != nil {
			t.Fatalf("list the live issues: %v", err)
		}
		if slots, err = records.Slots(ctx, tx); err != nil {
			t.Fatalf("list the slots: %v", err)
		}
		if facts, err = record.ReadLineFacts(ctx, tx, records, testProject, issues, nil); err != nil {
			t.Fatalf("read the line facts: %v", err)
		}
		if _, facts, _, err = blockers.JudgeLine(ctx, tx, records, testProject, issues, slots, facts, nil); err != nil {
			t.Fatalf("judge the waiting line: %v", err)
		}
	})
	return facts, slots
}

// seedPullRequest records the pull request of issue in state, as a GitHub event records one.
// Each issue takes a branch of its own, which is the table's own uniqueness rule.
func seedPullRequest(t *testing.T, pool *pgxpool.Pool, issue string, number int, state record.PullRequestState) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		pr := record.PullRequest{Issue: issue, Repo: "acme/widgets", Number: number, Branch: "legion/" + issue,
			HeadSHA: fmt.Sprintf("head-%d", number), Generation: 1, State: state}
		if err := record.NewStore().PutPullRequest(context.Background(), tx, pr); err != nil {
			t.Fatalf("record the pull request of %s: %v", issue, err)
		}
	})
}

// claimStore is the claim store of this pool, closed with the test: the claims the quiet
// measure reads are written through the daemon's own store, as the supervisor writes them.
func claimStore(t *testing.T, pool *pgxpool.Pool) *store.Store {
	t.Helper()
	records, err := store.Open(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(func() { records.Close() })
	return records
}

// seedClaim records the role claim of issue in state, with no step of its own: a claim that has
// stepped nowhere moves no tree, so what each fixture says about its tree is said by the state
// alone.
func seedClaim(t *testing.T, records *store.Store, tree, issue string, role claim.Role, state supervise.ClaimState) {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token of %s: %v", testProject, err)
	}
	token, err := claim.NewToken(project, issue, role)
	if err != nil {
		t.Fatalf("%s token of %s: %v", role, issue, err)
	}
	if err := records.PutClaim(context.Background(), supervise.Claim{Token: token, Project: project, Tree: tree,
		Issue: issue, Role: role, TreeEpoch: 1, Generation: 1, State: state}); err != nil {
		t.Fatalf("record the %s claim of %s in state %s: %v", role, issue, state, err)
	}
}

// settleMovements writes one movement over every record of the project and answers it: the
// newest stamp the seeding left plus a minute, since a movement is only ever written forwards
// (record.Store.MarkIssueMoved). Every tree of the fixture is then exactly as quiet as the
// clock the test measures at says, whatever order the rows were written in.
func settleMovements(t *testing.T, pool *pgxpool.Pool, keys ...string) time.Time {
	t.Helper()
	var newest time.Time
	if err := pool.QueryRow(context.Background(), "select max(moved_at) from issues").Scan(&newest); err != nil {
		t.Fatalf("read the newest movement of the fixture: %v", err)
	}
	settled := newest.Add(time.Minute).UTC()
	for _, key := range keys {
		markMoved(t, pool, key, settled)
	}
	return settled
}

// stalledNotice is one tree-stalled notice as these tests read it: the root it names and the
// reason it carries.
type stalledNotice struct {
	issue  string
	reason string
}

// stalledNotices is every tree-stalled notice in the outbox, oldest first. The other rows a
// pass writes — the tick's own wake, a start the slot repair queues — are not these tests'.
func stalledNotices(t *testing.T, pool *pgxpool.Pool) []stalledNotice {
	t.Helper()
	var told []stalledNotice
	for _, row := range effects(t, pool) {
		notice, isNotice := row.payload.(record.ControllerNotice)
		if !isNotice || notice.Kind != record.TreeStalledNotice {
			continue
		}
		told = append(told, stalledNotice{issue: row.issue, reason: notice.Reason})
	}
	return told
}

// stalledRoots is the roots the tree-stalled notices name, oldest first.
func stalledRoots(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var roots []string
	for _, told := range stalledNotices(t, pool) {
		roots = append(roots, told.issue)
	}
	return roots
}

// publishStalledNotices takes every tree-stalled notice out of the outbox, as the outbox does
// once it has published one. The passes below read an outbox the publisher has emptied, so what
// holds a second notice back is the mark the rule writes on the root and never a notice still
// waiting to be sent.
func publishStalledNotices(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "delete from outbox where kind = $1 and payload->>'kind' = $2",
		string(record.OutboxKindControllerNotice), string(record.TreeStalledNotice)); err != nil {
		t.Fatalf("publish the tree-stalled notices: %v", err)
	}
}

// The trees of TestAQuietTreeIsToldToTheControllerOncePerStall: one stopped tree of three
// issues, one stopped tree whose work is past its review, and one tree that keeps moving.
const (
	stalledRootA  = "LEGION-401"
	stalledChildA = "LEGION-402"
	stalledDoneA  = "LEGION-403"
	stalledRootB  = "LEGION-410"
	movingRoot    = "LEGION-420"
)

// A tree of this daemon's own that has stopped is told to the project's controller once, with
// what the controller needs to decide: the root, how long the tree has been quiet, and each
// issue of it the workflow still runs, with that issue's phase, the role whose turn it is or
// that the phase has no role, and the state of its pull request. A tree quiet for less than the
// deployment's bound is told to nobody.
//
// Once is the whole of the rule. The daemon marks the root with the movement the notice was
// told for, so a later pass of the same stall tells nothing — and neither does a pass of an
// admission built again from the store, as a restart builds one, since the mark is a column of
// the record and not a fact the daemon holds in memory. The tree moving is what ends the stall:
// a movement past the mark is a new stall, and the tree is told again.
//
// The bound is the deployment's own: the number its legion.yaml names, and six hours where it
// names none.
func TestAQuietTreeIsToldToTheControllerOncePerStall(t *testing.T) {
	pool := migratedPool(t)
	bound := deploymentBound(t, "")
	if bound != 6*time.Hour {
		t.Fatalf("a deployment that sets no bound of its own gives %s, want the six hours the loader defaults to", bound)
	}
	admission := quiet(t, 4)
	admission.UseStalledTreeNotice(bound)
	registerController(t, pool)

	// Tree A: a root in implementing on an open pull request, a child waiting for its merge on
	// a merged one, and a child the record shows as done. The done child is no member of the
	// tree — the pass's live read leaves it out, and the measure would drop it in any case —
	// so a late GitHub event on its old pull request moves nothing and the notice never names
	// it.
	seedQuietRoot(t, pool, stalledRootA, "A", phase.Implementing)
	seedQuietChild(t, pool, stalledRootA, stalledChildA, "A1", phase.AwaitingMerge, "in_progress")
	seedQuietChild(t, pool, stalledRootA, stalledDoneA, "A2", phase.Done, record.DoneStatus)
	seedPullRequest(t, pool, stalledRootA, 41, record.PullRequestOpen)
	seedPullRequest(t, pool, stalledChildA, 42, record.PullRequestMerged)
	// Tree B: a root in retro, which holds no admission slot at all under this release, on a
	// merged pull request — the tree this rule was filed for.
	seedQuietRoot(t, pool, stalledRootB, "B", phase.Retro)
	seedPullRequest(t, pool, stalledRootB, 43, record.PullRequestMerged)
	// Tree C: a root in implementing that keeps moving, with no pull request recorded.
	seedQuietRoot(t, pool, movingRoot, "C", phase.Implementing)

	// Every agent of every tree sits idle: an idle claim is no work under way, which is exactly
	// the tree this rule exists to find.
	records := claimStore(t, pool)
	seedClaim(t, records, stalledRootA, stalledRootA, claim.RoleArchitect, supervise.StateIdle)
	seedClaim(t, records, stalledRootA, stalledChildA, claim.RoleImplementer, supervise.StateIdle)
	seedClaim(t, records, stalledRootB, stalledRootB, claim.RoleImplementer, supervise.StateIdle)
	seedClaim(t, records, movingRoot, movingRoot, claim.RoleArchitect, supervise.StateIdle)

	settled := settleMovements(t, pool, stalledRootA, stalledChildA, stalledRootB, movingRoot)
	measuredAt := settled.Add(7 * time.Hour)
	admission.now = func() time.Time { return measuredAt }
	markMoved(t, pool, stalledDoneA, measuredAt)
	markMoved(t, pool, movingRoot, measuredAt.Add(-3*time.Hour))

	tick(t, pool, admission, "stall-1")

	toldOf := func(root string, hours float64, lines ...string) stalledNotice {
		reason := fmt.Sprintf("%s has stopped. The tree has been quiet for %.1f hours. No agent of the tree is at work.", root, hours)
		for _, line := range lines {
			reason += "\n" + line
		}
		return stalledNotice{issue: root, reason: reason + "\nRead the tree and decide what it needs." +
			" You can ask a role what it waits on. You can move an issue back a phase. You can park the tree."}
	}
	want := []stalledNotice{
		toldOf(stalledRootA, 7,
			stalledRootA+" is in implementing. The implementer has the turn. Pull request 41 is open.",
			stalledChildA+" is in awaiting_merge. No role works this phase. Pull request 42 is merged."),
		toldOf(stalledRootB, 7, stalledRootB+" is in retro. The implementer has the turn. Pull request 43 is merged."),
	}
	if got := stalledNotices(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("the controller was told %#v, want %#v", got, want)
	}
	if told := issue(t, pool, stalledRootA).TreeStalledToldAt; told == nil || !told.Equal(settled) {
		t.Fatalf("%s carries the stall mark %v, want the tree's own movement %s: the mark is what holds the daemon to one notice per stall",
			stalledRootA, told, settled.Format(time.RFC3339Nano))
	}

	// A second pass, later, of the same stall tells nothing. The notices of the first pass are
	// out of the outbox, so nothing but the mark can hold this one back.
	publishStalledNotices(t, pool)
	later := measuredAt.Add(time.Hour)
	admission.now = func() time.Time { return later }
	tick(t, pool, admission, "stall-2")
	if got := stalledRoots(t, pool); got != nil {
		t.Fatalf("a second pass of the same stall told the controller of %v, want nothing: the stall was already told", got)
	}

	// Nor does a pass of an admission built again from the store, which is what a restart
	// builds: the mark is a column of the root's record and no daemon's memory.
	restarted := quiet(t, 4)
	restarted.UseStalledTreeNotice(bound)
	restarted.now = func() time.Time { return later }
	tick(t, pool, restarted, "stall-3")
	if got := stalledRoots(t, pool); got != nil {
		t.Fatalf("a pass after a restart told the controller of %v, want nothing: the mark must survive the daemon", got)
	}

	// Tree A moves and stops again, which is a stall of its own: its movement now stands after
	// the mark, and it alone is told. Tree B has not moved since it was told, and tree C goes on
	// moving, so neither is.
	moved := later.Add(7 * time.Hour)
	markMoved(t, pool, stalledRootA, moved)
	stalledAgainAt := moved.Add(7 * time.Hour)
	markMoved(t, pool, movingRoot, stalledAgainAt.Add(-time.Hour))
	admission.now = func() time.Time { return stalledAgainAt }
	tick(t, pool, admission, "stall-4")
	if got, want := stalledRoots(t, pool), []string{stalledRootA}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the tree that moved and stopped again was told as %v, want %v alone", got, want)
	}

	// A deployment that names a bound of its own is told at that number. Tree C is quiet for
	// four hours here: nothing under the six-hour default, and a stall under a two-hour bound.
	publishStalledNotices(t, pool)
	fourHoursQuiet := stalledAgainAt.Add(3 * time.Hour)
	admission.now = func() time.Time { return fourHoursQuiet }
	tick(t, pool, admission, "stall-5")
	if got := stalledRoots(t, pool); got != nil {
		t.Fatalf("the six-hour deployment told the controller of %v, want nothing: no tree has been quiet for six hours", got)
	}
	impatient := quiet(t, 4)
	impatient.UseStalledTreeNotice(deploymentBound(t, "stalled_tree_notice_hours: 2\n"))
	impatient.now = func() time.Time { return fourHoursQuiet }
	tick(t, pool, impatient, "stall-6")
	want = []stalledNotice{toldOf(movingRoot, 4,
		movingRoot+" is in implementing. The implementer has the turn. The record holds no pull request.")}
	if got := stalledNotices(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("the two-hour deployment was told %#v, want %#v: the bound is the deployment's own number", got, want)
	}

	// A root the daemon has just started is named by nothing. It waited in the line carrying
	// the moved_at of the moment a person handed it over — nine hours here, which is how long
	// a full cap can hold a root — and its promotion is what moves it: the tree is opened, the
	// root is written in_progress and its architect's start is queued, so the record says the
	// daemon started it now (record.Store.MarkIssueMoved at the promotion). Were that movement
	// not written, the next pass would read the old stamp, find the tree running with no claim
	// at work and no wait of the daemon's own, and tell the controller that a tree the daemon
	// had started a moment before had stopped.
	publishStalledNotices(t, pool)
	const promotedRoot = "LEGION-430"
	seedWaiting(t, pool, promotedRoot, "D")
	markMoved(t, pool, promotedRoot, fourHoursQuiet.Add(-9*time.Hour))
	promotedAt := fourHoursQuiet.Add(time.Minute)
	admission.now = func() time.Time { return promotedAt }
	tick(t, pool, admission, "promotion")
	if started := issue(t, pool, promotedRoot); started.Status != "in_progress" {
		t.Fatalf("%s stands at %s/%s after the pass, want the promotion that opens its tree: this case is about a root the daemon has just started",
			promotedRoot, started.Phase, started.Status)
	}
	if got := stalledRoots(t, pool); got != nil {
		t.Fatalf("the pass that promoted %s told the controller of %v, want nothing", promotedRoot, got)
	}
	later = promotedAt.Add(time.Hour)
	admission.now = func() time.Time { return later }
	tick(t, pool, admission, "after-the-promotion")
	if got := stalledRoots(t, pool); got != nil {
		t.Fatalf("the pass after the promotion told the controller of %v, want nothing: a tree the daemon started an hour ago has not stopped", got)
	}
}

// Four waits the daemon makes on purpose are never a stall, and seven trees that look like
// them are. A tree every quiet issue of which is held by one of the four is working as
// designed: something the daemon itself knows about will move it, and telling the controller
// it has stopped would send a person to read a tree that is exactly where the rules put it.
//
// The fourth wait, the cap's own hold, stands here in both its shapes, because they are held
// by different rows: the issue whose start is written and marked for a slot the slots table
// names none of, and the issue waiting for its first phase, which holds no start row at all,
// because the start is written by the promotion that grants the slot (the LineFirstPhase arm
// of the pass, which calls Engine.StartFirstPhase). So nothing of that second issue is in
// record.LineFacts.Owed, and a rule that reads the cap's hold from the owed row alone finds it
// held by nothing and tells its tree. That second shape is the commonest one a tree waiting
// for the cap takes — a root at admitted waiting for its children with one released child in
// the line — and eight trees of the INFRA deployment stood in it at 2026-11-11T16:36Z.
//
// The seven told beside them are the shapes a careless rule would silence with the four: an
// issue with an open child of its own that is no children-first wait, because that rule holds
// an issue whose first phase has not started and nothing else; a held issue and an issue
// awaiting its merge, neither of which the daemon holds for anybody; a tree with a notice
// queued to a suspended role, which is a delivery waiting for a role nothing brings back; and
// two mixed trees, each with one issue in a wait of the daemon's and one that has simply
// stopped. The mixed two are what holds the rule to every quiet issue rather than any
// (dispatch://INFRA-2560, R5): a rule answered on one member would silence a tree whose other
// member nothing is going to move. They are the control for the waits as well: the pass that
// leaves the waiting trees alone is a pass that still names every tree that has stopped.
//
// The four waits silence a tree only while the project's line can still move, and the last two
// cases are that clause in both directions. The trees above are a project whose line moves: the
// ones that have stopped hold slots of their own and no wait silences them, so a worker of one
// of them can finish and give its slot back. The deadlocked project below holds nothing but
// trees that wait by design — every slot of it held by an issue one of the four silences, every
// claim of it idle — and no work of it can start again by itself, because a slot is freed by a
// phase or by a status change and both need a worker that needs a slot. Every tree of it is
// named, however its members wait. The control after it is that same project with one agent at
// work, which is a line that can still move, and its trees are told to nobody.
//
// The test is written over the behaviour and not over the predicate: it seeds the trees, runs
// an admission pass past the bound and reads what the controller is told, so a later rule that
// narrows a wait again fails it however the narrowing is spelled.
func TestAWaitByDesignIsNeverToldAsAStall(t *testing.T) {
	pool := migratedPool(t)
	bound := deploymentBound(t, "")
	if bound != 6*time.Hour {
		t.Fatalf("a deployment that sets no bound of its own gives %s, want the six hours the loader defaults to", bound)
	}
	// The cap is exactly the slots the fixture holds, so the two issues below that hold none —
	// the one owing its worker a start and the one waiting for its first phase — wait behind a
	// full cap, which is the fourth wait in each of its shapes.
	admission := quiet(t, 10)
	admission.UseStalledTreeNotice(bound)
	registerController(t, pool)
	records := claimStore(t, pool)
	idle := func(tree, issue string) {
		t.Helper()
		seedClaim(t, records, tree, issue, claim.RoleArchitect, supervise.StateIdle)
	}

	// 1. A root waiting on a person. The tree waits for an answer and only a person moves it.
	const awaiting = "LEGION-501"
	seedQuietRoot(t, pool, awaiting, "A", phase.AwaitingPerson)
	idle(awaiting, awaiting)

	// 2. A child a Dispatch blocked-by link holds where it stands: the waiting line passes over
	// it every pass until the issue it waits on is done. Its root waits for that child under
	// the children-first rule — a blocker holds a child alone (blockers.Holdable), so a tree
	// silent by this wait is one whose root waits too — and the child's own blocker is what
	// carries it: take the blocked-by rule out of the four and this tree is told, because
	// nothing would then stand between the child and a stall.
	const blockedRoot, blockedChild, blocker = "LEGION-510", "LEGION-511", "LEGION-519"
	seedQuietRoot(t, pool, blockedRoot, "B", phase.Admitted)
	blockedApproved := 1
	seedGate(t, pool, record.DesignGate{Issue: blockedRoot, ArtifactID: "artifact-510", LatestVersion: 1,
		ApprovedVersion: &blockedApproved, EverOpened: true})
	seedQuietChild(t, pool, blockedRoot, blockedChild, "B1", phase.Admitted, "todo")
	seedMirror(t, pool, blockedRoot, "in_progress")
	seedMirrorChild(t, pool, blockedChild, "todo", blockedRoot, blocker)
	seedMirror(t, pool, blocker, "in_progress")
	idle(blockedRoot, blockedRoot)

	// 3. A root whose own first phase has not started while a Dispatch child of it is open: the
	// children-first rule holds it until that child is released and leaves the workflow. Its
	// design gate stands open, which is where that wait begins.
	const childrenFirst, openChild = "LEGION-520", "LEGION-521"
	seedQuietRoot(t, pool, childrenFirst, "C", phase.Admitted)
	approved := 1
	seedGate(t, pool, record.DesignGate{Issue: childrenFirst, ArtifactID: "artifact-520", LatestVersion: 1,
		ApprovedVersion: &approved, EverOpened: true})
	seedMirror(t, pool, childrenFirst, "in_progress")
	seedMirrorChild(t, pool, openChild, "backlog", childrenFirst)
	idle(childrenFirst, childrenFirst)

	// 4. An issue whose start is marked for its admission slot while the slots table names no
	// row of it: its worker waits for the cap to free one, which is the waiting line working.
	const owedStart = "LEGION-530"
	seedUnslottedRoot(t, pool, owedStart, "D", phase.Implementing)
	inTx(t, pool, func(tx pgx.Tx) {
		row, err := record.NewOutboxRow(owedStart, record.SuperviseRequest{Op: "start", Tree: owedStart,
			Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing,
			Task: "Implement the change.", NeedsSlot: true}, fixedNow)
		if err != nil {
			t.Fatalf("build the start %s is owed: %v", owedStart, err)
		}
		if err := record.NewStore().Enqueue(context.Background(), tx, row); err != nil {
			t.Fatalf("enqueue the start %s is owed: %v", owedStart, err)
		}
	})
	idle(owedStart, owedStart)

	// 5. A child waiting in the line for the first slot of its first phase, under a root that
	// waits for it: the other shape of the fourth wait, and the one no owed start row names.
	// The root's design gate stands open, which is what puts its released child in the line.
	const firstSlotRoot, firstSlot = "LEGION-760", "LEGION-761"
	seedQuietRoot(t, pool, firstSlotRoot, "L", phase.Admitted)
	firstSlotApproved := 1
	seedGate(t, pool, record.DesignGate{Issue: firstSlotRoot, ArtifactID: "artifact-760", LatestVersion: 1,
		ApprovedVersion: &firstSlotApproved, EverOpened: true})
	seedQuietChild(t, pool, firstSlotRoot, firstSlot, "L1", phase.Admitted, "todo")
	seedMirror(t, pool, firstSlotRoot, "in_progress")
	seedMirrorChild(t, pool, firstSlot, "todo", firstSlotRoot)
	idle(firstSlotRoot, firstSlotRoot)

	// 6. A tree with none of those waits: the plain stopped tree.
	const stopped = "LEGION-540"
	seedQuietRoot(t, pool, stopped, "E", phase.Implementing)
	idle(stopped, stopped)

	// 7. An issue in implementing with an open Dispatch child of its own. The children-first
	// rule holds an issue whose own first phase has not started, and this one is past that, so
	// the open child is no wait of the daemon's: its tree has stopped.
	const working, workingChild = "LEGION-550", "LEGION-551"
	seedQuietRoot(t, pool, working, "F", phase.Implementing)
	seedMirror(t, pool, working, "in_progress")
	seedMirrorChild(t, pool, workingChild, "backlog", working)
	idle(working, working)

	// 8. A held issue. Its architect decides what becomes of it, which is the tree's own work.
	const held = "LEGION-560"
	seedQuietRoot(t, pool, held, "G", phase.Held)
	idle(held, held)

	// 9. An issue awaiting its merge. It waits for GitHub, which is nothing the daemon holds it
	// for, and a merge that never lands is exactly the stall this rule was filed for.
	const awaitingMerge = "LEGION-570"
	seedQuietRoot(t, pool, awaitingMerge, "H", phase.AwaitingMerge)
	idle(awaitingMerge, awaitingMerge)

	// 10. A tree holding a notice queued to a role whose claim is suspended. Only a start held
	// for a slot is a wait of the daemon's; a delivery queued to a role nothing brings back is
	// a tree that has stopped with a message nobody will read.
	const suspended = "LEGION-580"
	seedQuietRoot(t, pool, suspended, "I", phase.Implementing)
	seedClaim(t, records, suspended, suspended, claim.RoleImplementer, supervise.StateSuspended)
	inTx(t, pool, func(tx pgx.Tx) {
		row, err := record.NewOutboxRow(suspended, record.Notice{Kind: "held", Role: claim.RoleImplementer,
			Phase: phase.Implementing, Reason: string(record.HoldEscalated)}, fixedNow)
		if err != nil {
			t.Fatalf("build the notice queued to %s's suspended role: %v", suspended, err)
		}
		if err := record.NewStore().Enqueue(context.Background(), tx, row); err != nil {
			t.Fatalf("enqueue the notice queued to %s's suspended role: %v", suspended, err)
		}
	})

	// 11. A tree whose root waits on a person over a child that has stopped. The first wait is
	// the root's and the root's alone; the child in implementing is nobody's to move, and the
	// tree is told.
	const mixedAwaiting, mixedAwaitingChild = "LEGION-590", "LEGION-591"
	seedQuietRoot(t, pool, mixedAwaiting, "J", phase.AwaitingPerson)
	seedQuietChild(t, pool, mixedAwaiting, mixedAwaitingChild, "J1", phase.Implementing, "in_progress")
	idle(mixedAwaiting, mixedAwaiting)

	// 12. A tree whose child a Dispatch blocked-by link holds where it stands while its root
	// has stopped in implementing. The second wait is the child's, and the root is told with
	// it: the same mixture as the tree above, read from the other side of the tree and through
	// another of the four.
	const mixedBlocked, mixedBlockedChild, mixedBlocker = "LEGION-600", "LEGION-601", "LEGION-609"
	seedQuietRoot(t, pool, mixedBlocked, "K", phase.Implementing)
	seedQuietChild(t, pool, mixedBlocked, mixedBlockedChild, "K1", phase.Admitted, "todo")
	seedMirror(t, pool, mixedBlocked, "in_progress")
	seedMirrorChild(t, pool, mixedBlockedChild, "todo", mixedBlocked, mixedBlocker)
	seedMirror(t, pool, mixedBlocker, "in_progress")
	idle(mixedBlocked, mixedBlocked)

	// The fixture's own premise: each of the four is the wait it says it is, read from the
	// facts the pass itself judges from. Without this, a tree left out for some other reason —
	// a tree the daemon does not run, one that is not quiet — would read here as a wait
	// honoured and prove nothing.
	facts, slots := passFacts(t, pool)
	if _, blocked := facts.Held[blockedChild]; !blocked {
		t.Fatalf("%s is held by no Dispatch blocker: the pass's own facts hold %v", blockedChild, facts.Held)
	}
	if _, waits := facts.ChildWaits[blockedRoot]; !waits {
		t.Fatalf("%s waits for no child of its own: its tree is silent only while both of its issues wait, the pass's own facts hold %v",
			blockedRoot, facts.ChildWaits)
	}
	if _, waits := facts.ChildWaits[childrenFirst]; !waits {
		t.Fatalf("%s waits for no child of its own: the pass's own facts hold %v", childrenFirst, facts.ChildWaits)
	}
	if _, owes := facts.Owed[owedStart]; !owes {
		t.Fatalf("%s owes its worker no start marked for a slot: the pass's own facts hold %v", owedStart, facts.Owed)
	}
	for _, slot := range slots {
		if slot.Issue == owedStart {
			t.Fatalf("%s holds admission slot %d: the fourth wait is a start marked for a slot its issue does not hold", owedStart, slot.Index)
		}
	}
	// The premise of the fourth wait's second shape, read the same way: the child owes no start
	// row and holds no slot, so the owed-start half of the cap's hold does not name it, and it
	// is held by the line alone.
	if _, owes := facts.Owed[firstSlot]; owes {
		t.Fatalf("%s owes a start row, so this fixture is not an issue waiting for its first phase: %v", firstSlot, facts.Owed)
	}
	for _, slot := range slots {
		if slot.Issue == firstSlot {
			t.Fatalf("%s holds one of the cap's rows, so it is not waiting in the line: %#v", firstSlot, slots)
		}
	}
	if _, waits := facts.ChildWaits[firstSlotRoot]; !waits {
		t.Fatalf("%s waits for no child of its own: its tree is silent only while its root waits for the child and the child waits for the cap, the pass's own facts hold %v",
			firstSlotRoot, facts.ChildWaits)
	}
	if !issue(t, pool, awaiting).AwaitingPerson() {
		t.Fatalf("%s does not wait on a person: the first wait is the root that does", awaiting)
	}
	if _, blocked := facts.Held[mixedBlockedChild]; !blocked {
		t.Fatalf("%s is held by no Dispatch blocker: the mixed tree's premise is that one of its two issues waits by design, the pass's own facts hold %v",
			mixedBlockedChild, facts.Held)
	}
	if !issue(t, pool, mixedAwaiting).AwaitingPerson() {
		t.Fatalf("%s does not wait on a person: the mixed tree's premise is that its root does", mixedAwaiting)
	}

	settled := settleMovements(t, pool, awaiting, blockedRoot, blockedChild, childrenFirst, owedStart,
		firstSlotRoot, firstSlot, stopped, working, held, awaitingMerge, suspended, mixedAwaiting,
		mixedAwaitingChild, mixedBlocked, mixedBlockedChild)
	admission.now = func() time.Time { return settled.Add(7 * time.Hour) }

	tick(t, pool, admission, "waits")

	want := []string{stopped, working, held, awaitingMerge, suspended, mixedAwaiting, mixedBlocked}
	if got := stalledRoots(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("the controller was told of %v, want %v: a tree is silent only where every quiet issue of it waits by design, the two mixed trees are told, and %s waits for its children while %s waits in the line for its first slot, which is the cap's own hold",
			got, want, firstSlotRoot, firstSlot)
	}

	// The liveness clause of the silencing rule, in both directions (dispatch://INFRA-2560,
	// R5). The trees above are its first half: they stand in a project whose line can still
	// move, since the trees that have stopped hold slots no wait of the daemon's silences. The
	// two projects below hold nothing but trees that wait by design, which is where the clause
	// decides, and each stands on a database of its own — the clause is a fact of a whole
	// project, so a deadlock seeded beside the trees above would be no deadlock at all.
	const deadAwaiting, deadParent, deadChild = "LEGION-800", "LEGION-810", "LEGION-811"
	// deadlockedProject seeds one such project and the admission that reads it: a root waiting
	// on a person, holding a slot the first wait silences, and a root waiting for the one open
	// Dispatch child under it, holding a slot the third wait silences while that child stands
	// in the line holding none, which is the fourth. The cap is exactly the two slots they
	// hold, so the child in the line is offered neither. architect is the state of the parent's
	// architect claim, which is the one fact the control changes.
	deadlockedProject := func(architect supervise.ClaimState) (*pgxpool.Pool, *Admission) {
		t.Helper()
		deadPool := migratedPool(t)
		registerController(t, deadPool)
		deadAdmission := quiet(t, 2)
		deadAdmission.UseStalledTreeNotice(bound)
		seedQuietRoot(t, deadPool, deadAwaiting, "A", phase.AwaitingPerson)
		seedQuietRoot(t, deadPool, deadParent, "B", phase.Admitted)
		deadApproved := 1
		seedGate(t, deadPool, record.DesignGate{Issue: deadParent, ArtifactID: "artifact-810", LatestVersion: 1,
			ApprovedVersion: &deadApproved, EverOpened: true})
		seedQuietChild(t, deadPool, deadParent, deadChild, "B1", phase.Admitted, "todo")
		seedMirror(t, deadPool, deadParent, "in_progress")
		seedMirrorChild(t, deadPool, deadChild, "todo", deadParent)
		deadRecords := claimStore(t, deadPool)
		seedClaim(t, deadRecords, deadAwaiting, deadAwaiting, claim.RoleArchitect, supervise.StateIdle)
		seedClaim(t, deadRecords, deadParent, deadParent, claim.RoleArchitect, architect)

		// The project's own premise, read from the facts its pass judges from: both slots of
		// the project are held, each by a root one of the four waits silences, and the child
		// holds none and stands in the line, which is the cap's own hold. Without this, a
		// project named for some other reason would read here as a deadlock and prove nothing.
		deadFacts, deadSlots := passFacts(t, deadPool)
		if !issue(t, deadPool, deadAwaiting).AwaitingPerson() {
			t.Fatalf("%s does not wait on a person: the premise is that every slot holder of this project waits by design", deadAwaiting)
		}
		if _, waits := deadFacts.ChildWaits[deadParent]; !waits {
			t.Fatalf("%s waits for no child of its own: the pass's own facts hold %v", deadParent, deadFacts.ChildWaits)
		}
		for _, slot := range deadSlots {
			if slot.Issue != deadAwaiting && slot.Issue != deadParent {
				t.Fatalf("%s holds admission slot %d: the premise is that every slot of this project is held by an issue one of the four waits silences",
					slot.Issue, slot.Index)
			}
		}
		if len(deadSlots) != 2 {
			t.Fatalf("the project holds %#v, want one slot for each of its two roots: the cap is full and no holder of it waits for nothing",
				deadSlots)
		}
		if got, want := waitingLine(t, deadPool, testProject, nil), []string{deadChild}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the waiting line of the project is %v, want %v: the child is the issue the cap itself holds", got, want)
		}
		return deadPool, deadAdmission
	}

	// 13. The deadlocked project. Every slot holder of it is silenced by one of the four waits
	// and every claim of it is idle, so nothing of it can start again by itself, and every tree
	// of it is named with its quiet hours whatever its members wait for.
	deadPool, deadAdmission := deadlockedProject(supervise.StateIdle)
	deadSettled := settleMovements(t, deadPool, deadAwaiting, deadParent, deadChild)
	deadAdmission.now = func() time.Time { return deadSettled.Add(7 * time.Hour) }

	tick(t, deadPool, deadAdmission, "deadlocked")

	const stillness = "The tree has been quiet for 7.0 hours. No agent of the tree is at work."
	const decide = "\nRead the tree and decide what it needs. You can ask a role what it waits on." +
		" You can move an issue back a phase. You can park the tree."
	wantDeadlocked := []stalledNotice{
		{issue: deadAwaiting, reason: deadAwaiting + " has stopped. " + stillness + "\n" + deadAwaiting +
			" is in awaiting_person. No role works this phase. The record holds no pull request." + decide},
		{issue: deadParent, reason: deadParent + " has stopped. " + stillness + "\n" + deadParent +
			" is in admitted. No role works this phase. The record holds no pull request.\n" + deadChild +
			" is in admitted. No role works this phase. The record holds no pull request." + decide},
	}
	if got := stalledNotices(t, deadPool); !reflect.DeepEqual(got, wantDeadlocked) {
		t.Fatalf("the deadlocked project was told %#v, want %#v: where no slot holder of a project can finish and no agent of it is at work, every tree of it is named however its members wait",
			got, wantDeadlocked)
	}

	// 14. The control, which must stay silent: the same project with the parent's architect at
	// work. That agent will give its slot back when it is done, so the project's line can still
	// move and the four waits silence what they are there to silence — neither the tree waiting
	// on a person nor the tree whose architect is working is told.
	livePool, liveAdmission := deadlockedProject(supervise.StateWorking)
	liveSettled := settleMovements(t, livePool, deadAwaiting, deadParent, deadChild)
	liveAdmission.now = func() time.Time { return liveSettled.Add(7 * time.Hour) }

	tick(t, livePool, liveAdmission, "line-moving")

	if got := stalledRoots(t, livePool); got != nil {
		t.Fatalf("the project with an agent at work was told of %v, want nothing: every tree of it waits by design and its line can still move", got)
	}
}

// Every claim state that is work under way keeps its tree out of the report, however long the
// tree has been quiet: an agent in a long turn makes no tool call for hours and its record
// moves nowhere, so the quiet time alone would call its tree stopped. The four resting states
// are the other half of the rule — an idle agent waits to be prompted, and a suspended, failed
// or retired one is not coming back on its own — and a tree with nothing but those has stopped.
func TestAWorkingTreeIsNeverReportedStalled(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 9)
	admission.UseStalledTreeNotice(deploymentBound(t, ""))
	registerController(t, pool)
	records := claimStore(t, pool)

	working := []supervise.ClaimState{supervise.StateQueued, supervise.StateLaunchUncertain, supervise.StateLaunching,
		supervise.StateShimConnected, supervise.StateRegistered, supervise.StateReady, supervise.StateWorking}
	keys := make([]string, 0, len(working)+2)
	for i, state := range working {
		root := fmt.Sprintf("LEGION-%d", 601+i)
		seedQuietRoot(t, pool, root, fmt.Sprintf("A%d", i), phase.Implementing)
		seedClaim(t, records, root, root, claim.RoleImplementer, state)
		keys = append(keys, root)
	}

	// A tree whose claims are all stopped: suspended by the operator, out of a budget, or over.
	const stoppedClaims = "LEGION-610"
	seedQuietRoot(t, pool, stoppedClaims, "B", phase.Implementing)
	seedClaim(t, records, stoppedClaims, stoppedClaims, claim.RoleArchitect, supervise.StateSuspended)
	seedClaim(t, records, stoppedClaims, stoppedClaims, claim.RoleImplementer, supervise.StateFailed)
	seedClaim(t, records, stoppedClaims, stoppedClaims, claim.RoleTester, supervise.StateRetired)
	keys = append(keys, stoppedClaims)

	// A tree whose agents all sit idle, waiting to be prompted by nobody.
	const allIdle = "LEGION-620"
	seedQuietRoot(t, pool, allIdle, "C", phase.Implementing)
	seedClaim(t, records, allIdle, allIdle, claim.RoleArchitect, supervise.StateIdle)
	seedClaim(t, records, allIdle, allIdle, claim.RoleImplementer, supervise.StateIdle)
	keys = append(keys, allIdle)

	settled := settleMovements(t, pool, keys...)
	admission.now = func() time.Time { return settled.Add(7 * time.Hour) }

	tick(t, pool, admission, "states")

	want := []string{stoppedClaims, allIdle}
	if got := stalledRoots(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("the controller was told of %v, want %v: a claim in any state but idle, suspended, failed or retired is work under way",
			got, want)
	}
}
