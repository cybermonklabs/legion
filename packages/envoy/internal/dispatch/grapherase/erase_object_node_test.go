package grapherase

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// An issue, a document, an ask and a message each hold their text in their own row: the issue's
// title, the document's name and versions, the ask's question, the message's body. The erase
// reads text only from the comments and the document blocks that derive from a node, so a run
// over one of these nodes would delete the node and its tuples, record an erasure and answer
// applied, and leave the row and its text where they were; the project census would then read the
// row as missing a node, and the backfill would write the node again. The erase refuses each of
// the four kinds while its row stands: run as an operator runs it, without --confirm and then
// with a digest, each run answers the refusal, and the node, its tuples and its row with its text
// are all still there, with no erasure recorded.
//
// Its control is the refusal taken out of Erasure.read: the dry run then lists the node, the
// confirmed run over that listing answers applied, and the row's text is still in place.
func TestTheEraseRefusesAnIssueADocumentAnAskAndAMessageWhileTheirRowStands(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedObjectNodes(t, database)
	url := database.Pool.Config().ConnString()

	const refusal = "its row stands and holds the node's text, which the erase does not reach"
	for _, node := range []struct{ kind, id, table, column, key string }{
		{graphstore.KindIssue, seeded.issue, "issues", "title", "key"},
		{graphstore.KindArtifact, seeded.documentRef, "artifacts", "name", "ref_key"},
		{graphstore.KindAsk, seeded.ask, "asks", "question", "id::text"},
		{graphstore.KindMessage, seeded.message, "messages", "body", "id::text"},
	} {
		t.Run(node.kind, func(t *testing.T) {
			before := readTypeNode(t, database, node.kind, node.id)
			if !before.row || before.out == 0 {
				t.Fatalf("the seed holds %s:%s as %+v, want a node with tuples, so this test would prove nothing", node.kind, node.id, before)
			}
			text := rowText(t, database, node.table, node.column, node.key, node.id)
			listing, err := Erase(ctx, url, node.kind, node.id, "", 0)
			if err == nil && listing != nil {
				// The refusal is not there: run the confirmed erase an operator runs next, so the
				// failure says what that erase does to the row.
				confirmed, confirmErr := Erase(ctx, url, node.kind, node.id, listing.Digest, 0)
				t.Fatalf("erase --node %s:%s listed it and its confirmed run answered applied %t (err %v); the %s row still reads %q",
					node.kind, node.id, confirmed != nil && confirmed.Applied, confirmErr, node.table,
					rowText(t, database, node.table, node.column, node.key, node.id))
			}
			for _, confirm := range []string{"", "any digest"} {
				erasure, err := Erase(ctx, url, node.kind, node.id, confirm, 0)
				if err == nil || !strings.Contains(err.Error(), "refuses "+node.kind+":"+node.id+": ") || !strings.Contains(err.Error(), refusal) {
					t.Errorf("erase --node %s:%s (confirm %q) answered %v (applied %t), want the refusal %q",
						node.kind, node.id, confirm, err, erasure != nil && erasure.Applied, refusal)
				}
			}
			if after := readTypeNode(t, database, node.kind, node.id); after != before {
				t.Errorf("erase --node %s:%s left the store holding %+v of it, want %+v as before the runs", node.kind, node.id, after, before)
			}
			if after := rowText(t, database, node.table, node.column, node.key, node.id); after != text {
				t.Errorf("erase --node %s:%s left its %s row reading %q, want %q as before the runs", node.kind, node.id, node.table, after, text)
			}
		})
	}
}

// A build before this one writes no node, so a document it removes during a roll or after a
// rollback leaves the live node this build wrote for it, with no row behind it: the project
// census reads it as orphaned, and no backfill reads a node no row stands behind. The erase is
// that node's remedy: with no row standing, it lists the node and its tuples, removes them on the
// confirmed run, and records the erasure, and the census then reads no orphaned node.
//
// Its control is the refusal made regardless of the row, as Erase made it before a connection
// once: the orphaned node then has no remedy, and the census keeps reading it.
func TestTheEraseRemovesAnOrphanedDocumentsNode(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedObjectNodes(t, database)
	url := database.Pool.Config().ConnString()
	// The removal a build before this one makes: the row goes, and nothing states its node gone.
	if _, err := database.Pool.Exec(ctx, `delete from artifacts where ref_key = $1`, seeded.documentRef); err != nil {
		t.Fatalf("remove the document's row: %v", err)
	}
	requireOrphaned(t, database, 1)

	listing, err := Erase(ctx, url, graphstore.KindArtifact, seeded.documentRef, "", 0)
	if err != nil {
		t.Fatalf("erase --node artifact:%s with no row behind it answered %v, want its listing", seeded.documentRef, err)
	}
	if want := "delete graph_nodes artifact:" + seeded.documentRef; !strings.Contains(strings.Join(listing.Listing, "\n"), want) {
		t.Errorf("the listing reads %q, want it to name %q", listing.Listing, want)
	}
	erased, err := Erase(ctx, url, graphstore.KindArtifact, seeded.documentRef, listing.Digest, 0)
	if err != nil || !erased.Applied {
		t.Fatalf("the confirmed erase answered %+v, %v; want applied", erased, err)
	}
	if held := readTypeNode(t, database, graphstore.KindArtifact, seeded.documentRef); held.row || held.out != 0 || held.in != 0 || held.erased != 1 {
		t.Errorf("after the erase the store holds %+v of the document's node, want no row, no tuple and one erasure", held)
	}
	requireOrphaned(t, database, 0)
	// The issue's node, whose row stands, is untouched.
	if held := readTypeNode(t, database, graphstore.KindIssue, seeded.issue); !held.row || held.erased != 0 {
		t.Errorf("after the document's erase the store holds %+v of the issue's node, want it as seeded", held)
	}
}

// A create of this build writes a document's node before its row, in one transaction, so it holds
// the node's row from its node write until it commits the row. The erase reads whether the row
// stands under its own lock of the node (lockOwnersThenNode, then read), so an erase that queues
// behind such a create reads the row the create committed, and refuses. Here a build before this
// one has removed a document, leaving its node live behind no row, and a create of this build
// takes the address again: it has written the node and the row and not committed when the
// confirmed erase starts, the erase queues behind it on the node, and the create commits. The
// erase answers the refusal, and the node, its tuples and the row all stand, with no erasure
// recorded and nothing for the project census to name.
//
// Its control is the row's check moved out of read to just before the node's `for update` in
// lockOwnersThenNode: the erase then reads no row, queues, and erases the node the created row
// stands behind, which the census reads as a row with no node.
func TestTheErasesRowCheckUnderTheNodeLockRefusesTheCreateItWaitedFor(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedObjectNodes(t, database)
	if _, err := database.Pool.Exec(ctx, `delete from artifacts where ref_key = $1`, seeded.documentRef); err != nil {
		t.Fatalf("remove the document's row as a build before this one does: %v", err)
	}
	before := readTypeNode(t, database, graphstore.KindArtifact, seeded.documentRef)
	create := &lockstep.Hold{Name: "a create of " + seeded.documentRef + " that wrote its node and its row"}
	lockstep.Begin(t, database, create)
	created := graphstore.PlanUncaused([]graphstore.Node{graphstore.ArtifactNode(
		seeded.documentRef, "ERASE", "", model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC())}, nil)
	if err := store.RunWrite(ctx, create.Tx, func(b *store.WriteBatch) error {
		graphstore.QueueNodes(b, created)
		return nil
	}); err != nil {
		t.Fatalf("write the created document's node: %v", err)
	}
	if _, err := create.Tx.Exec(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('ERASE-1', 'ERASE', 'notes-md', 'Notes made again', 'doc', '{"kind":"user","id":"alice"}')
	`); err != nil {
		t.Fatalf("write the created document's row: %v", err)
	}
	pid, erased := startErase(t, database, graphstore.KindArtifact, seeded.documentRef)
	if early, done := storetest.WaitForQueuedBehind(t, create.Tx, pid, erased, lockstep.Wait); done {
		t.Fatalf("the erase answered (%+v, %v) without waiting on the node the create holds, so this test proves nothing", early.erasure, early.err)
	}
	t.Logf("the erase waits behind %s at %q", create.Name, lockstep.WaitingStatement(t, database, pid))
	if err := create.Tx.Commit(ctx); err != nil {
		t.Fatalf("commit the create: %v", err)
	}
	create.Release()
	var answer eraseAnswer
	select {
	case answer = <-erased:
	case <-time.After(lockstep.Wait):
		t.Fatalf("the erase did not answer within %s of the create's commit", lockstep.Wait)
	}
	if answer.err == nil || !strings.Contains(answer.err.Error(), "refuses artifact:"+seeded.documentRef+": its row stands") {
		t.Errorf("the erase queued behind the create answered %+v, %v; want the refusal while the row stands",
			answer.erasure, answer.err)
	}
	if after := readTypeNode(t, database, graphstore.KindArtifact, seeded.documentRef); after != before {
		t.Errorf("after the create and the erase the store holds %+v of the document's node, want %+v as the create left it", after, before)
	}
	if name := rowText(t, database, "artifacts", "name", "ref_key", seeded.documentRef); name != "Notes made again" {
		t.Errorf("the created document's row reads %q, want the create's name", name)
	}
	incomplete, err := graphcensus.ProjectComplete(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the incomplete rows: %v", err)
	}
	if len(incomplete) != 0 {
		t.Errorf("after the create and the erase the project census names %q, want nothing", incomplete)
	}
}

// requireOrphaned fails unless the project census reads want orphaned nodes, all of them
// documents, and no other finding.
func requireOrphaned(t *testing.T, database *store.Store, want int64) {
	t.Helper()
	census, err := graphcensus.ProjectCensus(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("take the project census: %v", err)
	}
	for _, kind := range census {
		orphaned := int64(0)
		if kind.Kind == graphstore.KindArtifact {
			orphaned = want
		}
		if kind.Orphaned != orphaned || kind.Findings() != orphaned {
			t.Errorf("the project census reads %+v for %s, want orphaned=%d and no other finding", kind, kind.Kind, orphaned)
		}
	}
}

// objectNodes is what seedObjectNodes wrote: an issue, its document, an ask on the issue and a
// message on it, each a row and its node in the project.
type objectNodes struct {
	issue, documentRef, ask, message string
}

// seedObjectNodes writes project ERASE, issue ERASE-1 with a document, an ask and a message, each
// row with its node as its create writes it (graphstore.IssueNode and its siblings) and the base
// attributes that node write states.
func seedObjectNodes(t *testing.T, database *store.Store) objectNodes {
	t.Helper()
	ctx := context.Background()
	seeded := objectNodes{
		issue: "ERASE-1", documentRef: "ERASE-1/notes-md",
		ask: "22222222-2222-4222-8222-000000000001", message: "22222222-2222-4222-8222-000000000002",
	}
	now := time.Now().UTC()
	alice := model.Actor{Kind: "user", ID: "alice"}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the seed: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, []graphstore.Node{
		graphstore.IssueNode(seeded.issue, "ERASE", alice, now),
		graphstore.ArtifactNode(seeded.documentRef, "ERASE", "", alice, now),
		graphstore.AskNode(seeded.ask, "ERASE", nil, alice, now),
		graphstore.MessageNode(seeded.message, "ERASE", nil, alice, now),
	}, nil); err != nil {
		t.Fatalf("write the nodes: %v", err)
	}
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('ERASE', 'Erase', 'alice')`,
		`insert into issues (key, project_key, number, title, created_by, rank)
		 values ('ERASE-1', 'ERASE', 1, 'A title somebody wrote', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		 values ('ERASE-1', 'ERASE', 'notes-md', 'Notes somebody named', 'doc', '{"kind":"user","id":"alice"}')`,
		`insert into asks (id, issue_key, author, question, options, multiple, urgency, kind)
		 values ('` + seeded.ask + `', 'ERASE-1', '{"kind":"user","id":"alice"}', 'A question somebody asked', '[]', false, 'med', 'question')`,
		`insert into messages (id, issue_key, author, body)
		 values ('` + seeded.message + `', 'ERASE-1', '{"kind":"user","id":"alice"}', 'A message somebody wrote')`,
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the rows: %v\n%s", err, statement)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the seed: %v", err)
	}
	return seeded
}

// rowText reads column of the table's row whose key reads id.
func rowText(t *testing.T, database *store.Store, table, column, key, id string) string {
	t.Helper()
	var text string
	if err := database.Pool.QueryRow(context.Background(),
		`select `+column+` from `+table+` where `+key+` = $1`, id).Scan(&text); err != nil {
		t.Fatalf("read %s.%s of %s: %v", table, column, id, err)
	}
	return text
}
