package graphcensus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// orphanIssue is the issue seedOrphan writes its document on.
const orphanIssue = "ORPH-1"

// seedOrphan writes project ORPH, issue ORPH-1 with its node, and a document of the issue at
// ORPH-1/<slug> with its node, as this build's create writes them, and each of lenses, a lens of
// alice's on the document, whose write records the document's row (graphstore.AttributeArtifactID).
// It then removes the document's row as the release before this one does: the row goes, a removal
// event is appended, and nothing states the node or a lens gone, so each lens is stale. It answers
// the document's address and the removal event.
func seedOrphan(t *testing.T, database *store.Store, slug string, lenses ...string) (string, int64) {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	ref := orphanIssue + "/" + slug
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			insert into projects (key, name, default_assignee) values ('ORPH', 'Orphans', 'alice') on conflict do nothing;
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('ORPH-1', 'ORPH', 1, 'One', '{"kind":"user","id":"alice"}', 'U') on conflict do nothing
		`); err != nil {
			return err
		}
		if err := graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.IssueNode(orphanIssue, "ORPH", alice, time.Now().UTC()),
			graphstore.ArtifactNode(ref, "ORPH", "", alice, time.Now().UTC()),
		}, nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ('ORPH-1', 'ORPH', $1, $1, 'doc', '{"kind":"user","id":"alice"}')
		`, slug); err != nil {
			return err
		}
		at := time.Now().UTC()
		var placed []graphstore.Node
		var placements []graphstore.Tuple
		for _, lens := range lenses {
			placed = append(placed, graphstore.Node{Kind: graphstore.KindLens, ID: lens, Supertag: graphstore.KindLens,
				Visibility: graphstore.VisibilityProject, State: graphstore.StateLive, Project: "ORPH",
				MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent})
			placements = append(placements, graphstore.Tuple{FromKind: graphstore.KindLens, FromID: lens,
				Kind: graphstore.TuplePartOf, ToKind: graphstore.KindArtifact, ToID: ref,
				At: at, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true})
		}
		if len(placed) == 0 {
			return nil
		}
		return graphstore.Write(ctx, tx, placed, placements)
	}); err != nil {
		t.Fatalf("seed the document %s: %v", ref, err)
	}
	var removal int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		var artifact []byte
		if err := tx.QueryRow(ctx, `
			delete from artifacts where ref_key = $1
			returning jsonb_build_object('id', id::text, 'issue_key', issue_key, 'slug', slug, 'ref_key', ref_key)
		`, ref).Scan(&artifact); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify)
			select 'ORPH-1', coalesce(max(seq), 0) + 1, 'artifact.removed', '{"kind":"user","id":"alice"}',
			       jsonb_build_object('artifact', $1::jsonb), false
			from events where issue_key = 'ORPH-1'
			returning id
		`, artifact).Scan(&removal)
	}); err != nil {
		t.Fatalf("remove the document %s as the release before does: %v", ref, err)
	}
	return ref, removal
}

// seedBeneath writes what stands under the lens lens of seedOrphan's document as the reasoning
// write stores it: a finding of the lens, a block node whose id names the lens
// (graphstore.FindingID), and the risk risk, each live in project ORPH at the sentinel with its
// part_of the lens. A risk stands only under a pre-mortem, so it gives the lens that kind first
// (its has_kind tuple). It answers the finding's id.
func seedBeneath(t *testing.T, database *store.Store, lens, risk string) string {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Now().UTC()
	finding := graphstore.FindingID(lens, "first")
	var nodes []graphstore.Node
	tuples := []graphstore.Tuple{{FromKind: graphstore.KindLens, FromID: lens,
		Kind: graphstore.TupleHasKind, ToKind: graphstore.KindLensKind, ToID: graphstore.LensKindPreMortem,
		At: at, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true}}
	for _, beneath := range []struct{ kind, id string }{{refs.BlockNodeKind, finding}, {graphstore.KindRisk, risk}} {
		nodes = append(nodes, graphstore.Node{Kind: beneath.kind, ID: beneath.id, Supertag: beneath.kind,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateLive, Project: "ORPH",
			MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent})
		tuples = append(tuples, graphstore.Tuple{FromKind: beneath.kind, FromID: beneath.id,
			Kind: graphstore.TuplePartOf, ToKind: graphstore.KindLens, ToID: lens,
			At: at, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true})
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, nodes, tuples)
	}); err != nil {
		t.Fatalf("seed a finding and a risk under lens %s: %v", lens, err)
	}
	return finding
}

// A document the release before this one removed, after this build wrote its node, leaves the node
// standing behind no row, which OrphanedDocumentNodes reads and StateOrphanedGone states gone: its
// state tuple is gone, made by the block-index tick, caused by that release's own artifact.removed
// event, and the project census then reads the store complete. A second call leaves the gone node
// as it is.
//
// Its control is the gone state caused at the sentinel rather than by the removal's event: the
// tuple then names no event.
func TestAnOrphanedDocumentNodeIsStatedGoneUnderItsRemoval(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	ref, removal := seedOrphan(t, database, "notes-md")
	orphaned, err := OrphanedDocumentNodes(ctx, database.Pool, 10)
	if err != nil || len(orphaned) != 1 || orphaned[0] != ref {
		t.Fatalf("the orphaned document nodes are %q (%v), want %s alone", orphaned, err, ref)
	}
	if err := StateOrphanedGone(ctx, database, ref); err != nil {
		t.Fatalf("state the orphaned node of %s gone: %v", ref, err)
	}
	history, err := graphstore.History(ctx, database.Pool, graphstore.KindArtifact, ref, graphstore.AttributeState)
	if err != nil || len(history) != 2 {
		t.Fatalf("the state history of %s is %+v (%v), want live then gone", ref, history, err)
	}
	gone := history[1]
	if gone.ToID != graphstore.StateGone || gone.SourceEvent != removal || gone.Who != SweepActor {
		t.Errorf("the gone statement of %s is %+v, want gone, caused by the removal event %d, stated by %+v", ref, gone, removal, SweepActor)
	}
	requireComplete(t, ctx, database, "after the orphan was stated gone")
	if orphaned, _ := OrphanedDocumentNodes(ctx, database.Pool, 10); len(orphaned) != 0 {
		t.Errorf("after the sweep the orphaned document nodes are %q, want none", orphaned)
	}
	if err := StateOrphanedGone(ctx, database, ref); !errors.Is(err, ErrNotOrphaned) {
		t.Errorf("a second sweep of %s answered %v, want ErrNotOrphaned", ref, err)
	}
}

// A create of this build at the address of an orphaned node writes the node and then the row in
// one transaction, holding the node's row until it commits. The sweep takes the issue's row, then
// queues behind the create on the node's row; once the create commits it reads the row standing
// and leaves the node: the node is live, the created row stands, and the census reads nothing.
//
// Its control is the sweep's read of a standing row moved before its lock of the node: it then
// reads no row, states the created document's node gone, and the census names the row gone.
func TestTheOrphanSweepLosesNothingOfACreateItRacesWith(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	ref, _ := seedOrphan(t, database, "notes-md")
	create := &lockstep.Hold{Name: "a create of " + ref + " that wrote its node and its row"}
	lockstep.Begin(t, database, create)
	created := graphstore.PlanUncaused([]graphstore.Node{graphstore.ArtifactNode(
		ref, "ORPH", "", model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC())}, nil)
	if err := store.RunWrite(ctx, create.Tx, func(b *store.WriteBatch) error {
		graphstore.QueueNodes(b, created)
		return nil
	}); err != nil {
		t.Fatalf("write the created document's node: %v", err)
	}
	if _, err := create.Tx.Exec(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('ORPH-1', 'ORPH', 'notes-md', 'Made again', 'doc', '{"kind":"user","id":"alice"}')
	`); err != nil {
		t.Fatalf("write the created document's row: %v", err)
	}
	sweeper := storetest.SingleConnection(t, database)
	var pid uint32
	if err := sweeper.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the sweep's backend: %v", err)
	}
	swept := make(chan error, 1)
	go func() { swept <- StateOrphanedGone(ctx, sweeper, ref) }()
	if early, done := storetest.WaitForQueuedBehind(t, create.Tx, pid, swept, lockstep.Wait); done {
		t.Fatalf("the sweep answered %v without waiting on the node the create holds, so this test proves nothing", early)
	}
	t.Logf("the sweep waits behind %s at %q", create.Name, lockstep.WaitingStatement(t, database, pid))
	if err := create.Tx.Commit(ctx); err != nil {
		t.Fatalf("commit the create: %v", err)
	}
	create.Release()
	select {
	case err := <-swept:
		if !errors.Is(err, ErrNotOrphaned) {
			t.Errorf("the sweep queued behind the create answered %v, want ErrNotOrphaned", err)
		}
	case <-time.After(lockstep.Wait):
		t.Fatalf("the sweep did not answer within %s of the create's commit", lockstep.Wait)
	}
	node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindArtifact, ref)
	if err != nil || !held || node.State != graphstore.StateLive {
		t.Errorf("after the create and the sweep the node of %s is %+v (held %t, %v), want it live", ref, node, held, err)
	}
	requireComplete(t, ctx, database, "after the create and the sweep")
}

// The sweep of one orphaned document (StateOrphanedGone) takes the store's lock order: the issue's
// owner lock first, then the node's row, then the state tuple it appends under the removal's event, and
// never the project's row. Each step is held in turn while the sweep's own backend is read at the
// statement it waits at and the locks it holds.
func TestTheOrphanSweepTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	ref, removal := seedOrphan(t, database, "notes-md")
	sweeper := storetest.SingleConnection(t, database)
	var pid uint32
	if err := sweeper.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the sweep's backend: %v", err)
	}
	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue ORPH-1's owner lock", Step: lockstep.StepOwner, Taken: true,
		Statement: lockstep.OwnerLockStatement,
		Unlocked:  []string{"graph_nodes", "graph_tuples"},
	}, store.IssueOwner(orphanIssue))
	node := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: ref + "'s node row", Step: lockstep.StepNode, Taken: true, Kind: graphstore.KindArtifact, ID: ref,
		Statement: "select state from graph_nodes where kind = $1 and id = $2 for update",
		Unlocked:  []string{"graph_tuples"},
	}, `select 1 from graph_nodes where kind = $1 and id = $2 for update`, graphstore.KindArtifact, ref)
	state := lockstep.Hold{Name: ref + "'s gone tuple", Step: lockstep.StepTuple, Taken: true, Statement: "with written as ("}
	lockstep.Begin(t, database, &state)
	if err := graphstore.Write(ctx, state.Tx, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindArtifact, FromID: ref, Kind: graphstore.AttributeState,
		ToKind: graphstore.KindValue, ToID: graphstore.StateGone,
		At: time.Now().UTC(), Who: SweepActor, SourceEvent: removal, Asserted: true,
	}}); err != nil {
		t.Fatalf("hold %s: %v", state.Name, err)
	}
	project := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project ORPH's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for update`, "ORPH")
	swept := make(chan error, 1)
	go func() { swept <- StateOrphanedGone(ctx, sweeper, ref) }()
	lockstep.Follow(t, database, "the orphan sweep", pid, swept, []*lockstep.Hold{owner, node, &state, project})
	held, _, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindArtifact, ref)
	if err != nil || held.State != graphstore.StateGone {
		t.Errorf("after the sweep the node of %s is %+v (%v), want it gone: the run the test followed is not one that committed", ref, held, err)
	}
}

// The sweep of every stale lens (StateStaleLensesGone) takes the store's lock order: a read of the
// stale lenses with no lock, then the owner lock of the issue their document names, then the node
// rows of each lens and of all that stands under it in ascending (kind, id) - here a finding of the
// lens, a block node, which sorts below it, the lens, then a risk under it, which sorts above it -
// then their state tuples, in one write; it appends no event, and never takes the project's row.
// Each step is held in turn while the sweep's own backend is read at the statement it waits at and
// the locks it holds; the sweep queues behind them in that order, and the lens, its finding and its
// risk then read gone.
//
// The state tuples are appended at the sentinel, which no repeat key covers, so no row of them can
// be held: they are held as the writes of graph_tuples, in share mode, which the sweep's node move
// waits on as it appends them (graphstore's keptCauseNodeMove). Postgres takes a statement's table
// locks when it parses it, and pgx prepares every statement of a batch its connection has not
// cached before the first of them runs, so a sweep whose write's statements are new would queue
// behind that hold ahead of the node rows. A sweep of another stale lens, with a finding and a risk
// under it, therefore runs first on the sweep's one connection: each statement of the write is then
// cached, and takes its table locks when it runs.
//
// Its control is the owner lock taken out of the sweep: it then never queues behind the owner
// lock, and takes the three node rows and the state tuples without it.
func TestTheStaleLensSweepTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	const (
		cachingLens = "00000000-0000-4000-8000-0000000005b1"
		cachingRisk = "00000000-0000-4000-8000-0000000005b2"
		lens        = "00000000-0000-4000-8000-0000000005b3"
		risk        = "00000000-0000-4000-8000-0000000005b4"
	)
	sweeper := storetest.SingleConnection(t, database)
	var pid uint32
	if err := sweeper.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the sweep's backend: %v", err)
	}
	seedOrphan(t, database, "cached-md", cachingLens)
	seedBeneath(t, database, cachingLens, cachingRisk)
	if stated, err := StateStaleLensesGone(ctx, sweeper); err != nil || stated != 3 {
		t.Fatalf("the sweep that caches the write's statements stated %d nodes gone (%v), want the lens, its finding and its risk", stated, err)
	}
	seedOrphan(t, database, "notes-md", lens)
	finding := seedBeneath(t, database, lens, risk)

	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue ORPH-1's owner lock", Step: lockstep.StepOwner, Taken: true,
		Statement: lockstep.OwnerLockStatement,
	}, store.IssueOwner(orphanIssue))
	nodeRow := func(name, kind, id string) *lockstep.Hold {
		return lockstep.HoldRow(t, database, &lockstep.Hold{
			Name: name, Step: lockstep.StepNode, Taken: true, Kind: kind, ID: id,
			Statement: "insert into graph_nodes (",
		}, `select 1 from graph_nodes where kind = $1 and id = $2 for update`, kind, id)
	}
	findingRow := nodeRow("the finding's node row", refs.BlockNodeKind, finding)
	lensRow := nodeRow("the lens's node row", graphstore.KindLens, lens)
	riskRow := nodeRow("the risk's node row", graphstore.KindRisk, risk)
	states := lockstep.Hold{
		Name: "the writes of graph_tuples, which the state tuples take", Step: lockstep.StepTuple, Taken: true,
		Statement: "with incoming as (",
	}
	lockstep.Begin(t, database, &states)
	if _, err := states.Tx.Exec(ctx, `lock table graph_tuples in share mode`); err != nil {
		t.Fatalf("hold %s: %v", states.Name, err)
	}
	project := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project ORPH's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for update`, "ORPH")
	swept := make(chan error, 1)
	go func() {
		_, err := StateStaleLensesGone(ctx, sweeper)
		swept <- err
	}()
	queued := lockstep.Follow(t, database, "the stale lens sweep", pid, swept,
		[]*lockstep.Hold{owner, findingRow, lensRow, riskRow, &states, project})
	if got, want := lockstep.HoldNames(queued), lockstep.HoldNames([]*lockstep.Hold{owner, findingRow, lensRow, riskRow, &states}); got != want {
		t.Errorf("the stale lens sweep queued behind %s, want %s: the owner lock, the node rows in ascending (kind, id), then the state tuples", got, want)
	}
	for _, node := range []struct{ kind, id string }{{refs.BlockNodeKind, finding}, {graphstore.KindLens, lens}, {graphstore.KindRisk, risk}} {
		held, _, err := graphstore.ReadNode(ctx, database.Pool, node.kind, node.id)
		if err != nil || held.State != graphstore.StateGone {
			t.Errorf("after the sweep the node %s/%s is %+v (%v), want it gone: the run the test followed is not one that committed", node.kind, node.id, held, err)
		}
	}
}

// The object pass writes at most its budget of batches: over 201 asks owed their node, one more
// than the backfill's batch, a pass of one batch writes that batch and answers More, and the next
// writes the one left and answers no More.
//
// Its control is the budget not spent: the first pass then writes all 201 and answers no More.
func TestTheObjectPassStopsAtItsBatchBound(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedOrphanIssueWithAsks(t, database, backfillBatch+1)
	first, err := WriteOwedObjectNodes(ctx, database, 1)
	if err != nil || first.Rows != backfillBatch || !first.More {
		t.Fatalf("a pass of one batch answered %+v (%v), want %d rows and More", first, err, backfillBatch)
	}
	second, err := WriteOwedObjectNodes(ctx, database, 1)
	if err != nil || second.Rows != 1 || second.More {
		t.Fatalf("the next pass answered %+v (%v), want the one row left and no More", second, err)
	}
	requireComplete(t, ctx, database, "after the two passes")
}

// A row no pass can write - one between a party the store refuses, or one whose node the store
// holds in another project - neither spends the pass's budget nor keeps its More, so the tick's
// debt clears once only such rows are left, and they hold back no row after them.
//
// With a budget of one batch over an issue owed its node, two such documents (a conversation file
// whose session holds white space, and ORPH-1/a-md, whose node is gone in project ELSEWHERE) and
// an ask owed its node: the first pass writes the issue and answers More for the ask past its
// budget; the second reads the two documents, writes neither and spends nothing on them, writes the
// ask and answers no More; a third writes nothing and answers no More; and once another issue is
// owed its node, a fourth spends its batch on that issue, reads past the budget only the two
// documents, and answers no More.
//
// Its controls are the read past the budget taking any row its marker does not mark, with which
// the fourth pass answers More over the two documents, and a batch spending budget whatever it
// wrote, with which the second answers More, and so does every pass after it, with the ask left
// with no node.
func TestRowsNoPassCanWriteNeitherSpendTheBudgetNorKeepMore(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedOrphanIssueWithAsks(t, database, 1)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ('ELSEWHERE', 'Elsewhere', 'alice')`); err != nil {
			return err
		}
		moved := graphstore.ArtifactNode(orphanIssue+"/a-md", "ELSEWHERE", "", alice, time.Now().UTC().Add(-time.Hour))
		if err := graphstore.Write(ctx, tx, []graphstore.Node{moved}, nil); err != nil {
			return err
		}
		moved.State, moved.MadeAt = graphstore.StateGone, time.Now().UTC().Add(-time.Minute)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{moved}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('ORPH-2', 'ORPH', 2, 'Two', '{"kind":"user","id":"alice"}', 'V');
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ('ORPH-1', 'ORPH', 'a-md', 'A', 'doc', '{"kind":"user","id":"alice"}');
			insert into artifacts (session_id, slug, name, kind, created_by)
			values ('ses bad', 'notes-md', 'Notes', 'file', '{"kind":"session","id":"ses bad"}');
		`)
		return err
	}); err != nil {
		t.Fatalf("seed the issue, the two documents no pass can write and the ask: %v", err)
	}
	passes := []struct {
		rows int64
		more bool
	}{{rows: 1, more: true}, {rows: 1, more: false}, {rows: 0, more: false}, {rows: 1, more: false}}
	for run, want := range passes {
		if run == 3 {
			if _, err := database.Pool.Exec(ctx, `
				insert into issues (key, project_key, number, title, created_by, rank)
				values ('ORPH-3', 'ORPH', 3, 'Three', '{"kind":"user","id":"alice"}', 'W')
			`); err != nil {
				t.Fatalf("seed issue ORPH-3 with no node: %v", err)
			}
		}
		pass, err := WriteOwedObjectNodes(ctx, database, 1)
		if err != nil || pass.Rows != want.rows || pass.More != want.more {
			t.Fatalf("pass %d of one batch answered %+v (%v), want %d row(s) and More %t", run+1, pass, err, want.rows, want.more)
		}
	}
	if node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindIssue, "ORPH-2"); err != nil || !held || node.Project != "ORPH" {
		t.Errorf("after the passes the node of issue ORPH-2 is %+v (held %t, %v), want it in ORPH", node, held, err)
	}
	var askNodes int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_nodes where kind = 'ask' and project_key = 'ORPH'`).Scan(&askNodes); err != nil || askNodes != 1 {
		t.Errorf("after the passes %d ask node(s) name ORPH (%v), want the one ask's", askNodes, err)
	}
}

// A backlog of rows no pass can write - more than one batch of them - opens no transaction of the
// pass and takes no owner lock, so a pass over it ends within its bound even while a writer holds
// the owners' locks: 200 conversation files whose session holds white space and ORPH-1/a-md, whose
// node is gone in project ELSEWHERE, behind an issue and an ask that hold their nodes. With issue
// ORPH-1's owner lock held by another transaction, a pass of one batch answers no row, no More and
// the moved row, well before the test's deadline.
//
// Its control is every batch written through writeObjectBatch, as before writableRows: the
// first batch, which holds ORPH-1/a-md, queues behind the held owner lock and the pass runs out
// the deadline.
func TestABacklogOfRowsNoPassCanWriteTakesNoOwnerLock(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedOrphanIssueWithAsks(t, database, 1)
	writeEveryOwedNode(t, database)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ('ELSEWHERE', 'Elsewhere', 'alice')`); err != nil {
			return err
		}
		moved := graphstore.ArtifactNode(orphanIssue+"/a-md", "ELSEWHERE", "", alice, time.Now().UTC().Add(-time.Hour))
		if err := graphstore.Write(ctx, tx, []graphstore.Node{moved}, nil); err != nil {
			return err
		}
		// A node not gone that names a project is marked, and its row read by no pass; gone,
		// it leaves the row unmarked, and its project another than the row's.
		moved.State, moved.MadeAt = graphstore.StateGone, time.Now().UTC().Add(-time.Minute)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{moved}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ('ORPH-1', 'ORPH', 'a-md', 'A', 'doc', '{"kind":"user","id":"alice"}');
			insert into artifacts (session_id, slug, name, kind, created_by)
			select 'ses bad', 'n-' || g, 'Notes', 'file', '{"kind":"session","id":"ses bad"}'
			from generate_series(1, 200) as g;
		`)
		return err
	}); err != nil {
		t.Fatalf("seed the backlog no pass can write: %v", err)
	}
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the owner lock's holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := store.LockOwners(ctx, holder, store.IssueOwner(orphanIssue)); err != nil {
		t.Fatalf("hold issue %s's owner lock: %v", orphanIssue, err)
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	started := time.Now()
	pass, err := WriteOwedObjectNodes(bounded, database, 1)
	if err != nil || pass.Rows != 0 || pass.More || pass.Moved == nil {
		t.Fatalf("a pass of one batch over the backlog answered %+v (%v) after %s, want no row, no More and the moved row, with %s's owner lock held elsewhere",
			pass, err, time.Since(started).Round(time.Millisecond), orphanIssue)
	}
}

// A row whose node is gone in another project stays unmarked, so every pass reads it again, and a
// batch that holds a writable row beside such rows leaves them before its transaction opens: they
// cost the read of their projects and no transaction, owner lock or transaction id each, on every
// pass. Twenty documents of ORPH-1 whose nodes are gone in project ELSEWHERE and one writable
// document, all in one batch: the pass writes the one row in the one transaction it opens and
// answers the twenty in Moved.
//
// Its control is writableRows answering every row writable, as before it left these: the batch
// fails on the first moved node and is written again one row at a time, 22 transactions in all.
func TestRowsWhoseNodeIsGoneInAnotherProjectOpenNoTransaction(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedOrphanIssueWithAsks(t, database, 0)
	writeEveryOwedNode(t, database)
	const movedRows = 20
	alice := model.Actor{Kind: "user", ID: "alice"}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ('ELSEWHERE', 'Elsewhere', 'alice')`); err != nil {
			return err
		}
		var nodes []graphstore.Node
		for index := range movedRows {
			nodes = append(nodes, graphstore.ArtifactNode(fmt.Sprintf("%s/m-%02d-md", orphanIssue, index), "ELSEWHERE", "", alice,
				time.Now().UTC().Add(-time.Hour)))
		}
		if err := graphstore.Write(ctx, tx, nodes, nil); err != nil {
			return err
		}
		for index := range nodes {
			nodes[index].State, nodes[index].MadeAt = graphstore.StateGone, time.Now().UTC().Add(-time.Minute)
		}
		if err := graphstore.Write(ctx, tx, nodes, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			select 'ORPH-1', 'ORPH', 'm-' || lpad(g::text, 2, '0') || '-md', 'Moved', 'doc', '{"kind":"user","id":"alice"}'::jsonb
			from generate_series(0, $1::integer - 1) as g
			union all
			select 'ORPH-1', 'ORPH', 'w-md', 'Writable', 'doc', '{"kind":"user","id":"alice"}'::jsonb
		`, movedRows)
		return err
	}); err != nil {
		t.Fatalf("seed the rows whose nodes are gone in another project and the writable row: %v", err)
	}
	begins := &beginCounter{}
	traced := storetest.TracedStore(t, database, begins)
	pass, err := WriteOwedObjectNodes(ctx, traced, 0)
	var moved interface{ Unwrap() []error }
	if err != nil || pass.Rows != 1 || !errors.As(pass.Moved, &moved) || len(moved.Unwrap()) != movedRows {
		t.Fatalf("the pass answered %+v (%v), want the writable row written and the %d rows whose nodes are in ELSEWHERE in Moved",
			pass, err, movedRows)
	}
	if opened := begins.count.Load(); opened != 1 {
		t.Errorf("the pass opened %d transaction(s), want 1: the writable row's batch alone", opened)
	}
}

// beginCounter is a pgx tracer that counts the transactions a pool opens: pgx sends each BEGIN as a
// statement of its own.
type beginCounter struct{ count atomic.Int64 }

func (c *beginCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(data.SQL)), "begin") {
		c.count.Add(1)
	}
	return ctx
}

func (*beginCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A kind whose batch fails holds back no other kind: with the store refusing every ask's node, the
// pass still writes the message's node, and answers the asks' failure.
//
// Its control is the pass stopping at the first failing kind: the message then keeps no node.
func TestAFailingKindHoldsBackNoOtherKind(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedOrphanIssueWithAsks(t, database, 1)
	const message = "33333333-3333-4333-8333-0000000000aa"
	for _, statement := range []string{
		`insert into messages (id, issue_key, author, body) values ('` + message + `', 'ORPH-1', '{"kind":"user","id":"alice"}', 'A message.')`,
		`create function refuse_ask_nodes() returns trigger language plpgsql as $$
		 begin raise exception 'the test refuses ask nodes'; end $$`,
		`create trigger refuse_ask_nodes before insert on graph_nodes for each row
		 when (new.kind = 'ask') execute function refuse_ask_nodes()`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the message and refuse ask nodes: %v\n%s", err, statement)
		}
	}
	pass, err := WriteOwedObjectNodes(ctx, database, 0)
	if err == nil {
		t.Fatalf("the pass with ask nodes refused answered %+v and no error, want the asks' failure", pass)
	}
	if _, held, readErr := graphstore.ReadNode(ctx, database.Pool, graphstore.KindMessage, message); readErr != nil || !held {
		t.Errorf("after the pass the message holds a node %t (%v), want it written beside the failing asks: %v", held, readErr, err)
	}
}

// A row whose node the store holds in another project is one no pass can write, since a node's
// project never moves (graphstore.ErrProjectMoved), and no writer makes one: it is a store a
// person has to read. It holds back no other row: with a gone node at ORPH-1/a-md in project
// ELSEWHERE and rows a-md and b-md in ORPH, the pass writes b-md's node, answers a-md in Moved, and
// answers no failure, and a second pass does the same. The project census names a-md `differs`,
// with the cure no command gives, and not `gone` with the backfill the passes just ran.
//
// Its controls are the batch written whole, with neither the rows in another project left before
// the write (writableRows) nor the retry of its rows one at a time, with which the pass fails at
// that batch and b-md keeps no node, and the census reading gone before another project, with
// which it names a-md gone and the backfill as its cure.
func TestARowWhoseNodeStandsInAnotherProjectHoldsBackNoOtherRow(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedOrphanIssueWithAsks(t, database, 0)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ('ELSEWHERE', 'Elsewhere', 'alice')`); err != nil {
			return err
		}
		moved := graphstore.ArtifactNode(orphanIssue+"/a-md", "ELSEWHERE", "", alice, time.Now().UTC().Add(-time.Hour))
		if err := graphstore.Write(ctx, tx, []graphstore.Node{moved}, nil); err != nil {
			return err
		}
		moved.State, moved.MadeAt = graphstore.StateGone, time.Now().UTC().Add(-time.Minute)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{moved}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ('ORPH-1', 'ORPH', 'a-md', 'A', 'doc', '{"kind":"user","id":"alice"}'),
			       ('ORPH-1', 'ORPH', 'b-md', 'B', 'doc', '{"kind":"user","id":"alice"}')
		`)
		return err
	}); err != nil {
		t.Fatalf("seed the node in another project and the two rows: %v", err)
	}
	for run := 1; run <= 2; run++ {
		pass, err := WriteOwedObjectNodes(ctx, database, 0)
		if err != nil || !errors.Is(pass.Moved, graphstore.ErrProjectMoved) || !strings.Contains(pass.Moved.Error(), orphanIssue+"/a-md") {
			t.Fatalf("pass %d answered %+v and %v, want a-md alone in Moved and no failure", run, pass, err)
		}
		if strings.Contains(pass.Moved.Error(), orphanIssue+"/b-md") {
			t.Errorf("pass %d left b-md as moved: %v", run, pass.Moved)
		}
		node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindArtifact, orphanIssue+"/b-md")
		if err != nil || !held || node.Project != "ORPH" || node.State != graphstore.StateLive {
			t.Errorf("after pass %d the node of b-md is %+v (held %t, %v), want it live in ORPH", run, node, held, err)
		}
	}
	incomplete, err := ProjectComplete(ctx, database.Pool)
	want := "artifact " + orphanIssue + "/a-md: " + classDiffers + differsCure
	if err != nil || len(incomplete) != 1 || incomplete[0] != want {
		t.Errorf("the project census names %q (%v), want exactly %q", incomplete, err, want)
	}
}

// The backfill command holds back no pass for such a row: with the moved node beside a comment
// whose node names no project, as the comment writer of the build before stored it, the run
// writes b-md's node and gives the comment's node its project, and then answers the moved row.
//
// Its control is Backfill returning at the object pass's moved row: the comment's node then
// names no project.
func TestTheBackfillRunsEveryPassPastARowWhoseNodeStandsInAnotherProject(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedOrphanIssueWithAsks(t, database, 0)
	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Now().UTC().Add(-time.Hour)
	const comment = "44444444-4444-4444-8444-0000000000aa"
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ('ELSEWHERE', 'Elsewhere', 'alice')`); err != nil {
			return err
		}
		moved := graphstore.ArtifactNode(orphanIssue+"/a-md", "ELSEWHERE", "", alice, at)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{moved}, nil); err != nil {
			return err
		}
		moved.State, moved.MadeAt = graphstore.StateGone, at.Add(time.Minute)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{moved}, nil); err != nil {
			return err
		}
		commentNode, tuples := graphstore.CommentFacts(nil, comment, alice, at, graphstore.NoEvent, "", "", "", nil)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{commentNode}, tuples); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into comments (id, issue_key, author, body, created_at, node_kind, node_address)
			values ($1::uuid, 'ORPH-1', '{"kind":"user","id":"alice"}', 'A remark.', $2, 'comment', $1)
		`, comment, at); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ('ORPH-1', 'ORPH', 'a-md', 'A', 'doc', '{"kind":"user","id":"alice"}'),
			       ('ORPH-1', 'ORPH', 'b-md', 'B', 'doc', '{"kind":"user","id":"alice"}')
		`)
		return err
	}); err != nil {
		t.Fatalf("seed the moved node, the two rows and the comment: %v", err)
	}
	if _, err := Backfill(ctx, database, &bytes.Buffer{}); !errors.Is(err, graphstore.ErrProjectMoved) {
		t.Fatalf("the backfill answered %v, want the moved row", err)
	}
	for _, want := range []struct{ kind, id string }{
		{graphstore.KindArtifact, orphanIssue + "/b-md"},
		{graphstore.KindComment, comment},
	} {
		node, held, err := graphstore.ReadNode(ctx, database.Pool, want.kind, want.id)
		if err != nil || !held || node.Project != "ORPH" {
			t.Errorf("after the backfill the node of %s %s is %+v (held %t, %v), want it in ORPH", want.kind, want.id, node, held, err)
		}
	}
}

// seedOrphanIssueWithAsks writes project ORPH and issue ORPH-1 with their nodes, and asks rows on
// the issue with no node, as the release before this one writes them.
func seedOrphanIssueWithAsks(t *testing.T, database *store.Store, asks int) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			insert into projects (key, name, default_assignee) values ('ORPH', 'Orphans', 'alice');
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('ORPH-1', 'ORPH', 1, 'One', '{"kind":"user","id":"alice"}', 'U')
		`); err != nil {
			return err
		}
		if err := graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.IssueNode(orphanIssue, "ORPH", model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC()),
		}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into asks (issue_key, author, question)
			select 'ORPH-1', '{"kind":"user","id":"alice"}', 'Question ' || n from generate_series(1, $1) n
		`, asks)
		return err
	}); err != nil {
		t.Fatalf("seed the issue and its asks: %v", err)
	}
}
