package api

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// An issue's tree artifact (R5 of the issue-tree spec, D3 and D14 of its plan) is created in its
// caller's transaction as an artifacts row of kind tree, with a slug of its own among the issue's
// documents (nextArtifactSlug), its node and that node's part_of to the issue written before the
// row that names it, the issue's one issue_trees row, and its version 1, empty, with that
// version's node (number, created_at, part_of the tree), which the create queues on its ledger
// (planConvertedIssueRoom) and its commit runs after the row; a second tree of the same issue is
// refused by that row's key.
func TestAnIssuesTreeArtifactTakesItsSlugItsNodeAndItsIssueTreesRow(t *testing.T) {
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	// The issue is created as before the issue room, holding no tree yet: an issue born converted
	// holds one from its create, and this test makes the issue's first.
	holdIssuesUnconverted(t, database)
	key := createTestIssue(t, handler, "TREE", "An issue with a tree")
	alice := model.Actor{Kind: "user", ID: "alice"}
	created := func() (string, error) {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		s := &server{deps: deps}
		var id string
		if err := tx.QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&id); err != nil {
			t.Fatalf("draw the tree's id: %v", err)
		}
		slug, err := s.nextArtifactSlug(ctx, tx, artifactTarget{IssueKey: &key}, issueTreeName)
		if err != nil {
			t.Fatalf("take the tree's slug: %v", err)
		}
		written := issueTreeWrite(slug, key, "TREE", alice, time.Now().UTC())
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			return s.createIssueTree(b, written, id, slug, key, "TREE", alice)
		}); err != nil {
			return "", err
		}
		joined, ledger := deps.Docs.Join(ctx, tx)
		defer ledger.Discard()
		version, err := ledger.QueueVersion(joined, id, "", []model.Actor{alice}, nil)
		if err != nil {
			t.Fatalf("queue the tree's version 1: %v", err)
		}
		versionNode, err := docs.WriteVersionNode(joined, tx, graphstore.VersionOwner{
			RefKey: graphstore.ArtifactRefKey(key, "TREE", "", slug), Project: "TREE",
		}, version.Number, alice, version.CreatedAt)
		if err != nil {
			t.Fatalf("queue the node of the tree's version 1: %v", err)
		}
		// What the two node writes stated is appended after them in the same write phase, as the
		// create queues it; the ledger's commit runs that phase and commits tx.
		if err := ledger.Queue(func(b *store.WriteBatch) error {
			graphstore.QueueCaused(b, graphstore.Joined(written, versionNode), graphstore.NoEvent)
			return nil
		}); err != nil {
			t.Fatalf("queue what the tree's node writes stated: %v", err)
		}
		return id, ledger.Commit(ctx)
	}
	id, err := created()
	if err != nil {
		t.Fatalf("create the issue's tree: %v", err)
	}

	var kind, slug, refKey, nodeKind, nodeAddress, held string
	if err := database.Pool.QueryRow(ctx, `
		select a.kind, a.slug, a.ref_key, a.node_kind, a.node_address, t.artifact_id::text
		from artifacts a join issue_trees t on t.issue_key = a.issue_key
		where a.id = $1`, id).Scan(&kind, &slug, &refKey, &nodeKind, &nodeAddress, &held); err != nil {
		t.Fatalf("read the tree artifact: %v", err)
	}
	if kind != "tree" || slug != issueTreeName || held != id {
		t.Errorf("the tree artifact is of kind %q with slug %q and the issue's tree is %s, want kind tree, slug %q and %s", kind, slug, held, issueTreeName, id)
	}
	if nodeKind != graphstore.KindArtifact || nodeAddress != refKey {
		t.Errorf("the tree artifact names the node %s %s, want %s %s", nodeKind, nodeAddress, graphstore.KindArtifact, refKey)
	}
	var partOf int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5 and asserted`,
		graphstore.KindArtifact, refKey, graphstore.TuplePartOf, graphstore.KindIssue, key).Scan(&partOf); err != nil {
		t.Fatalf("read the tree's part_of: %v", err)
	}
	if partOf != 1 {
		t.Errorf("the tree's node states %d part_of tuples to issue %s, want one", partOf, key)
	}
	// The tree's version 1, empty, as an empty document's create writes its version 1, with its
	// version node: live, numbered 1, created when the row was, and part_of the tree's node.
	var markdown string
	var createdAt time.Time
	if err := database.Pool.QueryRow(ctx, `
		select markdown, created_at from artifact_versions where artifact_id = $1 and number = 1`, id).Scan(&markdown, &createdAt); err != nil {
		t.Fatalf("read the tree's version 1: %v", err)
	}
	if markdown != "" {
		t.Errorf("the tree's version 1 holds %q, want it empty", markdown)
	}
	var versions int
	if err := database.Pool.QueryRow(ctx, `select count(*) from artifact_versions where artifact_id = $1`, id).Scan(&versions); err != nil {
		t.Fatalf("count the tree's versions: %v", err)
	}
	if versions != 1 {
		t.Errorf("the tree holds %d versions, want its version 1 alone", versions)
	}
	version := graphstore.VersionID(refKey, 1)
	node, nodeHeld, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindVersion, version)
	if err != nil || !nodeHeld || node.State != graphstore.StateLive {
		t.Fatalf("the tree's version 1 has node %+v (held %t, %v), want it live", node, nodeHeld, err)
	}
	if number, stated, err := graphstore.CurrentValue(ctx, database.Pool, graphstore.KindVersion, version, graphstore.AttributeNumber); err != nil || !stated || number != "1" {
		t.Errorf("%s states number %q (stated %t, %v), want 1", version, number, stated, err)
	}
	if stated, held, err := graphstore.CurrentValue(ctx, database.Pool, graphstore.KindVersion, version, graphstore.AttributeCreatedAt); err != nil || !held {
		t.Errorf("%s states no created_at (held %t, %v)", version, held, err)
	} else if value, err := time.Parse(time.RFC3339Nano, stated); err != nil || !value.Equal(createdAt) {
		t.Errorf("%s states created_at %q (%v), want the row's %s", version, stated, err, createdAt)
	}
	var versionPartOf int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5 and asserted`,
		graphstore.KindVersion, version, graphstore.TuplePartOf, graphstore.KindArtifact, refKey).Scan(&versionPartOf); err != nil {
		t.Fatalf("read the tree version's part_of: %v", err)
	}
	if versionPartOf != 1 {
		t.Errorf("the tree's version 1 states %d part_of tuples to the tree %s, want one", versionPartOf, refKey)
	}
	if _, err := created(); err == nil {
		t.Errorf("a second tree of issue %s was created, want it refused by the issue_trees row", key)
	}
}
