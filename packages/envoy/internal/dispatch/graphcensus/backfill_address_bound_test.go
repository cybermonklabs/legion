package graphcensus

import (
	"context"
	crand "crypto/rand"
	"io"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheBackfillLeavesOutAnOwnedByTuplePastTheBound is dispatch://INFRA-1149's R2 at the bound for
// the backfill. A pre-store comment owned by a document whose ref_key is past the store's address
// bound is left whole out of the store: the backfill writes no node and no tuple for it and leaves
// its row's node columns null, so the old comments arm of graph_edges answers its owned_by edge.
// On the old code the owned_by tuple failed the batch with "index row size ... exceeds btree ...
// 2704". The backfill leaves no node and no tuple end past the bound anywhere in the store either.
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
	requireNoAddressPastTheBound(t, database)

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

// TestTheCensusHoldsNoAuthorOfACommentPastTheBound is the same bound on the census's two sides
// that have no projection table. A comment owned by a document whose ref_key is past the
// store's address bound is left whole out of the store, by the writer (api's commentStored) and
// by the backfill alike, so it states no authored_by tuple and makes no node of its author. A
// person whose only comment is such a one, and a session that wrote only such a one, are
// therefore no nodes of the store, and the census may hold the store to neither: counting them
// on the row side left a difference nothing could ever close. The census counts the comments
// themselves past the bound instead, as a class of their own and no difference.
func TestTheCensusHoldsNoAuthorOfACommentPastTheBound(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	seedAgreeingStore(t, database)

	if _, err := database.Pool.Exec(ctx,
		`insert into projects (key, name, default_assignee) values ('LKY', 'Long keys', 'alice')`); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	// ref_key LKY/<slug> is 2,692 incompressible bytes: past the store's bound and inside the
	// btree bound artifacts_ref_key_key holds its own rows to.
	slug := randText(2688)
	var artifactID, refKey string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('LKY', $1, $1, 'doc', '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`, slug).Scan(&artifactID, &refKey); err != nil {
		t.Fatalf("create the long-keyed document: %v", err)
	}
	if graphstore.AddressFits(refKey) {
		t.Fatalf("the document's ref_key fits the store bound, so this proves nothing")
	}

	// Two comments on that document, each by a writer no other row of this database names: a
	// person, and a session. Both rows are written as the comment path leaves one it cannot
	// key - no node, and null node columns - and the backfill passes over them too.
	const longKeyPerson, longKeySession = "longkey-person", "longkey-session"
	for _, author := range []string{
		`{"kind":"user","id":"` + longKeyPerson + `"}`,
		`{"kind":"session","id":"` + longKeySession + `"}`,
	} {
		if _, err := database.Pool.Exec(ctx, `
			insert into comments (artifact_id, author, body)
			values ($1::uuid, $2::jsonb, 'A remark on a long-keyed document.')
		`, artifactID, author); err != nil {
			t.Fatalf("write the comment by %s: %v", author, err)
		}
	}
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}

	for _, held := range []struct{ kind, id string }{
		{graphstore.KindPerson, longKeyPerson},
		{graphstore.KindSession, longKeySession},
	} {
		if _, got, err := graphstore.ReadNode(ctx, database.Pool, held.kind, held.id); err != nil || got {
			t.Fatalf("the store holds a %s node for %s: %t (err %v), want none: their only comment is past the bound",
				held.kind, held.id, got, err)
		}
	}
	report := storeCensusOf(t, database)
	requireTheCensusAgrees(t, "the census over a comment past the bound", report)
	// Zero differences alone would pass a census that never counted the two comments at all.
	// They are left out of the store on purpose, so the census must count them past the bound,
	// as a class of their own, and not pass over them in silence.
	pastBound := censusKind(t, report, "comment").PastBound
	if pastBound < 2 {
		t.Fatalf("the census counts %d comment(s) past the bound, want at least the two on the long-keyed document", pastBound)
	}
	t.Logf("the census counts %d comment(s) past the bound", pastBound)
}

// requireNoAddressPastTheBound reads graph_nodes and graph_tuples whole for an address past the
// store's bound: a node whose id is, or a tuple with either end that is. The reads around it ask
// after the comment's own node and tuples by name, and a backfill that kept the comment out but
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
