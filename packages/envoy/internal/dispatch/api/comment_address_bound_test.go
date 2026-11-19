package api

import (
	"context"
	crand "crypto/rand"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestACommentOwnedByALongRefKeyLeavesOutItsTuple is this change's R2 at the bound for a
// comment's relations. An artifact's ref_key is bounded only by its own key's index, at
// maxNewArtifactKeyBytes (2,692), which is wider than a tuple key holds beside the comment's own
// address: a comment owned by such a document would key a graph_tuples entry past the roughly 2,704
// bytes a btree entry holds. commentStored now leaves such a comment whole out of the store - no
// node, no tuples, its row's node columns null - so the write goes on and the old comments arm of
// graph_edges answers its owned_by edge, rather than the tuple arm answering it short that edge.
// The write leaves no node and no tuple end past the bound anywhere in the store either.
func TestACommentOwnedByALongRefKeyLeavesOutItsTuple(t *testing.T) {
	ctx := context.Background()
	_, database, _ := newTestServer(t, testServerOptions{})

	if _, err := database.Pool.Exec(ctx,
		`insert into projects (key, name, default_assignee) values ('CB', 'CB project', 'alice')`); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	// ref_key is `CB/<slug>`; the slug fills it to the artifact key bound, past the store's address
	// bound so the comment's owned_by far end cannot be keyed.
	slug := randText(maxNewArtifactKeyBytes - len("CB/"))
	var artifactID, refKey string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('CB', $1, $1, 'doc', '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`, slug).Scan(&artifactID, &refKey); err != nil {
		t.Fatalf("create the long-keyed document: %v", err)
	}
	if len(refKey) != maxNewArtifactKeyBytes {
		t.Fatalf("the document's ref_key is %d bytes, want the key bound %d", len(refKey), maxNewArtifactKeyBytes)
	}
	if graphstore.AddressFits(refKey) {
		t.Fatalf("the document's ref_key fits the store bound, so this proves nothing")
	}

	commentID := "0f9c2d2b-6a16-4b2f-9b2a-7c5a1a6d4f40"
	author := model.Actor{Kind: "user", ID: "alice"}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	stored, err := commentStored(ctx, tx, commentID, author, time.Now().UTC(), nil, &artifactID, "CB", nil)
	if err != nil {
		t.Fatalf("read what the comment states: %v", err)
	}
	if err := store.RunWrite(ctx, tx, stored.writeNode); err != nil {
		t.Fatalf("write the comment's node: %v", err)
	}
	nodeKind, nodeAddress := stored.rowNode()
	if _, err := tx.Exec(ctx, `
		insert into comments (id, artifact_id, author, body, node_kind, node_address)
		values ($1::uuid, $2::uuid, '{"kind":"user","id":"alice"}', 'A remark on a long-keyed document.', $3, $4)
	`, commentID, artifactID, nodeKind, nodeAddress); err != nil {
		t.Fatalf("insert the comment row: %v", err)
	}
	// On the old code the owned_by tuple went in here and failed with "index row size ... exceeds
	// btree ... 2704"; now the comment is left whole out of the store and the write commits.
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error { return stored.caused(b, graphstore.NoEvent) }); err != nil {
		t.Fatalf("state what the comment is: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	requireNoAddressPastTheBound(t, database)

	// The comment has no node, and its row names none, so the old comments arm of graph_edges
	// answers its owned_by edge once rather than the tuple arm answering it short the edge.
	if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindComment, commentID); err != nil || held {
		t.Fatalf("the store holds the comment's node %t (err %v), want none", held, err)
	}
	var nodeKindCol *string
	if err := database.Pool.QueryRow(ctx, `select node_kind from comments where id = $1::uuid`, commentID).Scan(&nodeKindCol); err != nil {
		t.Fatalf("read the comment row's node columns: %v", err)
	}
	if nodeKindCol != nil {
		t.Fatalf("the comment row names node %q, want null columns so the old arm answers it", *nodeKindCol)
	}
	requireNoFarEndTuple(t, database, graphstore.KindComment, commentID, graphstore.TupleOwnedBy)
	requireGraphEdge(t, database, graphstore.KindComment, commentID, graphstore.TupleOwnedBy, refKey)
}

// TestACommentWhoseAuthorIsASessionPastTheBoundLeavesOutItsNode is this change's R2 at
// the bound for a comment's author. A session id carries no length bound, so a comment whose author
// is a session past the store's address bound names an author the store cannot key: AuthorFacts
// writes no session node, no authored_by tuple and no alias, and the comment keeps its own row.
func TestACommentWhoseAuthorIsASessionPastTheBoundLeavesOutItsNode(t *testing.T) {
	ctx := context.Background()
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CA")
	issue := createSpecIssue(t, handler, "CA", "A paragraph of the spec.\n")

	pastSession := randText(3000) // incompressible and past the bound: its node key and authored_by tuple exceed the btree on old code
	author := model.Actor{Kind: "session", ID: pastSession}
	commentID := "0f9c2d2b-6a16-4b2f-9b2a-7c5a1a6d4f41"

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	stored, err := commentStored(ctx, tx, commentID, author, time.Now().UTC(), nil, nil, "CA", nil)
	if err != nil {
		t.Fatalf("read what the comment states: %v", err)
	}
	if err := store.RunWrite(ctx, tx, stored.writeNode); err != nil {
		t.Fatalf("write the comment's node: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into comments (id, issue_key, author, body, node_kind, node_address)
		values ($1::uuid, $2, $3, 'A remark by a very long session.', $4, $1)
	`, commentID, issue.Key, `{"kind":"session","id":"`+pastSession+`"}`, graphstore.KindComment); err != nil {
		t.Fatalf("insert the comment row: %v", err)
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error { return stored.caused(b, graphstore.NoEvent) }); err != nil {
		t.Fatalf("state what the comment is: %v", err)
	}
	// On the old code the session node's primary key and the authored_by tuple failed with "index
	// row size ... exceeds btree ... 2704"; now AuthorFacts leaves them out and the write commits.
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		return graphstore.QueueAuthored(b, graphstore.KindComment, commentID, author, time.Now().UTC(), graphstore.NoEvent)
	}); err != nil {
		t.Fatalf("record who wrote the comment: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The comment's node is written; the over-bound session is no node and no authored_by tuple.
	if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindComment, commentID); err != nil || !held {
		t.Fatalf("the store holds the comment's node %t (err %v), want it", held, err)
	}
	if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindSession, pastSession); err != nil || held {
		t.Fatalf("the store holds the over-bound session node %t (err %v), want none", held, err)
	}
	requireNoFarEndTuple(t, database, graphstore.KindComment, commentID, graphstore.TupleAuthoredBy)
}

// requireNoFarEndTuple fails unless the store holds no tuple of kind out of the node.
func requireNoFarEndTuple(t *testing.T, database *store.Store, fromKind, fromID, kind string) {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3
	`, fromKind, fromID, kind).Scan(&count); err != nil {
		t.Fatalf("count the %s tuples of %s %s: %v", kind, fromKind, fromID, err)
	}
	if count != 0 {
		t.Fatalf("%s %s states %d %s tuple(s), want none: the far end is past the bound", fromKind, fromID, count, kind)
	}
}

// requireGraphEdge fails unless graph_edges answers exactly one edge of kind out of the node to
// toID: a comment left out of the store keeps its edges through the old comments arm, which reads
// a row whose node columns are null.
func requireGraphEdge(t *testing.T, database *store.Store, fromKind, fromID, kind, toID string) {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_edges where from_kind = $1 and from_id = $2 and kind = $3 and to_id = $4
	`, fromKind, fromID, kind, toID).Scan(&count); err != nil {
		t.Fatalf("count the %s edges of %s %s: %v", kind, fromKind, fromID, err)
	}
	if count != 1 {
		t.Fatalf("graph_edges answers %d %s edge(s) of %s %s, want exactly one from the old arm", count, kind, fromKind, fromID)
	}
}

// requireNoAddressPastTheBound reads graph_nodes and graph_tuples whole for an address past the
// store's bound: a node whose id is, or a tuple with either end that is. The reads above ask
// after the comment's own node and tuples by name, and a write that kept the comment out but
// keyed something else by the long ref_key would pass every one of them. A value is no address,
// so a value's to_id is not counted.
func requireNoAddressPastTheBound(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	var longNodes, longTuples int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_nodes where octet_length(id) > $1`,
		graphstore.MaxAddressBytes).Scan(&longNodes); err != nil {
		t.Fatalf("count the nodes past the bound: %v", err)
	}
	if longNodes != 0 {
		t.Errorf("the store holds %d node(s) whose id is past the %d-byte bound, want none", longNodes, graphstore.MaxAddressBytes)
	}
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples
		where octet_length(from_id) > $1 or (to_kind <> 'value' and octet_length(to_id) > $1)
	`, graphstore.MaxAddressBytes).Scan(&longTuples); err != nil {
		t.Fatalf("count the tuples with an end past the bound: %v", err)
	}
	if longTuples != 0 {
		t.Errorf("the store holds %d tuple(s) with an end past the %d-byte bound, want none", longTuples, graphstore.MaxAddressBytes)
	}
}

// randText is an incompressible string of n lower-case alphanumeric bytes: a btree compresses a
// run of one byte, so a control needs a key the index cannot shrink under its bound.
func randText(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
