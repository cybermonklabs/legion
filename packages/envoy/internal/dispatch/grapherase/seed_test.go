package grapherase

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// dryRun runs Erase over kind:id without --confirm, which reads the listing in a read-only
// transaction and writes nothing, fails the test on an error, and waits until that run's
// connection is gone from pg_stat_activity, so a test that reads the backend of the confirmed
// run after it finds that run's and no other.
func dryRun(t *testing.T, database *store.Store, kind, id string) *Erasure {
	t.Helper()
	ctx := context.Background()
	erasure, err := Erase(ctx, database.Pool.Config().ConnString(), kind, id, "", 0)
	if err != nil {
		t.Fatalf("erase --node %s:%s without --confirm: %v", kind, id, err)
	}
	if erasure.Applied || erasure.Digest == "" {
		t.Fatalf("erase --node %s:%s without --confirm answered applied %t and digest %q, want a listing and nothing removed",
			kind, id, erasure.Applied, erasure.Digest)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var backends int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from pg_stat_activity
			where application_name = $1 and datname = current_database() and pid <> pg_backend_pid()`,
			eraseApplicationName).Scan(&backends); err != nil {
			t.Fatalf("read the erase's backends: %v", err)
		}
		if backends == 0 {
			return erasure
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run without --confirm left %d backend(s) open for 30 s", backends)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// confirmedErase is what an operator runs: Erase over kind:id without --confirm, then with the
// digest that run printed.
func confirmedErase(t *testing.T, database *store.Store, kind, id string, bound time.Duration) (*Erasure, error) {
	t.Helper()
	listed := dryRun(t, database, kind, id)
	return Erase(context.Background(), database.Pool.Config().ConnString(), kind, id, listed.Digest, bound)
}

// listed reports whether a line of erasure's listing holds every one of parts.
func listed(erasure *Erasure, parts ...string) bool {
	for _, line := range erasure.Listing {
		found := true
		for _, part := range parts {
			if !strings.Contains(line, part) {
				found = false
				break
			}
		}
		if found {
			return true
		}
	}
	return false
}

// commentNodeKind is what the node and tuple store calls a comment (migration 20261109051232), the
// kind of the node most of this package's tests erase.
const commentNodeKind = "comment"

// seedWriter is who seedAgreeingStore states the blocks' parents by: the backfill's own writer
// (graphcensus's backfillActor), which writes every other fact of the seed.
var seedWriter = model.Actor{Kind: "system", ID: "dispatch-backfill-graph-store"}

// preStoreDocument is one document as a build before the node and tuple store left it: its id,
// its address, and one document_blocks row per block that names no node.
type preStoreDocument struct {
	artifactID, refKey string
}

// seedPreStoreDocument writes issue CORE-1 with one document at version 1 and a document_blocks
// row for each of blockIDs, each naming no node, which is what the backfill reads. The version
// carries markdown, so migration 0122's trigger gives the document its document_block_versions
// row, as a version gives one in production.
func seedPreStoreDocument(t *testing.T, database *store.Store, blockIDs ...string) preStoreDocument {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue CORE-1: %v", err)
	}
	var document preStoreDocument
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`).Scan(&document.artifactID, &document.refKey); err != nil {
		t.Fatalf("create the document: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, 'Written before the store existed.', '[{"kind":"user","id":"alice"}]')
	`, document.artifactID); err != nil {
		t.Fatalf("write version 1 of the document: %v", err)
	}
	for _, blockID := range blockIDs {
		if _, err := database.Pool.Exec(ctx, `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			values ($1::uuid, $2, $3, 'paragraph', '', $4, 1, now())
		`, document.artifactID, blockID, document.refKey+"#"+blockID, "Block "+blockID+"."); err != nil {
			t.Fatalf("write block %s of the document: %v", blockID, err)
		}
	}
	var owed bool
	if err := database.Pool.QueryRow(ctx, `
		select exists (select 1 from document_block_versions where artifact_id = $1)
	`, document.artifactID).Scan(&owed); err != nil || !owed {
		t.Fatalf("the document has no document_block_versions row to hold (err %v), so this test would hold nothing", err)
	}
	return document
}

// agreeingStore is a database whose store and projections agree on every kind the store census
// reads rows of: one document of two blocks, the inner one inside the outer one, one block
// reference out of the inner block, and one comment that the document owns and is anchored to,
// written by a person. outer and inner are the two blocks' node addresses, and comment the
// comment's id. cite is the address of a third block the document holds directly, which cites
// the inner block, where the seed was asked for one (seedCitingStore), and empty otherwise.
type agreeingStore struct {
	document                    preStoreDocument
	outer, inner, cite, comment string
}

// seedAgreeingStore writes the rows as a build before the store existed wrote them, runs the
// backfill over them (graphcensus.Backfill), and states each block's parent with the tuples the
// block index plans (refs' planBlockParents) and the column it projects them into
// (queueBlockParents): the outer block's part_of to the document, the inner block's to the outer
// block, and the inner row's parent_block_id beside it. It is graphcensus's own seed of the same
// name, but for the census graphcensus takes after it, which is that package's to hold.
func seedAgreeingStore(t *testing.T, database *store.Store) agreeingStore {
	t.Helper()
	return seedStore(t, database, false)
}

// seedCitingStore is seedAgreeingStore with a third block, `cite`, which the document holds
// directly and whose block reference names the inner block, so the backfill states a mentions
// tuple from it into the inner block's node.
func seedCitingStore(t *testing.T, database *store.Store) agreeingStore {
	t.Helper()
	return seedStore(t, database, true)
}

func seedStore(t *testing.T, database *store.Store, cite bool) agreeingStore {
	t.Helper()
	ctx := context.Background()
	blocks := []string{"outer", "inner"}
	if cite {
		blocks = append(blocks, "cite")
	}
	document := seedPreStoreDocument(t, database, blocks...)
	seeded := agreeingStore{
		document: document,
		outer:    document.refKey + "#outer",
		inner:    document.refKey + "#inner",
	}
	if cite {
		seeded.cite = document.refKey + "#cite"
		if _, err := database.Pool.Exec(ctx, `
			insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
			values ($1::uuid, 'cite', $2, $3, $4, 'mentions', 1)
		`, document.artifactID, seeded.cite, refs.BlockNodeKind, seeded.inner); err != nil {
			t.Fatalf("write the citing block's reference to the inner block: %v", err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
		values ($1::uuid, 'inner', $2, 'issue', 'CORE-1', 'mentions', 1)
	`, document.artifactID, seeded.inner); err != nil {
		t.Fatalf("write the inner block's reference to CORE-1: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (artifact_id, author, body, anchor)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'A remark on the spec.', jsonb_build_object('artifact_id', $1::text))
		returning id::text
	`, document.artifactID).Scan(&seeded.comment); err != nil {
		t.Fatalf("write the comment on the document: %v", err)
	}
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the parents' write: %v", err)
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	parent := func(address, toKind, toID string) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: address, Kind: graphstore.TuplePartOf,
			ToKind: toKind, ToID: toID, At: now, Who: seedWriter, SourceEvent: graphstore.NoEvent, Asserted: true,
		}
	}
	parents := []graphstore.Tuple{
		parent(seeded.outer, "artifact", document.refKey),
		parent(seeded.inner, refs.BlockNodeKind, seeded.outer),
	}
	if cite {
		parents = append(parents, parent(seeded.cite, "artifact", document.refKey))
	}
	if err := graphstore.Write(ctx, tx, nil, parents); err != nil {
		t.Fatalf("state the blocks' parents: %v", err)
	}
	if tag, err := tx.Exec(ctx, `
		update document_blocks set parent_block_id = 'outer' where artifact_id = $1::uuid and block_id = 'inner'
	`, document.artifactID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("project the inner block's parent: %d rows, %v", tag.RowsAffected(), err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the parents' write: %v", err)
	}
	return seeded
}

// The two documents the owner-order test seeds, one on each of two issues, whose ids sort the
// other way round from their owner keys: issue:ACME-1 sorts before issue:CORE-1, and CORE-1's
// document's id before ACME-1's.
const (
	coreDocumentID = "00000000-0000-4000-8000-0000000000d1"
	acmeDocumentID = "00000000-0000-4000-8000-0000000000d2"
)

// seedIssueDocument writes project, its issue <project>-1, and one document of that issue under id
// at version 1 holding markdown. The version carries markdown, so migration 0122's trigger gives
// the document its document_block_versions row, as a version gives one in production.
func seedIssueDocument(t *testing.T, database *store.Store, project, id, markdown string) preStoreDocument {
	t.Helper()
	ctx := context.Background()
	issue := project + "-1"
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ($1, $1, 'alice')
	`, project); err != nil {
		t.Fatalf("seed project %s: %v", project, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, 'One', '{"kind":"user","id":"alice"}', 'U')
	`, issue, project); err != nil {
		t.Fatalf("seed issue %s: %v", issue, err)
	}
	document := preStoreDocument{artifactID: id}
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1::uuid, $2, $3, 'spec', 'spec.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning ref_key
	`, id, issue, project).Scan(&document.refKey); err != nil {
		t.Fatalf("create %s's document: %v", issue, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 1, $2, '[{"kind":"user","id":"alice"}]')
	`, id, markdown); err != nil {
		t.Fatalf("write version 1 of %s: %v", document.refKey, err)
	}
	return document
}

// seedPreStoreBlock writes block blockID of document as a document_blocks row naming no node,
// which is what the backfill reads.
func seedPreStoreBlock(t *testing.T, database *store.Store, document preStoreDocument, blockID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, $2, $3, 'paragraph', '', $4, 1, now())
	`, document.artifactID, blockID, document.refKey+"#"+blockID, "Block "+blockID+"."); err != nil {
		t.Fatalf("write block %s of %s: %v", blockID, document.refKey, err)
	}
}
