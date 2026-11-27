package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The tests of new work on a closed issue whose work merged (D17 of the done-by-checks plan, Task
// 13) through the daemon's workflow: a request applied through the engine and admission as the
// daemon applies an event, the merge read it queues run by the daemon's outbox runner against a
// Dispatch and a GitHub of the test's own, the fact that read carries back, the controller's tick,
// and the outbox's close of a tree. Each drives an mnwRig (merged_new_work_rig_test.go).

// A merged child of a tree that runs, whose record holds no row of its merge (NONE): a whole-tree
// clear before this release deleted its pull request row and kept no clock. A real Dispatch holds
// its merged pull request #41, attributed to it, and a check of it that runs after the merge, so
// Dispatch's results route compares that check with the merge. A person's todo waits for the merge
// read and re-enters nothing. The read finds its one candidate through the results route: the
// merge commit's pull request, which GitHub's stand-in lists and then answers merged from the
// child's branch. That merge starts one next change of the child in its tree, carrying the
// person's request, with the merge's snapshot saved and its gate kept.
func TestAClearedMergedChildRecoversItsMergeFromDispatch(t *testing.T) {
	t.Parallel()
	d := newMNWDispatchWithMerge(t)
	client := newMNWCountedDispatch(d.server.base, d.server.token)
	r := newMNWRig(t, client, d.github.url)
	r.liveTree(d.root.LastSeq, d.patch(mnwChild, map[string]any{"status": "done"}))
	gates := r.gates(mnwChild)

	r.apply("dispatch", mnwMove(mnwChild, mnwRoot, "todo", d.patch(mnwChild, map[string]any{"status": "todo"}), "person"))
	r.waits(mnwChild, 1, record.RequesterPerson)

	r.readMerge(mnwChild, 20)

	got := r.started(mnwChild, 2, record.RequesterPerson)
	if got.Tree != mnwRoot || got.Parent == nil || *got.Parent != mnwRoot {
		t.Fatalf("%s's next change runs in tree %s under %v, want it a child of %s still", mnwChild, got.Tree, got.Parent, mnwRoot)
	}
	mnwMerged(t, got, 41, stepRecordsHead, doneByChecksMergeCommit)
	if after := r.gates(mnwChild); !reflect.DeepEqual(after, gates) {
		t.Fatalf("%s's gate after its next change started = %v, want the approved design %v", mnwChild, after, gates)
	}
	// The todo queued no child-status (r.waits). The one that stands now is the start of the change's
	// first phase, which every child that takes a slot is told with (ChildStartedNotice); a re-entry
	// would add its own beside it.
	if told := r.noticed(mnwChild, "child-status"); told != 1 {
		t.Fatalf("child-status notices of %s = %d, want one, the start of its change's first phase: a re-entry adds another", mnwChild, told)
	}
	if client.counted("results") != 1 {
		t.Fatalf("the merge read read %s's results %d times, want once: the results route names its merge", mnwChild, client.counted("results"))
	}
	if told := r.told(mnwMergeUnread); told != 0 {
		t.Fatalf("merge-unread notices = %d, want none: the read found the merge", told)
	}
}

// The same child with no trace of its merge anywhere: no row of the record, no link, and no result
// of Dispatch's names it (NONE). The controller's todo takes no re-entry and starts nothing: the
// child waits at done for its merge read, whose search reads the child's links and results once
// each, finds no candidate, and tells the controller at once (one merge-unread notice). Then the
// controller's ticks run across two UTC days. The first tick of each day adds one merge-unread
// notice, and no later tick of the day adds another. No tick sends a GET to Dispatch or GitHub,
// queues no stage of the read and asks no refresh: the read stays unread as its search left it.
func TestAClearedMergedChildWithNoTraceWaitsUnread(t *testing.T) {
	t.Parallel()
	board := &mergeReadBoard{outboxDispatch: &outboxDispatch{}, reads: map[string]int{}}
	github := newMergeReadGitHub(t)
	r := newMNWRig(t, board, github.url)
	r.liveTree(mnwSeq, mnwSeq)
	gates := r.gates(mnwChild)
	gets := func() int { return board.counted("issue") + board.counted("results") + github.count("") }

	r.apply("dispatch", mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+1, "controller"))
	r.waits(mnwChild, 1, claim.RoleController)

	r.readMerge(mnwChild, 10)
	searched := r.waits(mnwChild, 1, claim.RoleController)
	if searched.Progress.State != record.MergeReadStateUnread {
		t.Fatalf("the merge read of %s ended %+v, want its search completed unread", mnwChild, searched.Progress)
	}
	if board.counted("issue") != 1 || board.counted("results") != 1 || github.count("") != 0 {
		t.Fatalf("the search read Dispatch's issue %d and results %d times and sent GitHub %d GETs, want one of each read and no GET",
			board.counted("issue"), board.counted("results"), github.count(""))
	}
	if told := r.told(mnwMergeUnread); told != 1 {
		t.Fatalf("merge-unread notices once the search ended = %d, want one, at once", told)
	}

	sent := gets()
	start := r.clock()
	for day := 1; day <= 2; day++ {
		midnight := start.Truncate(24 * time.Hour).Add(time.Duration(day) * 24 * time.Hour)
		for _, after := range []time.Duration{30 * time.Minute, 6 * time.Hour, 12 * time.Hour, 18 * time.Hour, 23*time.Hour + 30*time.Minute} {
			r.at(midnight.Add(after))
			r.tick()
			r.turn()
		}
		if told := r.told(mnwMergeUnread); told != 1+day {
			t.Fatalf("merge-unread notices after the ticks of day %d (%s) = %d, want %d: one at once and one for each day's first tick",
				day, midnight.Format(time.DateOnly), told, 1+day)
		}
	}
	if gets() != sent {
		t.Fatalf("the ticks of two days sent %d GETs to Dispatch and GitHub, want none", gets()-sent)
	}
	stands := r.waits(mnwChild, 1, claim.RoleController)
	if stands.Progress.State != searched.Progress.State || stands.Progress.Attempts != searched.Progress.Attempts || stands.Progress.Refresh {
		t.Fatalf("the merge read after two days of ticks = %+v, want it as its search left it, %+v, with no refresh asked", stands.Progress,
			searched.Progress)
	}
	if after := r.gates(mnwChild); !reflect.DeepEqual(after, gates) {
		t.Fatalf("%s's gate while it waits = %v, want %v", mnwChild, after, gates)
	}
}

// The same child waits unread in a real Dispatch: a person's todo, and a search that read its
// links and results and found nothing. Then the person links its merged pull request #44 on the
// issue, and Dispatch's event of the edit reaches the daemon. The read's next try reads the links
// from Dispatch once more, replays no results read, and reads the new candidate from GitHub's
// stand-in, which answers it merged from the child's branch. Its merge starts the child's next
// change, carrying the person's request, with the merge's snapshot saved and its gate kept.
func TestALinkEndsTheWaitOfAClearedMergedChild(t *testing.T) {
	t.Parallel()
	d := newMNWDispatch(t)
	client := newMNWCountedDispatch(d.server.base, d.server.token)
	github := newMergeReadGitHub(t)
	r := newMNWRig(t, client, github.url)
	r.liveTree(d.root.LastSeq, d.patch(mnwChild, map[string]any{"status": "done"}))
	gates := r.gates(mnwChild)
	r.apply("dispatch", mnwMove(mnwChild, mnwRoot, "todo", d.patch(mnwChild, map[string]any{"status": "todo"}), "person"))
	r.waits(mnwChild, 1, record.RequesterPerson)
	r.readMerge(mnwChild, 10)
	if read := r.waits(mnwChild, 1, record.RequesterPerson); read.Progress.State != record.MergeReadStateUnread {
		t.Fatalf("the merge read of %s ended %+v, want its search completed unread", mnwChild, read.Progress)
	}
	issueReads, resultsReads := client.counted("issue"), client.counted("results")

	github.mu.Lock()
	github.pulls["acme/widgets#44"] = mergeReadPull{branch: claim.IssueBranch(mnwChild), head: "h44", merge: "m44", merged: true, at: r.clock().Add(-time.Hour)}
	github.mu.Unlock()
	linked := d.patch(mnwChild, map[string]any{"external_links": []map[string]string{{"url": "https://github.com/acme/widgets/pull/44"}}})
	r.apply("dispatch", mnwMove(mnwChild, mnwRoot, "todo", linked, "person"))
	r.readMerge(mnwChild, 10)

	got := r.started(mnwChild, 2, record.RequesterPerson)
	mnwMerged(t, got, 44, "h44", "m44")
	if client.counted("issue") != issueReads+1 || client.counted("results") != resultsReads || github.count("/pulls/44") != 1 {
		t.Fatalf("after the link the read sent Dispatch %d more issue reads and %d more results reads and GitHub %d reads of #44; "+
			"want the links read once more, no results read again and #44 read once",
			client.counted("issue")-issueReads, client.counted("results")-resultsReads, github.count("/pulls/44"))
	}
	if after := r.gates(mnwChild); !reflect.DeepEqual(after, gates) {
		t.Fatalf("%s's gate after its next change started = %v, want the approved design %v", mnwChild, after, gates)
	}
}

// mnwHolder is a closed root of the rename case, whose pull request row holds the new name and the
// branch the renamed root's row named, so the rename deletes that row.
const mnwHolder = "ACME-9"

// A closed root whose record holds only a retained pull request row, #7 on the repository's old
// name, which the record names closed. GitHub's stand-in answers #7 merged. The rename pass writes
// the new name over the old one and deletes that row, because another closed root's row holds the
// new name and the same branch: the deletion keeps the row's clock under the new name, and the
// root's class stays PARTIAL. No snapshot, production check, link or Dispatch result names it. A
// person's todo queues the merge read, whose one candidate is the kept clock. The GitHub read finds
// the merge, which starts one next change of the root, carrying the person's request, with its
// gate kept and the merge's snapshot saved under the new name.
func TestARenamedAwayRowStillFindsItsMerge(t *testing.T) {
	t.Parallel()
	const from = "acme/gizmos"
	board := &mergeReadBoard{outboxDispatch: &outboxDispatch{}, reads: map[string]int{}}
	github := newMergeReadGitHub(t)
	r := newMNWRig(t, board, github.url)
	root := mnwIssue(mnwRoot, mnwRoot, nil, phase.Done, "done", mnwSeq)
	root.Repo, root.GitHubRepo = from, from
	r.transact(func(ctx context.Context, tx pgx.Tx) error {
		for _, put := range []error{
			r.records.PutIssue(ctx, tx, root),
			r.records.PutIssue(ctx, tx, mnwIssue(mnwHolder, mnwHolder, nil, phase.Done, "done", mnwSeq)),
			r.records.PutPullRequest(ctx, tx, record.PullRequest{Issue: mnwRoot, Repo: from, Number: 7, Branch: "release/shared", Base: "main",
				HeadSHA: "h7", State: record.PullRequestClosed, Failing: []string{}}),
			r.records.PutPullRequest(ctx, tx, record.PullRequest{Issue: mnwHolder, Repo: liveRepo, Number: 9, Branch: "release/shared", Base: "main",
				HeadSHA: "h9", State: record.PullRequestMerged, Failing: []string{}}),
		} {
			if put != nil {
				return put
			}
		}
		return nil
	})
	r.gate(mnwRoot)
	gates := r.gates(mnwRoot)
	github.pulls["acme/widgets#7"] = mergeReadPull{branch: "release/shared", head: "h7", merge: "m7", merged: true, at: r.clock().Add(-48 * time.Hour)}

	renamePass{pool: r.pool, project: mnwProject, renames: ghrepo.MustRenames(ghrepo.MustRename(from, liveRepo)),
		transferred: func(context.Context, ghrepo.Rename) (ghrepo.Repository, string) {
			return ghrepo.MustParse(liveRepo), ""
		},
		keep: func(string, []string) string { return "" }, written: func() {}, log: quietLogger()}.run(r.ctx, nil)

	var rows, clocks int
	if err := r.pool.QueryRow(r.ctx, `select (select count(*) from pull_requests where issue = $1),
		(select count(*) from cleared_pull_requests where issue = $1 and repo = $2 and number = 7)`, mnwRoot, liveRepo).Scan(&rows, &clocks); err != nil {
		t.Fatalf("read %s's rows after the rename: %v", mnwRoot, err)
	}
	if renamed := r.issue(mnwRoot); renamed.Repo != liveRepo || renamed.Merge.Merged() || rows != 0 || clocks != 1 {
		t.Fatalf("after the rename %s is on %s with snapshot %+v, %d pull request rows and %d clocks of %s#7; want it on %s, no snapshot, no row and the one clock",
			mnwRoot, renamed.Repo, renamed.Merge, rows, clocks, liveRepo, liveRepo)
	}
	if class := r.class(mnwRoot); class != record.MergeHistoryPartial {
		t.Fatalf("the class of %s after the rename = %s, want PARTIAL: its kept clock is no proof of a merge", mnwRoot, class)
	}

	r.apply("dispatch", mnwMove(mnwRoot, "", "todo", mnwSeq+1, "person"))
	r.waits(mnwRoot, 1, record.RequesterPerson)
	r.readMerge(mnwRoot, 10)

	got := r.started(mnwRoot, 2, record.RequesterPerson)
	if got.Tree != mnwRoot || got.Parent != nil {
		t.Fatalf("%s's next change runs in tree %s under %v, want it the root of its own tree", mnwRoot, got.Tree, got.Parent)
	}
	mnwMerged(t, got, 7, "h7", "m7")
	if after := r.gates(mnwRoot); !reflect.DeepEqual(after, gates) {
		t.Fatalf("%s's gate after its next change started = %v, want the approved design %v", mnwRoot, after, gates)
	}
	if github.count("/pulls/7") != 1 || github.count("") != 1 || board.counted("issue") != 1 || board.counted("results") != 1 {
		t.Fatalf("the read sent GitHub %d GETs (%d of #7) and Dispatch %d issue reads and %d results reads; want #7 read once and one read of each",
			github.count(""), github.count("/pulls/7"), board.counted("issue"), board.counted("results"))
	}
	if told := r.told(mnwMergeUnread); told != 0 {
		t.Fatalf("merge-unread notices = %d, want none: the kept clock found the merge", told)
	}
}

// A merged root closed and lingered, and its linger's close reserved the tree's cleanup and
// retired the architect, but the runtime's release of the tree failed, so the cleanup stands
// reserved and unconfirmed. A person's todo starts the root's next change, which waits at admitted
// as a queued root: no slot, no launch, and the reservation stands, since nothing cancels it. The
// release then succeeds, the close confirms the cleanup, and still nothing launches in that epoch.
// The next admission pass promotes the root: the tree's next epoch opens, and the architect's start
// launches it bound to that epoch.
func TestAReservedCleanupFinishesBeforeReopenLaunches(t *testing.T) {
	t.Parallel()
	github := newNextBranchGitHub(t, map[string]string{"main": nextBaseHead, claim.IssueBranch(mnwRoot): nextMergedHead}, nil)
	r := newMNWRig(t, &outboxDispatch{issue: dispatch.Issue{Key: mnwRoot, Status: "todo", HandedOver: true, Labels: []string{dispatch.LegionLabel}}},
		github.url)
	trees, err := store.Open(r.ctx, r.pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(trees.Close)
	sup, rt := newOutboxSupervisor(t, mnwToken, t.TempDir())
	sup.deps.Store = trees
	r.runner.supervisor, r.runner.trees = sup, trees
	r.runner.provision = func(context.Context, workspace.Request) (workspace.Workspace, error) {
		return workspace.Workspace{Dir: t.TempDir(), Bookmark: claim.IssueBranch(mnwRoot)}, nil
	}
	r.runner.remove = func(context.Context, workspace.Workspace, sync.Locker) error { return nil }
	architect, err := claim.NewToken(mnwToken, mnwRoot, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	launches := func() int {
		spawned := 0
		for _, call := range rt.CallsOf("Spawn") {
			if call.Spec.Claim == architect {
				spawned++
			}
		}
		return spawned + len(resumesOf(rt, architect))
	}
	lifecycle := func() treelifecycle.Lifecycle {
		var read treelifecycle.Lifecycle
		r.read(func(ctx context.Context, tx pgx.Tx) error {
			var found bool
			var err error
			if read, found, err = treelifecycle.Read(ctx, tx, mnwToken, mnwRoot); err == nil && !found {
				t.Fatalf("the tree of %s has no lifecycle", mnwRoot)
			}
			return err
		})
		return read
	}
	reserved := func(when string) {
		t.Helper()
		if l := lifecycle(); l.Epoch != 1 || !l.CleanupStarted || !l.CleanupConfirmedAt.IsZero() {
			t.Fatalf("the tree's lifecycle %s = %+v, want epoch 1 with its cleanup reserved and unconfirmed", when, l)
		}
	}

	// The tree runs: its root in implementing, its lifecycle open at epoch 1, its architect launched
	// on a session it registered.
	root := mnwIssue(mnwRoot, mnwRoot, nil, phase.Implementing, "in_progress", mnwSeq)
	r.transact(func(ctx context.Context, tx pgx.Tx) error { return r.records.PutIssue(ctx, tx, root) })
	if _, err := trees.OpenTreeLifecycle(r.ctx, mnwToken, mnwRoot, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the tree of %s: %v", mnwRoot, err)
	}
	if err := r.runner.execute(r.ctx, mustOutboxRow(t, mnwRoot, record.SuperviseRequest{Op: "start", Tree: mnwRoot, Role: claim.RoleArchitect,
		Generation: 1}, r.clock())); err != nil {
		t.Fatalf("start the architect: %v", err)
	}
	machine, found := sup.Machine(architect)
	if !found {
		t.Fatalf("no claim of %s after its start", architect)
	}
	if err := machine.Handle(r.ctx, supervise.RequestRegister{Claim: architect, Generation: machine.Claim().Generation, Session: "ses-architect",
		SessionFile: filepath.Join(t.TempDir(), "architect.jsonl")}); err != nil {
		t.Fatalf("register the architect: %v", err)
	}

	// The root closes at its merge and lingers, and its linger expires while the runtime's release of
	// the tree fails: the close reserves the cleanup and retires the architect, and the cleanup waits.
	root.Phase, root.Status, root.DispatchStatus, root.LingerUntil = phase.Done, "done", "done", new(r.clock().Add(-time.Minute))
	r.transact(func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		_, err := r.records.PutMergeSnapshot(ctx, tx, mnwRoot, record.Merge{Number: 11, Head: nextMergedHead, SHA: "merge-11", Base: "main",
			Repo: liveRepo, MergedAt: r.clock().Add(-2 * time.Hour)})
		return err
	})
	r.gate(mnwRoot)
	gates := r.gates(mnwRoot)
	rt.FailCleanupTree(errors.New("the runtime's release of the tree failed for now"))
	r.apply("outbox", intake.LingerExpired{Issue: mnwRoot, Generation: 1})
	r.runAll()
	if state := machine.Claim().State; state != supervise.StateRetired {
		t.Fatalf("the architect after its tree's close = %s, want retired", state)
	}
	reserved("after the close whose release failed")
	before := launches()

	// A person's todo: the root's next change waits at admitted as a queued root.
	r.apply("dispatch", mnwMove(mnwRoot, "", "todo", mnwSeq+1, "person"))
	got := r.started(mnwRoot, 2, record.RequesterPerson)
	if got.Phase != phase.Admitted || r.slotted(mnwRoot) {
		t.Fatalf("%s's next change stands in %s holding a slot %t, want it waiting at admitted with none: its tree's cleanup is reserved",
			mnwRoot, got.Phase, r.slotted(mnwRoot))
	}
	if after := r.gates(mnwRoot); !reflect.DeepEqual(after, gates) {
		t.Fatalf("%s's gate after its next change started = %v, want the approved design %v", mnwRoot, after, gates)
	}
	reserved("after the todo")
	r.runAll()
	reserved("after the outbox ran with the release still failing")
	if launches() != before {
		t.Fatalf("the architect launched %d times while its tree's cleanup stood reserved, want none", launches()-before)
	}

	// The release succeeds: the reserved cleanup confirms, and nothing launches in its epoch.
	rt.FailCleanupTree(nil)
	for range 10 {
		if !lifecycle().CleanupConfirmedAt.IsZero() {
			break
		}
		r.at(r.clock().Add(2 * time.Minute))
		r.runAll()
	}
	if l := lifecycle(); l.Epoch != 1 || l.CleanupConfirmedAt.IsZero() {
		t.Fatalf("the tree's lifecycle once its release succeeds = %+v, want epoch 1 with its cleanup confirmed", l)
	}
	if launches() != before {
		t.Fatalf("the architect launched %d times in the epoch whose cleanup confirmed, want none", launches()-before)
	}

	// The next admission pass promotes the root: its tree's next epoch opens, and the architect
	// launches bound to it.
	r.tick()
	for range 5 {
		if launches() > before {
			break
		}
		r.runAll()
		r.at(r.clock().Add(time.Minute))
	}
	if l := lifecycle(); l.Epoch != 2 || l.CleanupStarted {
		t.Fatalf("the tree's lifecycle after the root's promotion = %+v, want its epoch 2 open", l)
	}
	if launches() == before {
		t.Fatalf("the architect did not launch after its tree's epoch 2 opened")
	}
	var epoch int64
	if err := r.pool.QueryRow(r.ctx, `select tree_epoch from claims where token = $1`, string(architect)).Scan(&epoch); err != nil {
		t.Fatalf("read the architect's claim: %v", err)
	}
	if epoch != 2 {
		t.Fatalf("the launched architect's claim binds tree epoch %d, want 2", epoch)
	}
	if after := r.issue(mnwRoot); after.Generation != 2 || after.NextChange != 2 || after.NextChanges != 1 || !r.slotted(mnwRoot) {
		t.Fatalf("%s after its promotion = generation %d, change %d of %d, slotted %t; want its one next change 2 holding a slot",
			mnwRoot, after.Generation, after.NextChange, after.NextChanges, r.slotted(mnwRoot))
	}
}
