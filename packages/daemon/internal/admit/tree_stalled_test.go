package admit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	return deployment(t, lines).StalledTreeNotice
}

// deployment is the configuration the daemon's own loader reads from the smallest file that
// loads with lines added, for a test that runs under more than one of its numbers.
func deployment(t *testing.T, lines string) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legion.yaml")
	if err := os.WriteFile(path, []byte(minimalDeployment+lines), 0o600); err != nil {
		t.Fatalf("write the deployment file: %v", err)
	}
	cfg, err := config.LoadForValidation(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("load the deployment file: %v", err)
	}
	return cfg
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
		if _, facts, _, _, err = blockers.JudgeLine(ctx, tx, records, testProject, issues, slots, facts, nil, fixedNow); err != nil {
			t.Fatalf("judge the waiting line: %v", err)
		}
	})
	return facts, slots
}

// passWords is the one word the waiting line's own walk gives each issue it leaves behind, by
// issue key (record.HeldReason, record.Waiting's second answer), read from the walk the
// admission pass hands the measure (blockers.JudgeLine, tellStalledTrees) over the facts and the
// slots passFacts gave. The walk can leave one issue behind at two of its sites, and the first
// row of a key is the one kept, as record.HeldList and the measure keep it. A case that says a
// member of a tree is held by one of the daemon's own waits reads the word here rather than
// assuming which of them its fixture built.
func passWords(t *testing.T, pool *pgxpool.Pool, facts record.LineFacts, slots []record.Slot) map[string]record.HeldReason {
	t.Helper()
	words := make(map[string]record.HeldReason)
	inTx(t, pool, func(tx pgx.Tx) {
		issues, err := record.NewStore().LiveIssues(context.Background(), tx, testProject)
		if err != nil {
			t.Fatalf("list the live issues: %v", err)
		}
		_, dropped := record.Waiting(issues, slots, facts)
		for _, row := range dropped {
			if _, seen := words[row.Issue]; !seen {
				words[row.Issue] = row.Reason
			}
		}
	})
	return words
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
	seedQuietChild(t, pool, stalledRootA, stalledChildA, "A1", phase.Merging, "in_progress")
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
			stalledChildA+" is in merging. The merger has the turn. Pull request 42 is merged."),
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
// The test of that fourth wait is the need and not the mechanism, which is why two more trees
// stand here that no row of the line names: a root waiting on a person with one released child
// at admitted under it, and a tree whose design gate has closed again over a member at
// admitted. The daemon keeps each of those members out of the waiting line itself — it starts
// nothing of a tree whose root waits on a person, and nothing under a closed design gate — so
// each needs a slot, holds none and stands in no entry of the line that could say so. Read
// from the line alone, both trees were told as stopped although the daemon was holding their
// members itself.
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
// The four waits silence a tree only while the project's line can still move, save a wait on
// work outside the tree, which silences it whatever the line does
// (lineHolds.waitsOnWorkOutsideItsTree, TestATreeThatWaitsOnABlockedByLinkIsNeverToldAsStopped),
// and the last two cases are that clause in both directions. The trees above are a project
// whose line moves: the ones that have stopped hold slots of their own and no wait silences
// them, so a worker of one of them can finish and give its slot back. The deadlocked project
// below holds nothing but trees that wait by design — every slot of it held by an issue one of
// the four silences, every claim of it idle — and no work of it can start again by itself,
// because a slot is freed by a phase or by a status change and both need a worker that needs a
// slot. None of its members waits on work outside its tree, so every tree of it is named. The
// control after it is that same project with one agent at work, which is a line that can still
// move, and its trees are told to nobody.
//
// The test is written over the behaviour and not over the predicate: it seeds the trees, runs
// an admission pass past the bound and reads what the controller is told, so a later rule that
// narrows a wait again fails it however the narrowing is spelled.
func TestAWaitByDesignIsNeverToldAsAStall(t *testing.T) {
	bound := deploymentBound(t, "")
	if bound != 6*time.Hour {
		t.Fatalf("a deployment that sets no bound of its own gives %s, want the six hours the loader defaults to", bound)
	}
	// The three readings below stand in subtests of their own, each on a database of its own,
	// because a failure of one says nothing about the others: a t.Fatalf of the waits scenario
	// ends its own subtest alone, where before it ended the function and left the liveness
	// clause unread and reported under the waits' name. The test's own name is unchanged, since
	// the issue's verification check runs it by that name.

	// The waits themselves, and the seven trees a careless rule would silence with them: the
	// scenario this test is named for, read from one project whose line can still move.
	t.Run("waits", func(t *testing.T) {
		pool := migratedPool(t)
		// The cap is exactly the slots the fixture holds, so the two issues below that hold none —
		// the one owing its worker a start and the one waiting for its first phase — wait behind a
		// full cap, which is the fourth wait in each of its shapes. The two trees the daemon holds
		// itself, below, each add a slotted root and no slot of their own members.
		admission := quiet(t, 12)
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

		// 9. An issue in its merge. Its merger merges the head it read, so a merge that never
		// lands is exactly the stall this rule was filed for, and it is no wait of the daemon's.
		const awaitingMerge = "LEGION-570"
		seedQuietRoot(t, pool, awaitingMerge, "H", phase.Merging)
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

		// 13. A root waiting on a person with one released child of its own standing at admitted.
		// The daemon starts nothing of a tree whose root waits, so it keeps that child out of the
		// waiting line itself: the child needs a slot, holds none, and the slot it waits for comes
		// with its root's answer. Its tree's design gate stands open, so the root's wait is the
		// only thing holding it.
		const personRoot, personChild = "LEGION-610", "LEGION-611"
		seedQuietRoot(t, pool, personRoot, "M", phase.AwaitingPerson)
		personApproved := 1
		seedGate(t, pool, record.DesignGate{Issue: personRoot, ArtifactID: "artifact-610", LatestVersion: 1,
			ApprovedVersion: &personApproved, EverOpened: true})
		seedQuietChild(t, pool, personRoot, personChild, "M1", phase.Admitted, "todo")
		seedMirror(t, pool, personRoot, "in_progress")
		seedMirrorChild(t, pool, personChild, "todo", personRoot)
		idle(personRoot, personRoot)

		// 14. A tree whose design gate has closed again over a member standing at admitted: its
		// architect is revising the spec the tree's work must follow, and nothing of the tree
		// starts until a person approves that version. The gate has opened before, so the root is
		// no root waiting for its tree to be opened and the tree is one the daemon runs.
		const gatedRoot, gatedChild = "LEGION-620", "LEGION-621"
		seedQuietRoot(t, pool, gatedRoot, "N", phase.Admitted)
		gatedApproved := 1
		seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: "artifact-620", LatestVersion: 2,
			ApprovedVersion: &gatedApproved, EverOpened: true})
		seedQuietChild(t, pool, gatedRoot, gatedChild, "N1", phase.Admitted, "todo")
		seedMirror(t, pool, gatedRoot, "in_progress")
		seedMirrorChild(t, pool, gatedChild, "todo", gatedRoot)
		idle(gatedRoot, gatedRoot)

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
		// The premise of the two the daemon holds itself, read the same way and from the one walk
		// the measure reads (record.Waiting's second answer, passWords): each member stands in no
		// entry of the line and holds none of the cap's rows, so no reading of the line can say
		// what holds it, and the walk names the word that does. Each case is driven by a word of
		// its own, so neither stands in for the other.
		words := passWords(t, pool, facts, slots)
		line := waitingLine(t, pool, testProject, nil)
		for _, member := range []struct {
			key  string
			word record.HeldReason
		}{{personChild, record.HeldRootAwaitingPerson}, {gatedChild, record.HeldDesignGate}} {
			if slices.Contains(line, member.key) {
				t.Fatalf("%s stands in the waiting line %v: the daemon keeps it out of the line, which is why no entry of the line names what holds it",
					member.key, line)
			}
			for _, slot := range slots {
				if slot.Issue == member.key {
					t.Fatalf("%s holds admission slot %d: a member the daemon keeps out of its line holds none", member.key, slot.Index)
				}
			}
			if got := words[member.key]; got != member.word {
				t.Fatalf("the line leaves %s behind for %q, want %q: this case is driven by that word and by no other", member.key, got, member.word)
			}
		}
		// The root waiting on the person is decided by that fact and its slot, not by a word:
		// it holds one of the cap's rows, so the walk places it among the slot holders, and a
		// slot holder waiting on a person is the one holder the measure counts as waiting.
		if !issue(t, pool, personRoot).AwaitingPerson() || !slices.ContainsFunc(slots, func(slot record.Slot) bool { return slot.Issue == personRoot }) {
			t.Fatalf("%s is not a slot holder waiting on a person (slots %#v): the tree is silent only while its root waits on the person as well",
				personRoot, slots)
		}
		if got := words[gatedRoot]; got != record.HeldDesignGate {
			t.Fatalf("the line leaves %s behind for %q, want %q: the tree is silent only while the closed gate holds its root as well",
				gatedRoot, got, record.HeldDesignGate)
		}

		settled := settleMovements(t, pool, awaiting, blockedRoot, blockedChild, childrenFirst, owedStart,
			firstSlotRoot, firstSlot, stopped, working, held, awaitingMerge, suspended, mixedAwaiting,
			mixedAwaitingChild, mixedBlocked, mixedBlockedChild, personRoot, personChild, gatedRoot, gatedChild)
		admission.now = func() time.Time { return settled.Add(7 * time.Hour) }

		tick(t, pool, admission, "waits")

		want := []string{stopped, working, held, awaitingMerge, suspended, mixedAwaiting, mixedBlocked}
		if got := stalledRoots(t, pool); !reflect.DeepEqual(got, want) {
			t.Fatalf("the controller was told of %v, want %v: a tree is silent only where every quiet issue of it waits by design, the two mixed trees are told, and %s waits for its children while %s waits in the line for its first slot, which is the cap's own hold",
				got, want, firstSlotRoot, firstSlot)
		}
	})

	// The liveness clause of the silencing rule, in both directions (dispatch://INFRA-2560,
	// R5). The trees above are its first half: they stand in a project whose line can still
	// move, since the trees that have stopped hold slots no wait of the daemon's silences. The
	// two projects below hold nothing but trees that wait by design, which is where the clause
	// decides, and each stands on a database of its own — the clause is a fact of a whole
	// project, so a deadlock seeded beside the trees above would be no deadlock at all.
	const deadAwaiting, deadParent, deadChild = "LEGION-800", "LEGION-810", "LEGION-811"
	// deadlockedProject seeds one such project and the admission that reads it: a root waiting
	// on a person, holding a slot the first wait silences, and a root at admitted with one open
	// Dispatch child under it. The cap is exactly the two slots they hold. opened says whether
	// the parent's design gate has opened. Where it has not, the parent is its architect writing
	// the design, which keeps its row through any wait (blockers.waitsOf), and the closed gate
	// holds the parent and the child alike (design_gate, the second wait): every slot of the
	// project is held by an issue a wait silences and no slot is free, which is the deadlock.
	// Where it has opened, the parent's own first phase waits for the child (children_first), and
	// a started issue that waits on another issue gives its row back (releaseSlotsForWaits): the
	// pass frees a slot before the measure and the child, standing in the line holding none,
	// takes it on that same pass, so that project is no deadlock under the composed rule, which
	// is the case the liveness clause's free-slot term answers. architect is the state of the
	// parent's architect claim, which is the one fact the control changes. t is the subtest's
	// own, so the database it opens is closed with that subtest and a premise it refuses is
	// refused there rather than in the test that owns neither.
	deadlockedProject := func(t *testing.T, architect supervise.ClaimState, opened bool) (*pgxpool.Pool, *Admission) {
		t.Helper()
		deadPool := migratedPool(t)
		registerController(t, deadPool)
		deadAdmission := quiet(t, 2)
		deadAdmission.UseStalledTreeNotice(bound)
		seedQuietRoot(t, deadPool, deadAwaiting, "A", phase.AwaitingPerson)
		seedQuietRoot(t, deadPool, deadParent, "B", phase.Admitted)
		gate := record.DesignGate{Issue: deadParent, ArtifactID: "artifact-810", LatestVersion: 1}
		if opened {
			deadApproved := 1
			gate.ApprovedVersion, gate.EverOpened = &deadApproved, true
		}
		seedGate(t, deadPool, gate)
		seedQuietChild(t, deadPool, deadParent, deadChild, "B1", phase.Admitted, "todo")
		seedMirror(t, deadPool, deadParent, "in_progress")
		seedMirrorChild(t, deadPool, deadChild, "todo", deadParent)
		deadRecords := claimStore(t, deadPool)
		seedClaim(t, deadRecords, deadAwaiting, deadAwaiting, claim.RoleArchitect, supervise.StateIdle)
		seedClaim(t, deadRecords, deadParent, deadParent, claim.RoleArchitect, architect)

		// The project's own premise, read from the facts its pass judges from before any pass
		// runs: both slots of the project are held, each by a root one of the four waits
		// silences. Without this, a project named for some other reason would read here as a
		// deadlock and prove nothing.
		deadFacts, deadSlots := passFacts(t, deadPool)
		if !issue(t, deadPool, deadAwaiting).AwaitingPerson() {
			t.Fatalf("%s does not wait on a person: the premise is that every slot holder of this project waits by design", deadAwaiting)
		}
		for _, slot := range deadSlots {
			if slot.Issue != deadAwaiting && slot.Issue != deadParent {
				t.Fatalf("%s holds admission slot %d: the premise is that every slot of this project is held by an issue one of the four waits silences",
					slot.Issue, slot.Index)
			}
		}
		if len(deadSlots) != 2 {
			t.Fatalf("the project holds %#v, want one slot for each of its two roots: the cap is full", deadSlots)
		}
		if opened {
			if _, waits := deadFacts.ChildWaits[deadParent]; !waits {
				t.Fatalf("%s waits for no child of its own: the pass's own facts hold %v", deadParent, deadFacts.ChildWaits)
			}
			if got, want := waitingLine(t, deadPool, testProject, nil), []string{deadChild}; !reflect.DeepEqual(got, want) {
				t.Fatalf("the waiting line of the project is %v, want %v: the child waits for a slot", got, want)
			}
			return deadPool, deadAdmission
		}
		words := passWords(t, deadPool, deadFacts, deadSlots)
		for _, key := range []string{deadParent, deadChild} {
			if words[key] != record.HeldDesignGate {
				t.Fatalf("the line leaves %s behind for %q, want %q: the closed gate is what holds the parent's tree", key, words[key], record.HeldDesignGate)
			}
		}
		if got := waitingLine(t, deadPool, testProject, nil); len(got) != 0 {
			t.Fatalf("the waiting line of the project is %v, want it empty: a free slot would have an issue to take it", got)
		}
		return deadPool, deadAdmission
	}

	// 15. The deadlocked project. Every slot holder of it is silenced by one of the four waits
	// and every claim of it is idle, so nothing of it can start again by itself, and every tree
	// of it is named with its quiet hours whatever its members wait for.
	t.Run("deadlocked", func(t *testing.T) {
		deadPool, deadAdmission := deadlockedProject(t, supervise.StateIdle, false)
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
			t.Fatalf("the deadlocked project was told %#v, want %#v: where no slot holder of a project can finish and no agent of it is at work, every tree of it is named save one that waits on work outside it, and no member here does",
				got, wantDeadlocked)
		}
	})

	// 16. The control, which must stay silent: the same project with the parent's architect at
	// work. That agent will give its slot back when it is done, so the project's line can still
	// move and the four waits silence what they are there to silence — neither the tree waiting
	// on a person nor the tree whose architect is working is told.
	t.Run("line-moving", func(t *testing.T) {
		livePool, liveAdmission := deadlockedProject(t, supervise.StateWorking, false)
		liveSettled := settleMovements(t, livePool, deadAwaiting, deadParent, deadChild)
		liveAdmission.now = func() time.Time { return liveSettled.Add(7 * time.Hour) }

		tick(t, livePool, liveAdmission, "line-moving")

		if got := stalledRoots(t, livePool); got != nil {
			t.Fatalf("the project with an agent at work was told of %v, want nothing: every tree of it waits by design and its line can still move", got)
		}
	})

	// 16a. The same two trees with the parent's design gate open: the parent's own first phase
	// waits for its open child, and a started issue that waits on another issue gives its row
	// back (releaseSlotsForWaits) on the pass, before the measure. Every claim is idle and every
	// remaining slot holder waits by design, so the claim term and the slot term both fail, but
	// a slot now stands free and the child in the line waits on nothing, so the promotion that
	// follows the measure starts it on that same pass: the line moves, the free-slot term says
	// so, and neither tree is told. Without that term this project read as a deadlock on the very
	// pass that promotes one of its issues.
	t.Run("released-slot-moves-the-line", func(t *testing.T) {
		freePool, freeAdmission := deadlockedProject(t, supervise.StateIdle, true)
		freeSettled := settleMovements(t, freePool, deadAwaiting, deadParent, deadChild)
		freeAdmission.now = func() time.Time { return freeSettled.Add(7 * time.Hour) }

		tick(t, freePool, freeAdmission, "released-slot-moves-the-line")

		if got := issue(t, freePool, deadParent); got.ReleasedForWaitAt == nil || holdsSlot(t, freePool, deadParent) {
			t.Fatalf("%s = %+v holding a slot %t, want it released for the wait on its child: the premise of this case", deadParent,
				got, holdsSlot(t, freePool, deadParent))
		}
		if !holdsSlot(t, freePool, deadChild) {
			t.Fatalf("%s holds no slot after the pass, want the one its parent gave back: the premise is that the line moved on this pass", deadChild)
		}
		if got := stalledRoots(t, freePool); got != nil {
			t.Fatalf("the project was told of %v, want nothing: a slot a wait gave back and an issue of the line to take it is the line moving", got)
		}
	})

	// The liveness clause reads "at work" as R6 defines it (dispatch://INFRA-2560, R6, which the
	// architect ruled reaches this clause too): a phase worker the daemon names silent — up
	// between turns in ready, holding no task, its last step older than
	// `worker_silence_minutes` (workflow.SilentPhaseWorker) — makes no phase change and no
	// status change, so it gives no slot back and keeps the line moving no more than an idle
	// claim does. silentWorkerProject seeds the deadlocked project above with one tree more: a
	// root in merging, which holds no slot, as an issue past its review holds none, so no slot
	// holder of the project is added, and whose merger is that worker, its last step two hours
	// before the movement the clock is measured from. silence is the `worker_silence_minutes`
	// the admission registers, zero for an admission that registers none.
	const deadMerging = "LEGION-820"
	silentWorkerProject := func(t *testing.T, silence, background time.Duration) (*pgxpool.Pool, *Admission) {
		t.Helper()
		silentPool, silentAdmission := deadlockedProject(t, supervise.StateIdle, false)
		if silence > 0 {
			silentAdmission.UseWorkerSilence(silence, background)
		}
		merging := seedQuietRoot(t, silentPool, deadMerging, "C", phase.Merging)
		merger := putWorkerClaim(t, silentPool, deadMerging, deadMerging, claim.RoleMerger, merging.Generation,
			supervise.StateReady, "")
		if _, silentSlots := passFacts(t, silentPool); len(silentSlots) != 2 {
			t.Fatalf("the project holds %#v, want the two slots of the deadlocked project alone: the root in merging must hold none", silentSlots)
		}
		silentSettled := settleMovements(t, silentPool, deadAwaiting, deadParent, deadChild, deadMerging)
		steppedAt(t, silentPool, merger, silentSettled.Add(-2*time.Hour))
		silentAdmission.now = func() time.Time { return silentSettled.Add(7 * time.Hour) }
		return silentPool, silentAdmission
	}

	// 17. The deadlocked project whose one claim at work by its state is a merger the daemon
	// names silent, under the shipped `worker_silence_minutes`. No slot holder keeps the line
	// moving and that worker does not either, so every tree of it is named: the two that wait
	// by design, and the tree of the silent merger, which nothing of it keeps at work.
	t.Run("silent-worker-deadlocked", func(t *testing.T) {
		shipped := deployment(t, "")
		silentPool, silentAdmission := silentWorkerProject(t, shipped.WorkerSilence, shipped.WorkerBackground)

		tick(t, silentPool, silentAdmission, "silent-worker-deadlocked")

		want := []string{deadAwaiting, deadParent, deadMerging}
		if got := stalledRoots(t, silentPool); !reflect.DeepEqual(got, want) {
			t.Fatalf("the project whose only claim at work by its state is a silent merger was told of %v, want %v: a phase worker the daemon names silent is no work under way, so it keeps no line moving and every tree of a project no slot holder moves is named",
				got, want)
		}
	})

	// 18. The same project under an admission that registers no `worker_silence_minutes`, the
	// setting off: no claim is named silent, so the merger in ready is work under way as its
	// state says. Its tree is at work and is not named, and the project's line moves, so the two
	// trees that wait by design are silenced, exactly as before R6.
	t.Run("silent-worker-bound-zero", func(t *testing.T) {
		offPool, offAdmission := silentWorkerProject(t, 0, 0)

		tick(t, offPool, offAdmission, "silent-worker-bound-zero")

		if got := stalledRoots(t, offPool); got != nil {
			t.Fatalf("the project was told of %v with no silence bound registered, want nothing: with the setting off a claim in ready is work under way, its tree is not named and the line it keeps moving silences the waits",
				got)
		}
	})
}

// Every claim state that is work under way keeps its tree out of the report, however long the
// tree has been quiet: an agent in a long turn makes no tool call for hours and its record
// moves nowhere, so the quiet time alone would call its tree stopped. The four resting states
// are the other half of the rule — an idle agent waits to be prompted, and a suspended, failed
// or retired one is not coming back on its own — and a tree with nothing but those has stopped.
//
// R6 (dispatch://INFRA-2560) adds one exception, and the second pass below holds it with its
// control: a phase worker the daemon itself names silent — up between turns in ready, holding
// no task, its last step older than `worker_silence_minutes` (workflow.SilentPhaseWorker) — is
// no work under way, so a tree whose only claim is that worker is named; the same tree with
// that worker's last step inside the bound is working and is not.
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

	// The second pass runs under a deployment whose `worker_silence_minutes` is longer than its
	// `stalled_tree_notice_hours`, which the loader takes (the silence bound stands above its
	// floor and the background bound above it). That is the only shape in which the control
	// says anything: a step moves its tree as well (record.QuietTrees), so under the shipped
	// bounds — half an hour of silence against six hours of quiet — a worker inside the silence
	// bound keeps its whole tree inside the quiet bound, and the control would be unnamed for its
	// quiet time alone, however the measure read a claim in ready. Here both trees are quiet
	// for seven hours, past the six, and the two workers differ in the age of their last step
	// alone: nine hours, past the eight, and seven and a half, inside it.
	t.Run("silent phase worker", func(t *testing.T) {
		silentPool := migratedPool(t)
		cfg := deployment(t, "worker_silence_minutes: 480\nworker_background_hours: 12\n")
		if cfg.WorkerSilence != 8*time.Hour || cfg.StalledTreeNotice != 6*time.Hour {
			t.Fatalf("the deployment gives a silence bound of %s and a quiet bound of %s, want eight hours and six",
				cfg.WorkerSilence, cfg.StalledTreeNotice)
		}
		silentAdmission := quiet(t, 2)
		silentAdmission.UseStalledTreeNotice(cfg.StalledTreeNotice)
		silentAdmission.UseWorkerSilence(cfg.WorkerSilence, cfg.WorkerBackground)
		registerController(t, silentPool)

		// A tree whose only claim is its implementer, up between turns in ready with no task,
		// whose last step is past the silence bound: the worker the daemon names silent.
		const silentReady = "LEGION-630"
		silent := seedQuietRoot(t, silentPool, silentReady, "D", phase.Implementing)
		silentToken := putWorkerClaim(t, silentPool, silentReady, silentReady, claim.RoleImplementer,
			silent.Generation, supervise.StateReady, "")

		// The control: the same tree, with that worker's last step inside the silence bound.
		const steppedReady = "LEGION-640"
		stepped := seedQuietRoot(t, silentPool, steppedReady, "E", phase.Implementing)
		steppedToken := putWorkerClaim(t, silentPool, steppedReady, steppedReady, claim.RoleImplementer,
			stepped.Generation, supervise.StateReady, "")

		silentSettled := settleMovements(t, silentPool, silentReady, steppedReady)
		steppedAt(t, silentPool, silentToken, silentSettled.Add(-2*time.Hour))
		steppedAt(t, silentPool, steppedToken, silentSettled.Add(-30*time.Minute))
		silentAdmission.now = func() time.Time { return silentSettled.Add(7 * time.Hour) }

		tick(t, silentPool, silentAdmission, "silent-ready")

		wantSilent := []string{silentReady}
		if got := stalledRoots(t, silentPool); !reflect.DeepEqual(got, wantSilent) {
			t.Fatalf("the controller was told of %v, want %v: a phase worker in ready that the daemon names silent is no work under way, and one whose last step is inside worker_silence_minutes is",
				got, wantSilent)
		}
	})
}

// A started issue that gave its admission slot back to wait on another issue (blockers.waitsOf,
// releaseSlotsForWaits) is a wait by design: a blocker holds it, and it stands in the waiting
// line as record.LineWaitingOnIssue holding no slot, which is the line's own membership and so
// no word of record.HeldReason (silencedTree's doc says why the table gains no row for it). Its
// tree is told to nobody while the project's line can move, and, since the link is work outside
// the tree, where the line cannot move as well (lineHolds.waitsOnWorkOutsideItsTree,
// TestATreeThatWaitsOnABlockedByLinkIsNeverToldAsStopped). The same link on an issue past its
// review is no such wait: that issue holds no slot by the phase rule (record.KeepsSlot), needs
// none and stands in no line, so the walk leaves it behind as an issue already started that has
// stopped, and its tree is told. A third tree that has simply stopped holds a slot no wait
// silences, which keeps the project's line moving and is told as the control.
//
// The release runs on a first pass at the moment the movements settle, and the measure is read
// on a second pass seven hours later, so a release that marked a movement could not hide the
// tree it released behind a quiet time inside the bound.
func TestAStartedIssueThatGaveItsSlotBackForAWaitIsAWaitByDesign(t *testing.T) {
	const (
		released = "LEGION-701"
		pastRev  = "LEGION-710"
		stopped  = "LEGION-720"
		blocker  = "LEGION-709"
	)
	pool := migratedPool(t)
	bound := deploymentBound(t, "")
	admission := quiet(t, 4)
	admission.UseStalledTreeNotice(bound)
	registerController(t, pool)
	records := claimStore(t, pool)

	seedMirror(t, pool, blocker, "in_progress")
	seedQuietRoot(t, pool, released, "A", phase.Implementing)
	seedMirror(t, pool, released, "in_progress", blocker)
	seedClaim(t, records, released, released, claim.RoleImplementer, supervise.StateIdle)
	seedQuietRoot(t, pool, pastRev, "B", phase.Merging)
	seedMirror(t, pool, pastRev, "in_progress", blocker)
	seedClaim(t, records, pastRev, pastRev, claim.RoleMerger, supervise.StateIdle)
	seedQuietRoot(t, pool, stopped, "C", phase.Implementing)
	seedClaim(t, records, stopped, stopped, claim.RoleImplementer, supervise.StateIdle)

	settled := settleMovements(t, pool, released, pastRev, stopped)
	admission.now = func() time.Time { return settled }
	tick(t, pool, admission, "release")

	// The premise, read after the release: the issue at work before its review gave its row back
	// for the wait and stands in the line as the waiting kind; the issue past its review holds no
	// row, carries no mark of a release for a wait and stands in no line.
	if at := issue(t, pool, released).ReleasedForWaitAt; at == nil || !at.Equal(settled) || holdsSlot(t, pool, released) {
		t.Fatalf("%s = %+v holding a slot %t, want it released for its wait at %v and holding none", released,
			issue(t, pool, released), holdsSlot(t, pool, released), settled)
	}
	if marked := issue(t, pool, pastRev).ReleasedForWaitAt; marked != nil || holdsSlot(t, pool, pastRev) {
		t.Fatalf("%s carries a release for a wait at %v and holds a slot %t, want neither: past its review it holds no slot to give back",
			pastRev, marked, holdsSlot(t, pool, pastRev))
	}
	facts, slots := passFacts(t, pool)
	kinds := map[string]record.LineKind{}
	inTx(t, pool, func(tx pgx.Tx) {
		issues, err := record.NewStore().LiveIssues(context.Background(), tx, testProject)
		if err != nil {
			t.Fatalf("list the live issues: %v", err)
		}
		line, _ := record.Waiting(issues, slots, facts)
		for _, entry := range line {
			kinds[entry.Issue.Key] = entry.Kind
		}
	})
	if kinds[released] != record.LineWaitingOnIssue {
		t.Fatalf("%s stands in the line as %v, want %v: the started issue that gave its row back waits there for one", released,
			kinds[released], record.LineWaitingOnIssue)
	}
	if kind, stands := kinds[pastRev]; stands {
		t.Fatalf("%s stands in the line as %v, want it in no line: an issue past its review needs no slot", pastRev, kind)
	}

	settled = settleMovements(t, pool, released, pastRev, stopped)
	admission.now = func() time.Time { return settled.Add(7 * time.Hour) }
	tick(t, pool, admission, "measure")

	got := stalledRoots(t, pool)
	slices.Sort(got)
	if want := []string{pastRev, stopped}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the controller was told of %v, want %v: an issue that gave its slot back for a wait waits by design, and the same link on an issue past its review does not",
			got, want)
	}
}

// A started issue that gave its slot back to wait on a Dispatch blocked-by link (blockers.waitsOf,
// releaseSlotsForWaits) never makes its own tree read as stopped, whatever project the issue it
// waits on is in: its wait ends when that issue finishes, and nothing of its own tree is the
// thing that has stopped. That holds where the project's line cannot move as well
// (lineCanMove): the waiter is no slot holder and no entry the promotion can start, so a project
// whose only work waits on another project reads as unable to move, and before this rule its
// tree was told on the second pass. Where the issue it waits on is in this project and has
// stopped, the notice goes on that issue's tree, which is the tree that has stopped, and not on
// the waiter's.
//
// Each project runs over several passes with the clock moved past `stalled_tree_notice_hours`
// each time, and each case says what every pass must have told: nothing for the project whose
// only tree waits, and the stopped tree alone, on every pass, for the two that hold one. The
// first pass is the one that releases the waiter's row.
func TestATreeThatWaitsOnABlockedByLinkIsNeverToldAsStopped(t *testing.T) {
	const (
		waiter     = "LEGION-901"
		otherIssue = "OTHER-9"
		stoppedRet = "LEGION-910"
		sameRoot   = "LEGION-920"
	)
	bound := deploymentBound(t, "")
	// project seeds the waiter, a root in implementing with a Dispatch blocked-by link to
	// blocker, under a cap of four, and answers the pool and the admission.
	project := func(t *testing.T, blocker string) (*pgxpool.Pool, *Admission, *store.Store) {
		t.Helper()
		pool := migratedPool(t)
		admission := quiet(t, 4)
		admission.UseStalledTreeNotice(bound)
		registerController(t, pool)
		records := claimStore(t, pool)
		seedQuietRoot(t, pool, waiter, "A", phase.Implementing)
		seedMirror(t, pool, waiter, "in_progress", blocker)
		seedClaim(t, records, waiter, waiter, claim.RoleImplementer, supervise.StateIdle)
		return pool, admission, records
	}
	// The waiter alone, on a link to an issue of another project: the project's only tree waits,
	// nothing of the project can move, and no pass tells anything.
	t.Run("blocker-in-another-project", func(t *testing.T) {
		pool, admission, _ := project(t, otherIssue)
		seedMirrorIn(t, pool, "OTHER", otherIssue, "in_progress")
		for pass, roots := range waitPasses(t, pool, admission, []string{waiter}, waiter) {
			if roots != nil {
				t.Fatalf("pass %d told the controller of %v, want nothing: a started issue that gave its slot back to wait on %s waits by design, whatever project %s is in",
					pass+1, roots, otherIssue, otherIssue)
			}
		}
	})

	// The control: the same project with a second tree that has truly stopped, a root in retro,
	// which holds no slot. The project's line still cannot move, so that tree is told on the
	// first pass past the bound, and the waiter's tree is told on none.
	t.Run("stopped-tree-beside-it", func(t *testing.T) {
		pool, admission, records := project(t, otherIssue)
		seedQuietRoot(t, pool, stoppedRet, "B", phase.Retro)
		seedClaim(t, records, stoppedRet, stoppedRet, claim.RoleImplementer, supervise.StateIdle)
		for pass, roots := range waitPasses(t, pool, admission, []string{waiter}, waiter, stoppedRet) {
			if want := []string{stoppedRet}; !reflect.DeepEqual(roots, want) {
				t.Fatalf("after pass %d the controller was told of %v, want %v: the tree that has stopped is told, and the tree that waits on another issue is not",
					pass+1, roots, want)
			}
		}
	})

	// The issue it waits on is the root of another tree of this project, standing in its merge
	// with its merger idle: that tree has stopped, so it is the one told, and the waiter's tree,
	// which stands still only because that tree does, is not.
	t.Run("stopped-blocker-in-this-project", func(t *testing.T) {
		pool, admission, records := project(t, sameRoot)
		seedQuietRoot(t, pool, sameRoot, "B", phase.Merging)
		seedMirror(t, pool, sameRoot, "in_progress")
		seedClaim(t, records, sameRoot, sameRoot, claim.RoleMerger, supervise.StateIdle)
		for pass, roots := range waitPasses(t, pool, admission, []string{waiter}, waiter, sameRoot) {
			if want := []string{sameRoot}; !reflect.DeepEqual(roots, want) {
				t.Fatalf("after pass %d the controller was told of %v, want %v: the notice goes on the tree of the issue that stopped and not on the tree that waits on it",
					pass+1, roots, want)
			}
		}
	})
}

// waitPasses runs the release pass at the settled movement of keys and then three passes, each
// seven hours past the one before, and answers the roots told on each pass after the first. The
// notices are left in the outbox, so a tree told once is in every later answer. Each of waiters
// must have given its row back for its wait on the release pass: that is the premise of every
// case that reads it.
func waitPasses(t *testing.T, pool *pgxpool.Pool, admission *Admission, waiters []string, keys ...string) [][]string {
	t.Helper()
	settled := settleMovements(t, pool, keys...)
	admission.now = func() time.Time { return settled }
	tick(t, pool, admission, "release")
	for _, waiter := range waiters {
		if at := issue(t, pool, waiter).ReleasedForWaitAt; at == nil || holdsSlot(t, pool, waiter) {
			t.Fatalf("%s = %+v holding a slot %t, want it released for its wait: the premise of this case", waiter,
				issue(t, pool, waiter), holdsSlot(t, pool, waiter))
		}
	}
	var told [][]string
	for pass := 1; pass <= 3; pass++ {
		at := settled.Add(time.Duration(pass) * 7 * time.Hour)
		admission.now = func() time.Time { return at }
		tick(t, pool, admission, fmt.Sprintf("measure-%d", pass))
		roots := stalledRoots(t, pool)
		slices.Sort(roots)
		told = append(told, roots)
	}
	return told
}

// A root whose own first phase waits for an open child (the children-first wait) gave its slot
// back for that wait, and whether the wait is on work outside its tree depends on the child
// (lineHolds.waitsOnWorkOutsideItsTree). A child the daemon has released is released work, which
// nothing of the waiting tree moves: the tree is told to nobody however the line stands. A child
// the architect filed and never released is work of the tree that its architect can release, so
// the wait leaves the tree to the stall measure exactly as any other wait of the daemon's does:
// told once the project's line can no longer move.
//
// What fails without the rule: at 7b31b601 every wait of a started issue on another issue was
// silenced whatever the line did, so the unreleased child's tree was told to nobody and
// unreleased-child fails there. Before that commit no such wait was silenced where the line
// could not move, and released-child fails there, as it does under a rule that silences no
// children-first wait at all.
//
// Each project is the root alone at admitted under a design gate that has opened, with its
// architect idle, under a cap of four, measured over three passes past the bound.
func TestAWaitOnAChildIsAWaitByDesignOnlyOnceTheChildIsReleased(t *testing.T) {
	const (
		root  = "LEGION-950"
		child = "LEGION-951"
		other = "OTHER-9"
	)
	bound := deploymentBound(t, "")
	project := func(t *testing.T) (*pgxpool.Pool, *Admission, *store.Store) {
		t.Helper()
		pool := migratedPool(t)
		admission := quiet(t, 4)
		admission.UseStalledTreeNotice(bound)
		registerController(t, pool)
		records := claimStore(t, pool)
		seedQuietRoot(t, pool, root, "A", phase.Admitted)
		seedGate(t, pool, record.DesignGate{Issue: root, ArtifactID: "artifact-950", LatestVersion: 1,
			ApprovedVersion: new(1), EverOpened: true})
		seedMirror(t, pool, root, "in_progress")
		seedClaim(t, records, root, root, claim.RoleArchitect, supervise.StateIdle)
		return pool, admission, records
	}

	// A child the architect filed and has not released, so the daemon holds no record of it.
	// Nothing else of the project can move, and the tree is told on every pass.
	t.Run("unreleased-child", func(t *testing.T) {
		pool, admission, _ := project(t)
		seedMirrorChild(t, pool, child, "backlog", root)
		for pass, roots := range waitPasses(t, pool, admission, []string{root}, root) {
			if want := []string{root}; !reflect.DeepEqual(roots, want) {
				t.Fatalf("after pass %d the controller was told of %v, want %v: a wait on a child nobody released is the tree's own work, which its architect can do",
					pass+1, roots, want)
			}
		}
	})

	// The released child: recorded in the tree, in implementing, and itself waiting on an issue
	// of another project, so it gave its own slot back. Nothing of the project can move, and
	// neither wait is the tree's own work: the tree is told to nobody.
	t.Run("released-child", func(t *testing.T) {
		pool, admission, records := project(t)
		seedQuietChild(t, pool, root, child, "A1", phase.Implementing, "in_progress")
		takeSlot(t, pool, child)
		seedMirrorChild(t, pool, child, "in_progress", root, other)
		seedMirrorIn(t, pool, "OTHER", other, "in_progress")
		seedClaim(t, records, root, child, claim.RoleImplementer, supervise.StateIdle)
		for pass, roots := range waitPasses(t, pool, admission, []string{root, child}, root, child) {
			if roots != nil {
				t.Fatalf("pass %d told the controller of %v, want nothing: %s waits on a child the daemon released, and that child waits on %s",
					pass+1, roots, root, other)
			}
		}
	})
}

// A queued root past its review holds no row and keeps none (slotFor), so its promotion opens
// its tree whatever the cap says. The liveness clause's free-slot term reads that same decision
// (capLets): a project whose only slot holder waits by design under a full cap, and whose only
// line entry is such a root, can still move, and its waiting tree is told to nobody on the pass
// that opens the carried root's tree.
//
// What fails without the rule: at 7b31b601 the free-slot term returned false at a full cap
// before it read the line, so the project read as unable to move on the very pass that opens
// the carried root's tree, and the controller was told that LEGION-930's tree had stopped.
func TestAFullCapWithAHandedOverRootPastItsReviewStillMoves(t *testing.T) {
	const (
		awaiting = "LEGION-930"
		carried  = "LEGION-940"
	)
	pool := migratedPool(t)
	admission := quiet(t, 1)
	admission.UseStalledTreeNotice(deploymentBound(t, ""))
	registerController(t, pool)
	records := claimStore(t, pool)
	seedQuietRoot(t, pool, awaiting, "A", phase.AwaitingPerson)
	seedClaim(t, records, awaiting, awaiting, claim.RoleArchitect, supervise.StateIdle)
	carriedRoot(t, pool, carried, phase.Accepting, "B")

	if !holdsSlot(t, pool, awaiting) {
		t.Fatalf("%s holds no slot: the premise is that it fills the cap of one", awaiting)
	}
	assertWaiting(t, pool, []string{carried})

	settled := settleMovements(t, pool, awaiting)
	admission.now = func() time.Time { return settled.Add(7 * time.Hour) }
	tick(t, pool, admission, "measure")

	if !treeOpen(t, pool, carried) {
		t.Fatalf("the tree of %s is closed after the pass: the premise is that its promotion opens it under the full cap", carried)
	}
	if got := stalledRoots(t, pool); got != nil {
		t.Fatalf("the controller was told of %v, want nothing: the promotion of this pass starts %s under the full cap, so the line moves",
			got, carried)
	}
}
