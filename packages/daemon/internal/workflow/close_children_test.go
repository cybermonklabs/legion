package workflow

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// architectOf is the architect claim token of key in the LEGION project.
func architectOf(t *testing.T, key string) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, key, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// landClose applies Dispatch's event of key closed at done, written by a person, which lands a
// close the daemon queued as that close's echo does (closeLanded), and mirrors it, keeping the
// parent the copy holds for key; the record then holds the status, as admission's observation of
// the event writes it after the engine's handler (admit's recordObservation), which these tests'
// admission stub does not. The boot's open of each run no start counted comes before it, as before
// every fact applyFacts applies.
func landClose(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	landCloseBy(t, pool, engine, key, false)
}

// landCloseBy is landClose with the event's author: the daemon (daemon true), whose own close
// Dispatch echoes as an event of the daemon's session (intake.DispatchIssue.DaemonActor), or a
// person.
func landCloseBy(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, daemon bool) {
	t.Helper()
	issue := storedIssue(t, pool, key)
	var parent string
	if err := pool.QueryRow(t.Context(), `select coalesce(parent, '') from dispatch_issues where key = $1`, key).Scan(&parent); err != nil {
		t.Fatalf("read the copy's parent of %s: %v", key, err)
	}
	bootBefore(t, pool, engine)
	applyMirrored(t, pool, engine, key+"-done", intake.DispatchIssue{Key: key, ResolvedRepo: issue.Repo, Seq: issue.LastDispatchSeq + 1,
		Type: "issue.closed", Status: "done", Title: issue.Title, Parent: parent, Rank: issue.Rank, DaemonActor: daemon})
	if _, err := pool.Exec(t.Context(), "update issues set status = 'done', dispatch_status = 'done' where key = $1", key); err != nil {
		t.Fatalf("record the observation of %s at done: %v", key, err)
	}
	factLeft(t, pool)
}

// closePass is the decision pass's reading of the closes the project owes (admit's dueCloses):
// each issue that carries a close the daemon queued or awaits its merge is handed to
// Engine.WriteDueClose with the pass's chains, in one transaction (store.Transact).
func closePass(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		ctx = engine.OpenFact(ctx)
		records := record.NewStore()
		issues, err := records.Issues(ctx, tx)
		if err != nil {
			return fmt.Errorf("list issues: %w", err)
		}
		chains, err := record.ReadChains(ctx, tx, records, "LEGION", nil)
		if err != nil {
			return fmt.Errorf("read the chains: %w", err)
		}
		if _, err := engine.WriteDueCloses(ctx, tx, issues, chains); err != nil {
			return fmt.Errorf("WriteDueCloses: %w", err)
		}
		if err := engine.SettleFact(ctx, tx); err != nil {
			return fmt.Errorf("settle: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// supervisedOps is the op of every supervise row queued for issue, in order.
func supervisedOps(t *testing.T, pool *pgxpool.Pool, issue string) []string {
	t.Helper()
	return outboxStrings(t, pool, `select payload->>'op' from outbox where kind = 'supervise' and issue = $1 order by id`, issue)
}

// close_root writes the close only when Dispatch's copy shows each child of the root done (R24 of
// dispatch://INFRA-3320): a child in icebox, the backlog or triage refuses the call with
// CHILDREN_NOT_DONE naming each child with its status, after R8's codes, and nothing is queued.
// Once each child is done the close is one status write, done with the reason, and the root keeps
// its phase, its claims and its slot until Dispatch shows it done: then it moves to phase done and
// leaves.
func TestCloseRootWaitsForEveryChildDone(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1",
		Project: "LEGION", Title: "Root", Phase: phase.Admitted, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	mirrorChild(t, pool, "LEGION-2", "LEGION-1", "icebox", 1)
	mirrorChild(t, pool, "LEGION-3", "LEGION-1", "done", 1)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	closeRoot := intake.CloseRoot{Issue: "LEGION-1", Reason: "moot", By: architectOf(t, "LEGION-1")}

	message := refusedWith(t, apply("close-held", closeRoot), "CHILDREN_NOT_DONE", "LEGION-2 (icebox)")
	if strings.Contains(message, "LEGION-3") {
		t.Fatalf("the refusal %q names LEGION-3, which is done", message)
	}
	assertOutboxCount(t, pool, "dispatch_status", 0)

	mirrorChild(t, pool, "LEGION-2", "LEGION-1", "done", 2)
	if result := apply("close", closeRoot); result.Refusal != nil {
		t.Fatalf("close refused once each child is done: %+v", result.Refusal)
	}
	apply("close-again", closeRoot)
	want := []record.StatusWrite{record.CloseStatusWrite("done", "in_progress", "Closed by its architect before its first phase: moot", 2)}
	if got := statusWrites(t, pool, "LEGION-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("status writes = %#v, want the one close, %#v", got, want)
	}
	root := storedIssue(t, pool, "LEGION-1")
	pending := record.PendingClose{Generation: 2, Entry: 1, Kind: "close_root", Reason: "Closed by its architect before its first phase: moot"}
	if root.Phase != phase.Admitted || root.Status != "in_progress" || root.PendingClose == nil || *root.PendingClose != pending {
		t.Fatalf("the root before Dispatch shows done = phase %s, status %s, pending %+v; want it admitted and in_progress with %+v",
			root.Phase, root.Status, root.PendingClose, pending)
	}
	assertOutboxCount(t, pool, "issue_suspend", 0)

	landClose(t, pool, engine, "LEGION-1")
	root = storedIssue(t, pool, "LEGION-1")
	if root.Phase != phase.Done || root.PendingClose != nil {
		t.Fatalf("the root once Dispatch shows done = phase %s, pending %+v; want done with no close pending", root.Phase, root.PendingClose)
	}
	assertOutboxCount(t, pool, "issue_suspend", 1)
}

// The echo of the daemon's own close lands it while the close's own row is still in the outbox:
// Dispatch publishes the event of the write before the outbox has finished the row that made it,
// and the consumer can apply that event first. A close is counted on no record before Dispatch
// shows it (writeClose), so its echo is no earlier step the record has passed, as the echo of a
// write the record counted is while a later write is queued (dispatchIssue): the root moves to
// phase done and leaves, as it does on a person's done, since the daemon acts on a close once
// Dispatch shows the issue done (R20 and R24 of dispatch://INFRA-3320).
func TestTheEchoOfTheDaemonsCloseLandsItWhileItsRowIsStillQueued(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1",
		Project: "LEGION", Title: "Root", Phase: phase.Admitted, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	if result := apply("close", intake.CloseRoot{Issue: "LEGION-1", Reason: "moot", By: architectOf(t, "LEGION-1")}); result.Refusal != nil {
		t.Fatalf("close_root refused: %+v", result.Refusal)
	}
	if got := statusWritesOf(t, pool, "LEGION-1"); !slices.Equal(got, []string{"done"}) {
		t.Fatalf("status writes of LEGION-1 = %v, want the close's row still queued as its echo arrives", got)
	}

	landCloseBy(t, pool, engine, "LEGION-1", true)
	root := storedIssue(t, pool, "LEGION-1")
	if root.Phase != phase.Done || root.PendingClose != nil {
		t.Fatalf("the root once Dispatch echoes the daemon's close = phase %s, pending %+v; want done with no close pending", root.Phase, root.PendingClose)
	}
	assertOutboxCount(t, pool, "issue_suspend", 1)
}

// A merged issue's close waits in merging for each Dispatch child of it that is not done, one in
// the backlog or in icebox included (R24 of dispatch://INFRA-3320): the merge queues no close, and
// its pr-merged notice names each child with its status. Its close is queued on the first pass
// whose copy shows each child done, a child the daemon records nothing of included, and the issue
// leaves merging only when Dispatch shows it done, opening its production check then.
func TestAMergedIssueWaitsInMergingForEachChildNotDone(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1",
		Project: "LEGION", Title: "Root", Phase: phase.Merging, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	seedPR(t, pool, record.PullRequest{Repo: "acme/widgets", Number: 42, Issue: "LEGION-1", HeadSHA: "c0de", State: record.PullRequestOpen})
	mirrorChild(t, pool, "LEGION-2", "LEGION-1", "backlog", 1)
	mirrorChild(t, pool, "LEGION-3", "LEGION-1", "icebox", 1)
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)

	apply("merged", intake.PullRequestMerged{Repo: "acme/widgets", Number: 42, HeadSHA: "c0de", MergeSHA: "m3r9e"})
	if got := storedIssue(t, pool, "LEGION-1"); got.Phase != phase.Merging || got.PendingClose != nil {
		t.Fatalf("the merged issue = phase %s, pending %+v; want it in merging with no close queued", got.Phase, got.PendingClose)
	}
	assertOutboxCount(t, pool, "dispatch_status", 0)
	merged := noticesOfKind(t, pool, "LEGION-1", "pr-merged")
	if len(merged) != 1 || !strings.Contains(merged[0].Reason, "LEGION-2 (backlog), LEGION-3 (icebox)") {
		t.Fatalf("pr-merged notices = %+v, want one naming each child not done with its status", merged)
	}

	mirrorChild(t, pool, "LEGION-2", "LEGION-1", "done", 2)
	closePass(t, pool, engine)
	assertOutboxCount(t, pool, "dispatch_status", 0)
	mirrorChild(t, pool, "LEGION-3", "LEGION-1", "done", 2)
	closePass(t, pool, engine)
	closePass(t, pool, engine)
	want := []record.StatusWrite{record.CloseStatusWrite("done", "in_progress", "", 1)}
	if got := statusWrites(t, pool, "LEGION-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("status writes = %#v, want the one close the pass queued once each child is done, %#v", got, want)
	}
	if got := storedIssue(t, pool, "LEGION-1"); got.Phase != phase.Merging || got.PendingClose == nil || got.PendingClose.Kind != "merge" {
		t.Fatalf("the issue before Dispatch shows done = phase %s, pending %+v; want it in merging with the merge's close pending", got.Phase, got.PendingClose)
	}
	assertOutboxCount(t, pool, "check_bound", 0)

	landClose(t, pool, engine, "LEGION-1")
	if got := storedIssue(t, pool, "LEGION-1"); got.Phase != phase.Done || got.PendingClose != nil {
		t.Fatalf("the issue once Dispatch shows done = phase %s, pending %+v; want done", got.Phase, got.PendingClose)
	}
	assertOutboxCount(t, pool, "check_bound", 1)
}

// A close Dispatch refuses for a child it was not done changes nothing (R24 of
// dispatch://INFRA-3320; intake.CloseRefused): the issue stays in its phase, the mark stays,
// refused, and the architect is told the children Dispatch named. The first pass whose copy shows
// each child the issue then has done queues the close again, once; a child a person unlinked
// since holds it no more. A refusal of a close of an earlier phase entry changes nothing.
func TestARefusedCloseKeepsItsMarkAndIsQueuedAgainOnceEachChildIsDone(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1",
		Project: "LEGION", Title: "Root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	if result := apply("close", intake.CloseRoot{Issue: "LEGION-1", Reason: "moot", By: architectOf(t, "LEGION-1")}); result.Refusal != nil {
		t.Fatalf("close refused: %+v", result.Refusal)
	}
	// A child filed after the close was decided, which Dispatch's refusal names.
	mirrorChild(t, pool, "LEGION-2", "LEGION-1", "triage", 1)

	apply("refused-earlier", intake.CloseRefused{Issue: "LEGION-1", Generation: 1, Entry: 7, Children: []string{"LEGION-2 (triage)"}})
	if got := storedIssue(t, pool, "LEGION-1"); got.PendingClose == nil || got.PendingClose.Refused {
		t.Fatalf("the close after a refusal of another entry = %+v, want it pending and not refused", got.PendingClose)
	}
	apply("refused", intake.CloseRefused{Issue: "LEGION-1", Generation: 1, Entry: 1, Children: []string{"LEGION-2 (triage)"}})
	root := storedIssue(t, pool, "LEGION-1")
	if root.Phase != phase.Admitted || root.Status != "in_progress" || root.PendingClose == nil || !root.PendingClose.Refused {
		t.Fatalf("the root after the refusal = phase %s, status %s, pending %+v; want it unchanged with its close refused", root.Phase, root.Status, root.PendingClose)
	}
	told := noticesOfKind(t, pool, "LEGION-1", "message-refused")
	if len(told) != 1 || !strings.Contains(told[0].Reason, "LEGION-2 (triage)") || !strings.Contains(told[0].Reason, "stays in admitted with its claims and its slot") {
		t.Fatalf("message-refused notices = %+v, want one naming the child and saying the issue stays", told)
	}

	closePass(t, pool, engine)
	if got := len(statusWrites(t, pool, "LEGION-1")); got != 1 {
		t.Fatalf("status writes after a pass with the child open = %d, want only the first close", got)
	}
	// A person unlinks the child, which stays open under no parent.
	mirrorChild(t, pool, "LEGION-2", "", "triage", 2)
	closePass(t, pool, engine)
	closePass(t, pool, engine)
	writes := statusWrites(t, pool, "LEGION-1")
	want := record.CloseStatusWrite("done", "in_progress", "Closed by its architect before its first phase: moot", 1)
	if len(writes) != 2 || writes[1] != want {
		t.Fatalf("status writes = %#v, want the close written again once, %#v", writes, want)
	}
	if got := storedIssue(t, pool, "LEGION-1"); got.PendingClose == nil || got.PendingClose.Refused {
		t.Fatalf("the close written again = %+v, want it pending and no longer refused", got.PendingClose)
	}
	if ops := supervisedOps(t, pool, "LEGION-1"); slices.Contains(ops, "issue_close") {
		t.Fatalf("supervise rows %v, want no claim closed before Dispatch shows done", ops)
	}
}
