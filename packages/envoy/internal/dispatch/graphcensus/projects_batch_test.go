package graphcensus

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Each line ProjectComplete answers ends with its class's cure. Here an issue's node names neither
// a project nor parties (unscoped), whose cure is the backfill, and another issue's node names a
// project other than its row's (differs), which no command cures, since a node's project never
// moves. The backfill then gives the first node its project, as its line says, and leaves the
// second, which its line says it would.
//
// Its control is the backfill's marker read as any live node, a node naming neither included: the
// backfill then leaves the unscoped node as it is, and its line stays.
func TestEachIncompleteLineNamesItsCureAndTheBackfillCuresAnUnscopedNode(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CURE', 'Cures', 'alice'), ('OTHR', 'Other', 'alice')
	`); err != nil {
		t.Fatalf("seed the projects: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank, created_at)
		values ('CURE-1', 'CURE', 1, 'Unscoped', '{"kind":"user","id":"alice"}', 'U', $1),
		       ('CURE-2', 'CURE', 2, 'Differs', '{"kind":"user","id":"alice"}', 'V', $1)
	`, at); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.IssueNode("CURE-1", "", alice, at),
			graphstore.IssueNode("CURE-2", "OTHR", alice, at),
		}, nil)
	}); err != nil {
		t.Fatalf("write the two issues' nodes: %v", err)
	}
	unscoped := "issue CURE-1: unscoped" + backfillCure
	differs := "issue CURE-2: differs" + differsCure
	incomplete, err := ProjectComplete(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the incomplete rows: %v", err)
	}
	if want := []string{unscoped, differs}; !slices.Equal(incomplete, want) {
		t.Fatalf("ProjectComplete answers %q, want %q", incomplete, want)
	}
	if _, err := Backfill(ctx, database, &bytes.Buffer{}); err != nil {
		t.Fatalf("run the backfill: %v", err)
	}
	if incomplete, err = ProjectComplete(ctx, database.Pool); err != nil {
		t.Fatalf("read the incomplete rows after the backfill: %v", err)
	}
	if want := []string{differs}; !slices.Equal(incomplete, want) {
		t.Errorf("after the backfill ProjectComplete answers %q, want %q: the unscoped node given its project, the differing one left", incomplete, want)
	}
	node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindIssue, "CURE-1")
	if err != nil || !held || node.Project != "CURE" {
		t.Errorf("after the backfill the node of CURE-1 is %+v (held %t, %v), want it in project CURE", node, held, err)
	}
	// The node was live and its row's own: the backfill states its project and nothing else, so
	// its state history is the one live statement it was written with.
	history, err := graphstore.History(ctx, database.Pool, graphstore.KindIssue, "CURE-1", graphstore.AttributeState)
	if err != nil || len(history) != 1 || history[0].ToID != graphstore.StateLive {
		t.Errorf("after the backfill the state history of CURE-1 is %+v, %v; want its one live statement", history, err)
	}
}

// A removal of this build takes the document's issue owner lock, states its node gone and deletes
// the row, and commits. A batch of the backfill that read the row before then takes the same owner
// lock before it writes (lockObjectOwners), so it waits for the removal, reads under that lock that
// the document is gone, and writes no node for it: the removal's gone statement stands, and the
// census names nothing. Here the removal holds the issue's owner lock, with its node stated gone
// and its row deleted, while the batch is shown queued behind it; then the removal commits.
//
// Its control is the batch's owner lock taken out: the batch then reads the row the removal has not
// committed yet, waits only on the node, and moves it live after the removal commits, which the
// census names orphaned.
func TestTheObjectBatchWaitsForARemovalOnTheIssueRowAndWritesNoNode(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	created := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIdentityIssue(t, database, created, alice)
	document := insertIdentityDocument(t, database, "alice", created)
	kind := objectKindOf(t, graphstore.KindArtifact)
	nodes, err := readObjectNodes(ctx, database, kind.kind, kind.read, kind.start, backfillBatch)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("read the document no node marks: %d node(s), %v; want 1", len(nodes), err)
	}

	removal, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the removal: %v", err)
	}
	defer removal.Rollback(ctx)
	if err := store.LockOwners(ctx, removal, store.IssueOwner("IDEN-1")); err != nil {
		t.Fatalf("take the issue's owner lock as the removal does: %v", err)
	}
	gone := graphstore.ArtifactNode("IDEN-1/notes-md", "IDEN", "", alice, created)
	gone.State = graphstore.StateGone
	if err := graphstore.Write(ctx, removal, []graphstore.Node{gone}, nil); err != nil {
		t.Fatalf("state the node gone: %v", err)
	}
	if _, err := removal.Exec(ctx, `delete from artifacts where id = $1`, document); err != nil {
		t.Fatalf("delete the document's row: %v", err)
	}
	type answer struct {
		written KindBackfill
		err     error
	}
	answered := make(chan answer, 1)
	go func() {
		written, err := writeObjectBatch(ctx, database, kind, nodes)
		answered <- answer{written, err}
	}()
	if queued := storetest.WaitForLockWaiters(t, removal, 1); queued < 1 {
		t.Fatal("the batch never queued behind the removal, so this test proves nothing")
	}
	if err := removal.Commit(ctx); err != nil {
		t.Fatalf("commit the removal: %v", err)
	}
	var got answer
	select {
	case got = <-answered:
	case <-time.After(30 * time.Second):
		t.Fatal("the batch did not answer within 30s of the removal's commit")
	}
	if got.err != nil || got.written.Nodes != 0 {
		t.Errorf("the batch answered %+v, %v; want no node written for a row the removal took", got.written, got.err)
	}
	requireComplete(t, ctx, database, "after the removal and the batch")
}

// A conversation's file whose session id holds white space is between a party the parties' join
// refuses (graphstore.PartiesValue), and so is a comment on it. No writer of this build writes such
// a row, but a row of an earlier build may hold one. The backfill skips each such row, counts it in
// the report's Skipped, and writes every other row's node, so one such row stops neither the batch
// it falls in nor any later run. The census names the skipped document missing, and since no
// command writes its node, its line says so rather than sending the operator to the backfill.
//
// Its controls are the refused party's skip taken out of the object pass (writableRows answering
// such a row writable), which makes the backfill fail on the join's refusal and write nothing
// more, and the refused party's cure taken out of the census, whose line then names the backfill.
func TestTheBackfillSkipsARowBetweenARefusedPartyAndWritesTheRest(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	created := time.Date(2026, 11, 17, 11, 0, 0, 0, time.UTC)
	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIdentityIssue(t, database, created, alice)
	insertIdentityDocument(t, database, "alice", created)
	var bad, badRef string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (session_id, slug, name, kind, created_by, created_at)
		values ('ses bad', 'notes-md', 'Notes', 'file', '{"kind":"session","id":"ses bad"}', $1)
		returning id::text, ref_key
	`, created).Scan(&bad, &badRef); err != nil {
		t.Fatalf("write the conversation file whose session holds white space: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (artifact_id, author, body) values ($1::uuid, '{"kind":"session","id":"ses bad"}', 'A remark.')
	`, bad); err != nil {
		t.Fatalf("write the comment on it: %v", err)
	}

	report, err := Backfill(ctx, database, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("the backfill answered %v, want it to skip the row between a refused party", err)
	}
	if done := reportedKind(t, report, graphstore.KindArtifact); done.Skipped != 1 || done.Nodes != 1 {
		t.Errorf("the backfill reports %+v for documents, want one node written and one row skipped", done)
	}
	if node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindArtifact, "IDEN-1/notes-md"); err != nil || !held || node.State != graphstore.StateLive {
		t.Errorf("the issue's document's node is %+v (held %t, %v), want live: one bad row stops nothing else", node, held, err)
	}
	incomplete, err := ProjectComplete(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the incomplete rows: %v", err)
	}
	if !slices.Contains(incomplete, "artifact "+badRef+": missing"+refusedCure) {
		t.Errorf("ProjectComplete answers %q, want it to name the skipped file missing, with no command to cure it", incomplete)
	}
}
