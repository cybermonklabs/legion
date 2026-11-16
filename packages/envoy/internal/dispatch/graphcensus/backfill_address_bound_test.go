package graphcensus

import (
	"context"
	crand "crypto/rand"
	"io"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheBackfillLeavesOutAnOwnedByTuplePastTheBound is dispatch://INFRA-1149's R2 at the bound for
// the backfill. A pre-store comment owned by a document whose ref_key is past the store's address
// bound is left whole out of the store: the backfill writes no node and no tuple for it and leaves
// its row's node columns null, so the old comments arm of graph_edges answers its owned_by edge.
// On the old code the owned_by tuple failed the batch with "index row size ... exceeds btree ...
// 2704".
func TestTheBackfillLeavesOutAnOwnedByTuplePastTheBound(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)

	if _, err := database.Pool.Exec(ctx,
		`insert into projects (key, name, default_assignee) values ('CBF', 'CBF project', 'alice')`); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	slug := randText(2688) // ref_key CBF/<slug> is 2,692 incompressible bytes, so the owned_by tuple keys past the btree on old code
	var artifactID, refKey string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('CBF', $1, $1, 'doc', '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`, slug).Scan(&artifactID, &refKey); err != nil {
		t.Fatalf("create the long-keyed document: %v", err)
	}
	if graphstore.AddressFits(refKey) {
		t.Fatalf("the document's ref_key fits the store bound, so this proves nothing")
	}

	// A comment written before the store existed names no node (node_kind null).
	var commentID string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (artifact_id, author, body)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'A remark on a long-keyed document.')
		returning id::text
	`, artifactID).Scan(&commentID); err != nil {
		t.Fatalf("write the pre-store comment: %v", err)
	}

	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}

	// The comment is left whole out of the store: no node, its row names none, and the old comments
	// arm of graph_edges answers its owned_by edge once.
	if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindComment, commentID); err != nil || held {
		t.Fatalf("the store holds the comment's node %t (err %v), want none", held, err)
	}
	var nodeKind *string
	if err := database.Pool.QueryRow(ctx, `select node_kind from comments where id = $1::uuid`, commentID).Scan(&nodeKind); err != nil {
		t.Fatalf("read the comment row's node columns: %v", err)
	}
	if nodeKind != nil {
		t.Fatalf("the backfill stamped the comment row with node %q, want null columns so the old arm answers it", *nodeKind)
	}
	var owned int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3
	`, graphstore.KindComment, commentID, graphstore.TupleOwnedBy).Scan(&owned); err != nil {
		t.Fatalf("count the comment's owned_by tuples: %v", err)
	}
	if owned != 0 {
		t.Fatalf("the comment states %d owned_by tuple(s), want none: the far end is past the bound", owned)
	}
	var edges int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_edges where from_kind = $1 and from_id = $2 and kind = $3 and to_id = $4
	`, graphstore.KindComment, commentID, graphstore.TupleOwnedBy, refKey).Scan(&edges); err != nil {
		t.Fatalf("count the comment's owned_by edges: %v", err)
	}
	if edges != 1 {
		t.Fatalf("graph_edges answers %d owned_by edge(s) of the comment, want exactly one from the old arm", edges)
	}
}

// randText is an incompressible string of n lower-case alphanumeric bytes: a btree compresses a
// run of one byte, so a control needs an address the index cannot shrink under its bound.
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
