package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A create's transaction can start while the removal of the document at its address is still
// under way: the removal holds the issue's owner lock, the create queues behind it, and the
// removal states the node gone and deletes the row before it commits. The create dates its row
// and its node's live statement with one time it reads from the database's clock after it holds
// the issue's owner lock (graphstore.StatementTime), so that time is later than the removal's gone
// statement: the node's newest state statement, read by the order the store appends it in and by
// the order of its time alike, is the new document's live one, dated at its row's created_at.
//
// Its control is the create dated at its transaction's start (now()): its live statement is then
// dated before the removal's gone one, and a read of the node's state as of now answers gone.
func TestACreateThatWaitedOnARemovalDatesItsDocumentAfterIt(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "CLK", "A document made while its namesake is removed", "The spec.\n")
	first := emptyDocument(t, handler, issue.Key, "Scratch.md")

	removal, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the removal: %v", err)
	}
	defer removal.Rollback(ctx)
	if err := store.LockOwners(ctx, removal, store.IssueOwner(issue.Key)); err != nil {
		t.Fatalf("take the issue's owner lock as the removal does: %v", err)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts",
			map[string]any{"name": "Scratch.md"}, "alice")
	}()
	// The create's own owner lock, which every upload takes before its owner check (issueStatus
	// under lockIssue): a document settlement queues on the issue's owner lock too, and is never
	// taken for the create, since it runs no settlement while this test holds the lock.
	waitForLockWait(t, database, `%public.owner_lock(%`)
	// The create's transaction has begun. The removal states the node gone at its own time and
	// deletes the row, as removeArtifact does, then commits.
	goneAt, err := graphstore.StatementTime(ctx, removal, graphstore.KindArtifact, first.RefKey)
	if err != nil {
		t.Fatalf("read the removal's time: %v", err)
	}
	gone := graphstore.ArtifactNode(first.RefKey, "CLK", "", model.Actor{Kind: "user", ID: "alice"}, goneAt)
	gone.State = graphstore.StateGone
	if err := graphstore.Write(ctx, removal, []graphstore.Node{gone}, nil); err != nil {
		t.Fatalf("state the node gone: %v", err)
	}
	clearAndDeleteArtifact(t, ctx, removal, first.ID)
	if err := removal.Commit(ctx); err != nil {
		t.Fatalf("commit the removal: %v", err)
	}

	var response *httptest.ResponseRecorder
	select {
	case response = <-answered:
	case <-time.After(30 * time.Second):
		t.Fatal("the create did not answer within 30s of the removal's commit")
	}
	if response.Code != http.StatusCreated {
		t.Fatalf("the create answered %d: %s", response.Code, response.Body.String())
	}
	second := decodeBody[uploadedArtifact](t, response).Artifact
	if second.RefKey != first.RefKey {
		t.Fatalf("the create took the address %q, want the removed one's %q, so this test proves nothing", second.RefKey, first.RefKey)
	}
	if !second.CreatedAt.After(goneAt) {
		t.Errorf("the new document's created_at %s is not after the removal's gone statement %s", second.CreatedAt, goneAt)
	}
	history, err := graphstore.History(ctx, database.Pool, graphstore.KindArtifact, second.RefKey, graphstore.AttributeState)
	if err != nil || len(history) == 0 {
		t.Fatalf("read the state history of %s: %d tuple(s), %v", second.RefKey, len(history), err)
	}
	if newest := history[len(history)-1]; newest.ToID != graphstore.StateLive || !newest.At.Equal(second.CreatedAt) {
		t.Errorf("the newest state statement of %s, by the order the store appended it in, is %s at %s, want live at the new row's created_at %s",
			second.RefKey, newest.ToID, newest.At, second.CreatedAt)
	}
	var asOfNow string
	if err := database.Pool.QueryRow(ctx, `
		select to_id from graph_tuples
		where from_kind = 'artifact' and from_id = $1 and kind = 'state' and to_kind = 'value' and at <= clock_timestamp()
		order by at desc, seq desc limit 1
	`, second.RefKey).Scan(&asOfNow); err != nil {
		t.Fatalf("read the state of %s as of now: %v", second.RefKey, err)
	}
	if asOfNow != graphstore.StateLive {
		t.Errorf("the state of %s as of now, by the order of its statements' time, is %s, want live", second.RefKey, asOfNow)
	}
	requireProjectsComplete(t, database)
}

// This build removes a document, stating its node gone, and a build before this one makes another
// at its address, which writes no node and dates its row at its transaction's start, here before
// the gone statement. When this build removes that second document, its node is gone already and
// the removal leaves it gone: it states nothing, rather than first moving the node live at the
// second row's created_at, before the gone statement it would then follow. The node's state
// history is the first document's live and gone, in time order, and the census names nothing.
//
// Its control is the removal's first-node write made whatever the store holds: the history then
// gains a live statement dated before the gone one it follows.
func TestARemovalLeavesTheGoneNodeOfAnEarlierBuildsDocumentGone(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "GNE", "A document an earlier build made where one was removed", "The spec.\n")
	first := emptyDocument(t, handler, issue.Key, "Scratch.md")
	removedDocument(t, handler, first.ID)
	history, err := graphstore.History(ctx, database.Pool, graphstore.KindArtifact, first.RefKey, graphstore.AttributeState)
	if err != nil || len(history) != 2 || history[1].ToID != graphstore.StateGone {
		t.Fatalf("after the first removal the state history of %s is %+v, %v; want live then gone", first.RefKey, history, err)
	}
	goneAt := history[1].At
	var second, ref string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by, created_at)
		select a.issue_key, i.project_key, 'scratch-md', 'Scratch.md', 'doc', '{"kind":"user","id":"bob"}', $2
		from (select $1::text as issue_key) a join issues i on i.key = a.issue_key
		returning id::text, ref_key
	`, issue.Key, goneAt.Add(-time.Second)).Scan(&second, &ref); err != nil {
		t.Fatalf("make the second document as an earlier build does: %v", err)
	}
	if ref != first.RefKey {
		t.Fatalf("the second document took the address %q, want the removed one's %q, so this test proves nothing", ref, first.RefKey)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 1, '', '[{"kind":"user","id":"bob"}]')
	`, second); err != nil {
		t.Fatalf("store the second document's empty first version: %v", err)
	}
	removedDocument(t, handler, second)
	after, err := graphstore.History(ctx, database.Pool, graphstore.KindArtifact, first.RefKey, graphstore.AttributeState)
	if err != nil {
		t.Fatalf("read the state history of %s: %v", first.RefKey, err)
	}
	if len(after) != 2 {
		t.Errorf("after the second removal the state history of %s holds %d statement(s), want the first document's two: %+v", first.RefKey, len(after), after)
	}
	for index := 1; index < len(after); index++ {
		if after[index].At.Before(after[index-1].At) {
			t.Errorf("the state history of %s is out of time order at %d: %s at %s after %s at %s", first.RefKey, index,
				after[index].ToID, after[index].At, after[index-1].ToID, after[index-1].At)
		}
	}
	requireProjectsComplete(t, database)
}

// removeAsAnEarlierBuild removes the untouched document as a build before this one does
// (earlierBuildRemoval), failing the test where it cannot.
func removeAsAnEarlierBuild(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	if err := earlierBuildRemoval(context.Background(), database, artifactID); err != nil {
		t.Fatalf("remove document %s as an earlier build does: %v", artifactID, err)
	}
}

// earlierBuildRemoval removes the untouched document as a build before this one does, in one
// transaction: the document's owner lock first, as every writer of the store takes it
// (events.LockOwnerRows, the order a settlement of the document takes it in), then the rows
// removeArtifact clears and the artifact row, and no node written.
func earlierBuildRemoval(ctx context.Context, database *store.Store, artifactID string) error {
	return pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		artifact, err := loadArtifactRowByID(ctx, tx, everyReader, artifactID)
		if err != nil {
			return fmt.Errorf("read document %s: %w", artifactID, err)
		}
		owners := events.OwnerRows{}
		var issue string
		if artifact.IssueKey != nil {
			issue = *artifact.IssueKey
		}
		owners.Add(issue, artifact.ID)
		if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
			return err
		}
		return deleteArtifactRows(ctx, tx, artifact)
	})
}

// removeBesideASettlement removes the untouched document as an earlier build does while a
// transaction holds what a settlement of it takes, in the store's lock order: the issue's owner
// lock, then the document's revision fence row (document_block_versions), and then, once the
// removal waits, the document's own row `for key share`, as a version's insert checks its foreign
// key. The removal must wait at the issue's owner lock, so the settlement's last step takes nothing
// the removal holds; it then commits, and the removal finishes.
func removeBesideASettlement(t *testing.T, database *store.Store, issue, artifactID string) {
	t.Helper()
	ctx := context.Background()
	settlement, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the settlement: %v", err)
	}
	defer settlement.Rollback(ctx)
	if err := store.LockOwners(ctx, settlement, store.IssueOwner(issue)); err != nil {
		t.Fatalf("take the issue's owner lock as a settlement does: %v", err)
	}
	if tag, err := settlement.Exec(ctx, `select 1 from document_block_versions where artifact_id = $1::uuid for no key update`, artifactID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("take the document's revision fence as a settlement does: %d row(s), %v", tag.RowsAffected(), err)
	}
	// The removal runs on a connection of its own, so the wait read is its own and never a
	// background settlement's queued on the same row.
	remover := storetest.SingleConnection(t, database)
	var pid uint32
	if err := remover.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the removal's backend: %v", err)
	}
	removed := make(chan error, 1)
	go func() { removed <- earlierBuildRemoval(ctx, remover, artifactID) }()
	var waitingAt string
	for deadline := time.Now().Add(10 * time.Second); ; {
		if err := database.Pool.QueryRow(ctx, `
			select coalesce((select query from pg_stat_activity where pid = $1 and wait_event_type = 'Lock'), '')
		`, pid).Scan(&waitingAt); err != nil {
			t.Fatalf("read where the removal waits: %v", err)
		}
		if waitingAt != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the removal waited on no lock within 10s of the settlement taking the issue's owner lock and the fence, so this row proves nothing")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("the removal waits at %q", strings.Join(strings.Fields(waitingAt), " "))
	if _, err := settlement.Exec(ctx, `select 1 from artifacts where id = $1::uuid for key share`, artifactID); err != nil {
		t.Fatalf("take the document's row as a settlement's version insert does, with the removal waiting: %v", err)
	}
	if err := settlement.Commit(ctx); err != nil {
		t.Fatalf("commit the settlement: %v", err)
	}
	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("remove document %s beside the settlement as an earlier build does: %v", artifactID, err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the removal did not finish within 30s of the settlement's commit")
	}
}

// clearAndDeleteArtifact runs, in tx, the statements removeArtifact runs to take an untouched
// document's rows away (deleteArtifactRows), failing the test where one fails.
func clearAndDeleteArtifact(t *testing.T, ctx context.Context, tx pgx.Tx, artifactID string) {
	t.Helper()
	artifact, err := loadArtifactRowByID(ctx, tx, everyReader, artifactID)
	if err != nil {
		t.Fatalf("read document %s: %v", artifactID, err)
	}
	if err := deleteArtifactRows(ctx, tx, artifact); err != nil {
		t.Fatal(err)
	}
}

// deleteArtifactRows runs, in tx, the statements removeArtifact runs to take an untouched
// document's rows away: its clears, its reference rows and the artifact row.
func deleteArtifactRows(ctx context.Context, tx pgx.Tx, artifact model.Artifact) error {
	for _, clear := range artifactRemovalClears(artifact) {
		if _, err := tx.Exec(ctx, clear.statement, clear.args...); err != nil {
			return fmt.Errorf("clear the %s of document %s: %w", clear.what, artifact.ID, err)
		}
	}
	if err := refs.ForgetDocument(ctx, tx, artifact.ID); err != nil {
		return fmt.Errorf("forget the references of document %s: %w", artifact.ID, err)
	}
	if _, err := tx.Exec(ctx, `delete from artifacts where id = $1`, artifact.ID); err != nil {
		return fmt.Errorf("delete document %s: %w", artifact.ID, err)
	}
	return nil
}

// requireProjectsComplete fails unless the project census names no row or node incomplete.
func requireProjectsComplete(t *testing.T, database *store.Store) {
	t.Helper()
	incomplete, err := graphcensus.ProjectComplete(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("read the incomplete rows: %v", err)
	}
	if len(incomplete) != 0 {
		t.Errorf("the project census names %q, want nothing", incomplete)
	}
}

// waitForLockWait waits until a session of database waits on a lock at a statement matching the
// LIKE pattern like, and fails the test after 10 seconds.
func waitForLockWait(t *testing.T, database *store.Store, like string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		waiting, err := storetest.CountLockWaits(context.Background(), database, like)
		if err != nil {
			t.Fatalf("read the lock waits: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no session waited on a lock at %q within 10s, so this test proves nothing", like)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
