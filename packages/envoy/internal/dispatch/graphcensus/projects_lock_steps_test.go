package graphcensus

import (
	"context"
	"encoding/json"
	"io"
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

// zedDocumentID is the project document the object batch's test seeds beside the two issue
// documents (coreDocumentID, acmeDocumentID). No issue owns it, so its owner is the document
// itself, whose owner key "artifact:<id>" sorts before every issue's, while its address,
// ZED/notes, sorts after both issue documents' addresses.
const zedDocumentID = "00000000-0000-4000-8000-0000000000d3"

// requireTheObjectBatchLockOrder holds writeObjectBatch to its steps over documents, the kind a
// writer deletes rows of (objectKind.owners). The batch first takes the owner lock of each of its
// documents (lockObjectOwners), each once and in ascending owner key (events.LockOwnerRows), and
// reads under them which rows still stand and the nodes the store holds of them. It then writes
// its nodes in runs of consecutive nodes of one cause, each run queued through
// graphstore.QueueNodes, which takes the run's node rows in ascending (kind, id), and
// graphstore.Caused, which appends the run's tuples under that cause. It writes no projection row
// and appends no event.
//
// Three documents are seeded as a build before the node writers left them, rows with no node: an
// issue document of ACME-1 with its own artifact.created event, an issue document of CORE-1 with
// none (an issue's spec, which issue.created records) and a document of project ZED that no issue
// owns. The batch reads them in address order, ACME-1/spec, CORE-1/spec, ZED/notes, so it writes
// ACME-1/spec's node under its event and then the other two at the sentinel, one run of two
// nodes. Their owner keys sort the other way round: artifact:<ZED/notes's id> before issue:ACME-1
// and issue:CORE-1, so a batch that took each document's owner lock in the order it read them
// would take issue ACME-1's first. ACME-1/spec's state tuple carries its event, which gives it a
// repeat key a hold can stand on. The issues' nodes are written before the run, so the object
// pass has nothing of the issue kind to write and the run the test follows writes the documents
// alone.
func requireTheObjectBatchLockOrder(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	acme := seedIssueDocument(t, database, "ACME", acmeDocumentID, "Written before the store existed.")
	core := seedIssueDocument(t, database, "CORE", coreDocumentID, "Written before the store existed.")
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('ZED', 'Zed', 'alice')`); err != nil {
		t.Fatalf("seed project ZED: %v", err)
	}
	zed := preStoreDocument{artifactID: zedDocumentID}
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (id, project_key, slug, name, kind, is_primary, created_by)
		values ($1::uuid, 'ZED', 'notes', 'notes.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning ref_key
	`, zed.artifactID).Scan(&zed.refKey); err != nil {
		t.Fatalf("create project ZED's document: %v", err)
	}
	now := time.Now()
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.IssueNode("ACME-1", "ACME", alice, now), graphstore.IssueNode("CORE-1", "CORE", alice, now),
		}, nil)
	}); err != nil {
		t.Fatalf("write the issues' nodes: %v", err)
	}
	acmeCreated := insertIssueEvent(t, database, "ACME-1", 1, "artifact.created",
		map[string]any{"artifact": map[string]any{"id": acme.artifactID}})

	ownerUnlocked := []string{"graph_nodes", "graph_tuples"}
	ownerHold := func(name string, owner store.Owner) *lockstep.Hold {
		return lockstep.HoldOwner(t, database, &lockstep.Hold{
			Name: name, Step: lockstep.StepOwner, Taken: true,
			Statement: lockstep.OwnerLockStatement, Unlocked: ownerUnlocked,
		}, owner)
	}
	issueOwner := func(issue string) *lockstep.Hold {
		return ownerHold("issue "+issue+"'s owner lock", store.IssueOwner(issue))
	}
	zedOwner := ownerHold(zed.refKey+"'s owner lock", store.ArtifactOwner(zed.artifactID))
	// A node the batch makes is held as the same key inserted by a transaction still open, which is
	// what its insert waits on; the hold's own base attributes are at the sentinel, which no repeat
	// key covers, so the hold holds the node's row and nothing else.
	nodeHold := func(document preStoreDocument, project string) *lockstep.Hold {
		return holdWrite(t, database, &lockstep.Hold{
			Name: document.refKey + "'s node row", Step: lockstep.StepNode, Taken: true,
			Kind: graphstore.KindArtifact, ID: document.refKey, Statement: "insert into graph_nodes (",
		}, []graphstore.Node{graphstore.ArtifactNode(document.refKey, project, "", backfillActor, now)}, nil)
	}
	state := holdWrite(t, database, &lockstep.Hold{
		Name: acme.refKey + "'s state tuple", Step: lockstep.StepTuple, Taken: true,
		Statement: "with written as (",
	}, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindArtifact, FromID: acme.refKey, Kind: graphstore.AttributeState,
		ToKind: graphstore.KindValue, ToID: graphstore.StateLive,
		At: now, Who: alice, SourceEvent: acmeCreated, Asserted: true,
	}})
	project := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project CORE's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for update`, "CORE")

	report := followTheBackfill(t, database, "writeObjectBatch", []*lockstep.Hold{
		issueOwner("ACME-1"), zedOwner, issueOwner("CORE-1"),
		nodeHold(acme, "ACME"), state, nodeHold(core, "CORE"), nodeHold(zed, "ZED"), project,
	})

	for _, want := range []struct {
		document preStoreDocument
		project  string
		cause    int64
	}{{acme, "ACME", acmeCreated}, {core, "CORE", graphstore.NoEvent}, {zed, "ZED", graphstore.NoEvent}} {
		var nodeState string
		var nodeProject *string
		var cause, stated int64
		if err := database.Pool.QueryRow(ctx, `
			select n.state, n.project_key, n.source_event,
			       (select count(*) from graph_tuples t
			        where t.from_kind = n.kind and t.from_id = n.id and t.kind = $3 and t.to_kind = $4
			          and t.to_id = n.state and t.source_event = n.source_event and t.asserted)
			from graph_nodes n where n.kind = $1 and n.id = $2
		`, graphstore.KindArtifact, want.document.refKey, graphstore.AttributeState, graphstore.KindValue).Scan(
			&nodeState, &nodeProject, &cause, &stated); err != nil {
			t.Fatalf("read %s's node back: %v", want.document.refKey, err)
		}
		if nodeState != graphstore.StateLive || nodeProject == nil || *nodeProject != want.project || cause != want.cause || stated != 1 {
			t.Errorf("%s's node is %s in project %v under event %d with %d state tuple(s) under it, want live in %s under %d with one: the run the test followed is not one that committed",
				want.document.refKey, nodeState, nodeProject, cause, stated, want.project, want.cause)
		}
	}
	if done := reportedKind(t, report, graphstore.KindArtifact); done.Rows != 3 || done.Nodes != 3 || done.Tuples != 9 || !done.Complete {
		t.Errorf("the backfill reports %#v for documents, want 3 rows, 3 nodes, 9 tuples (visibility, state and project of each) and the cutover complete", done)
	}
}

// requireTheBlockScopeBatchLockOrder holds writeScopeBatch over block nodes to its steps. A block's
// node is written under the revision fence, so the fill takes each document's owner lock and then
// its document_block_versions row first (blockDocuments, then lockBlockDocuments), the order the
// block batch takes them in (TestTheBlockBatchTakesEveryOwnerRowBeforeAnyFenceRow). Only then does
// graphstore.FillScopes take the node rows, in ascending (kind, id) and in a statement of their
// own, give each its project_key, and append each one's project tuple, under the node's own
// cause. It names no node on a document_blocks row and appends no event.
//
// Two block nodes of one issue document are seeded as #3178's block writer left them: live, under
// the artifact.version event of the version that wrote them, naming no project and no parties,
// with the document_blocks rows that name them. The issue's and the document's nodes are written
// by a backfill run before the blocks exist, as the object pass writes them, so the run the test
// follows takes no owner lock for them; its one batch that does work is this fill. The first
// block's project tuple carries the version's event, which gives it a repeat key a hold can stand
// on.
//
// The comment nodes the same call site fills are held in a run of their own
// (requireTheCommentScopeBatchLockOrder): the comment fill runs before the block fill, and in one
// run its node row would be taken before the block fill's owner lock, which the owner-first rule
// reads as one writer taking a node row before an owner lock.
func requireTheBlockScopeBatchLockOrder(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document := seedIssueDocument(t, database, "CORE", coreDocumentID, "First.\n\nSecond.\n")
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the issue and its document before their blocks exist: %v", err)
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	made := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	versioned := insertIssueEvent(t, database, "CORE-1", 1, "artifact.version",
		map[string]any{"artifact_id": document.artifactID, "version": map[string]any{"number": 1}})
	blockIDs := []string{"first", "second"}
	addresses := make([]string, len(blockIDs))
	nodes := make([]graphstore.Node, len(blockIDs))
	memberships := make([]graphstore.Tuple, len(blockIDs))
	for index, blockID := range blockIDs {
		addresses[index] = document.refKey + "#" + blockID
		nodes[index] = graphstore.Node{
			Kind: refs.BlockNodeKind, ID: addresses[index], Supertag: refs.BlockSupertag,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			MadeAt: made, MadeBy: alice, SourceEvent: versioned,
		}
		memberships[index] = graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: addresses[index], Kind: refs.TupleInDocument,
			ToKind: graphstore.KindArtifact, ToID: document.refKey,
			At: made, Who: alice, SourceEvent: versioned, Asserted: true,
		}
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := graphstore.Write(ctx, tx, nodes, memberships); err != nil {
			return err
		}
		for index, blockID := range blockIDs {
			if _, err := tx.Exec(ctx, `
				insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at, node_kind, node_address)
				values ($1::uuid, $2, $3, 'paragraph', '', $4, 1, $5, $6, $3)
			`, document.artifactID, blockID, addresses[index], "Block "+blockID+".", made, refs.BlockNodeKind); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the block nodes naming no project and the rows that name them: %v", err)
	}

	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue CORE-1's owner lock", Step: lockstep.StepOwner, Taken: true,
		Statement: lockstep.OwnerLockStatement,
		Unlocked:  []string{"document_block_versions", "graph_nodes", "graph_tuples"},
	}, store.IssueOwner("CORE-1"))
	fence := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the document's document_block_versions row", Step: lockstep.StepFence, Taken: true,
		Kind: "document", ID: document.artifactID,
		Statement: "select 1 from document_block_versions where artifact_id = $1 for no key update",
		Unlocked:  []string{"graph_nodes", "graph_tuples"},
	}, `select 1 from document_block_versions where artifact_id = $1::uuid for update`, document.artifactID)
	// The fill locks node rows that are already there `for no key update`, so each is held at that
	// level, which conflicts with it and with no foreign key's `for key share`.
	nodeHold := func(address string) *lockstep.Hold {
		return lockstep.HoldRow(t, database, &lockstep.Hold{
			Name: address + "'s node row", Step: lockstep.StepNode, Taken: true, Kind: refs.BlockNodeKind, ID: address,
			Statement: "select 1 from graph_nodes held",
		}, `select 1 from graph_nodes where kind = $1 and id = $2 for no key update`, refs.BlockNodeKind, address)
	}
	tuple := holdWrite(t, database, &lockstep.Hold{
		Name: addresses[0] + "'s project tuple", Step: lockstep.StepTuple, Taken: true,
		Statement: "insert into graph_tuples (",
	}, nil, []graphstore.Tuple{{
		FromKind: refs.BlockNodeKind, FromID: addresses[0], Kind: graphstore.AttributeProject,
		ToKind: graphstore.KindValue, ToID: "CORE",
		At: made, Who: alice, SourceEvent: versioned, Asserted: true,
	}})
	row := lockstep.HoldRow(t, database, &lockstep.Hold{Name: addresses[0] + "'s document_blocks row", Step: lockstep.StepProjection},
		`select 1 from document_blocks where artifact_id = $1::uuid and block_id = $2 for no key update`, document.artifactID, blockIDs[0])
	project := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project CORE's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for update`, "CORE")

	report := followTheBackfill(t, database, "writeScopeBatch",
		[]*lockstep.Hold{owner, fence, nodeHold(addresses[1]), nodeHold(addresses[0]), tuple, row, project})

	for _, address := range addresses {
		requireScopeFilledUnder(t, database, refs.BlockNodeKind, address, "CORE", versioned)
	}
	if done := reportedKind(t, report, refs.BlockNodeKind+"_project"); done.Rows != 2 || done.Nodes != 2 || done.Tuples != 2 || !done.Complete {
		t.Errorf("the backfill reports %#v for the block scopes, want 2 rows, 2 nodes, 2 tuples and every block complete", done)
	}
}

// requireTheCommentScopeBatchLockOrder holds writeScopeBatch over comment nodes to its steps. A
// comment's node is written under no fence, so the fill takes no owner lock and no
// document_block_versions row: graphstore.FillScopes takes the node rows, gives each its
// project_key, and appends each one's project tuple, under the node's own cause. It names no node
// on a comments row and appends no event.
//
// One comment node is seeded as #3178's comment writer left it: live, under its comment.created
// event, naming no project and no parties, with the comments row that names it. The issue's node
// is written by a backfill run before the comment exists, so the run the test follows writes
// nothing but the fill. The comment's issue row, its project's row and its comments row are held
// throughout, and the fill must queue behind none of them.
func requireTheCommentScopeBatchLockOrder(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('SCOP', 'Scopes', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('SCOP-1', 'SCOP', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue SCOP-1: %v", err)
	}
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the issue before its comment exists: %v", err)
	}
	const comment = "22222222-2222-4222-8222-000000000001"
	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Date(2026, 11, 2, 7, 30, 0, 0, time.UTC)
	created := insertIssueEvent(t, database, "SCOP-1", 1, "comment.created", map[string]any{"id": comment})
	node, facts := graphstore.CommentFacts(nil, comment, alice, at, created, "", "", "", nil)
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, facts); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into comments (id, issue_key, author, body, created_at, node_kind, node_address)
			values ($1::uuid, 'SCOP-1', '{"kind":"user","id":"alice"}', 'On the issue.', $2, $3, $1)
		`, comment, at, graphstore.KindComment)
		return err
	}); err != nil {
		t.Fatalf("seed the comment node naming no project and the row that names it: %v", err)
	}

	owner := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "issue SCOP-1's row", Step: lockstep.StepOwner},
		`select 1 from issues where key = $1 for update`, "SCOP-1")
	project := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project SCOP's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for update`, "SCOP")
	row := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "the comment's comments row", Step: lockstep.StepProjection},
		`select 1 from comments where id = $1::uuid for no key update`, comment)
	nodeRow := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the comment's node row", Step: lockstep.StepNode, Taken: true, Kind: graphstore.KindComment, ID: comment,
		Statement: "select 1 from graph_nodes held",
	}, `select 1 from graph_nodes where kind = $1 and id = $2 for no key update`, graphstore.KindComment, comment)
	tuple := holdWrite(t, database, &lockstep.Hold{
		Name: "the comment's project tuple", Step: lockstep.StepTuple, Taken: true,
		Statement: "insert into graph_tuples (",
	}, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindComment, FromID: comment, Kind: graphstore.AttributeProject,
		ToKind: graphstore.KindValue, ToID: "SCOP",
		At: at, Who: alice, SourceEvent: created, Asserted: true,
	}})

	report := followTheBackfill(t, database, "writeScopeBatch", []*lockstep.Hold{owner, project, row, nodeRow, tuple})

	requireScopeFilledUnder(t, database, graphstore.KindComment, comment, "SCOP", created)
	if done := reportedKind(t, report, graphstore.KindComment+"_project"); done.Rows != 1 || done.Nodes != 1 || done.Tuples != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for the comment scopes, want 1 row, 1 node, 1 tuple and every comment complete", done)
	}
}

// requireScopeFilledUnder fails unless node kind/id names project in its column and holds one
// project tuple naming it under cause, so the fill the test followed is the one that committed.
func requireScopeFilledUnder(t *testing.T, database *store.Store, kind, id, project string, cause int64) {
	t.Helper()
	var named *string
	var stated int64
	if err := database.Pool.QueryRow(context.Background(), `
		select n.project_key,
		       (select count(*) from graph_tuples t
		        where t.from_kind = n.kind and t.from_id = n.id and t.kind = $3 and t.to_kind = $4
		          and t.to_id = $5 and t.source_event = $6 and t.asserted)
		from graph_nodes n where n.kind = $1 and n.id = $2
	`, kind, id, graphstore.AttributeProject, graphstore.KindValue, project, cause).Scan(&named, &stated); err != nil {
		t.Fatalf("read node %s/%s back: %v", kind, id, err)
	}
	if named == nil || *named != project || stated != 1 {
		t.Errorf("node %s/%s names project %v and holds %d project tuple(s) naming %s under event %d, want %s and one: the run the test followed is not one that committed",
			kind, id, named, stated, project, cause, project)
	}
}

// insertIssueEvent appends, at seq of issue, an event of eventType carrying payload, as a build
// before the node writers recorded it, and answers the event's id.
func insertIssueEvent(t *testing.T, database *store.Store, issue string, seq int, eventType string, payload map[string]any) int64 {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode the payload of %s's %s event: %v", issue, eventType, err)
	}
	var event int64
	if err := database.Pool.QueryRow(context.Background(), `
		insert into events (issue_key, seq, type, actor, payload, notify)
		values ($1, $2, $3, '{"kind":"user","id":"alice"}', $4, false)
		returning id
	`, issue, seq, eventType, encoded).Scan(&event); err != nil {
		t.Fatalf("append %s's %s event: %v", issue, eventType, err)
	}
	return event
}
