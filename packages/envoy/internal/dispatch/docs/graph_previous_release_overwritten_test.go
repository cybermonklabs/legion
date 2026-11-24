package docs

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// A link the release before this one adds during a roll reaches the store even when this build
// writes the document's next version before any tick of this build reads the event of that
// release's version, and the census reaches zero with no second backfill.
//
// The release before this one writes a version whose index pass inserts the new link's row naming
// no node and states nothing in the store. This build then writes the next version of the same
// document, keeping that link, with the edit route's own write (api's editArtifact): the edit and
// the named version in one transaction, the version's artifact.version event, and what the
// version's index pass stated appended under that event (api's commitArtifactVersionEvent). That
// pass states the version and moves store_version to it, so this build's next tick, which reads
// the event of the previous release's version, finds the document's store_version equal to its
// version and marks nothing. A pass that named a link row the render keeps only on the drain of a
// mark would therefore leave the row the previous release wrote naming no node, and the census
// would refuse it until someone ran the backfill again, which nothing tells anyone to do. While
// the previous release holds every period of the block-index lease, this build ticks not at all,
// so the window is the whole roll. The tester drove the same sequence against
// legion-envoy-v32.11.1 beside this build on one database (census: `difference at block_ref ...:
// the row names no node`).
func TestALinkThePreviousReleaseAddsBeforeThisBuildsNextVersionReachesTheStore(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	appendIssueEvent(t, database)
	document := createOwedDocument(t, database, service, "CORE-1", "", "overwritten", nestedSpec("overwritten"), "")
	settle(t, database, document)
	// Rows of every other kind the census reads, as TestAVersionThePreviousReleaseWritesAfterTheDrainReachesTheStore
	// seeds them, so the census can pass only on agreement and never refuses an empty kind here.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-overwritten-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-overwritten-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	drainBlockIndex(t, service, "seed")
	takeTheStoreOff(t, database)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	drainBlockIndex(t, service, "the backfill's mark")
	requireCensusReadsZero(t, database, "after this build drained the backfill's mark, before any version of the previous release")
	awaitedBlockIndexTick(t, service)

	// A roll: a process of this build and one of the release before serve side by side.
	registerProcess(t, database, "")
	previousLeaves := registerProcess(t, database, previousReleaseCommit)

	// The release before this one moves the mention onto the middle paragraph: its index pass
	// inserts that link's row naming no node.
	added := moveLinkAsThePreviousRelease(t, database, service, document, "overwritten")
	if blocks, links := rowsNamingNoNode(t, database, document); links == 0 {
		t.Fatalf("the previous release's link row of %s names its node (%d block row(s) name none), so this test proves nothing", document, blocks)
	}

	// Before any tick of this build reads that version's event, this build writes the next
	// version of the document with the edit route's write, keeping the link.
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := service.ApplyOps(joined, document, []model.EditOp{{
		Op: "replace", Find: "Tail of overwritten.", With: "Tail of overwritten, edited through this build.",
	}}, model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
		t.Fatalf("edit %s through this build: %v", document, err)
	}
	// The edit route's named version (api's editArtifact), in the edit's own transaction.
	before := latestVersionNumber(t, service, document)
	written, err := service.NamedVersion(joined, document, "This build's version after the previous release's", model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatalf("write this build's version of %s: %v", document, err)
	}
	// The version's artifact.version event, and what the version's index pass stated appended
	// under it, in the same transaction, as the route's commitArtifactVersionEvent queues them.
	payload, err := json.Marshal(map[string]any{
		"artifact_id": document,
		"version":     map[string]any{"number": written.Version.Number},
	})
	if err != nil {
		t.Fatalf("encode the version event's payload: %v", err)
	}
	event := appendDocumentEvent(t, ctx, tx, "artifact.version", payload)
	if err := causeInWritePhase(joined, tx, written.Blocks, event); err != nil {
		t.Fatalf("append what this build's version of %s stated: %v", document, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit this build's version of %s: %v", document, err)
	}
	if after := latestVersionNumber(t, service, document); after == before {
		t.Fatalf("this build wrote no version of %s after the previous release's, so this test proves nothing", document)
	}
	if holder := holderOf(t, service, document, "dispatch://CORE-2"); holder != added.blockID {
		t.Fatalf("this build's version moved the mention to block %s, want it kept on %s: this test proves nothing", holder, added.blockID)
	}

	// This build's ticks while the previous release serves, then once it has left.
	awaitedBlockIndexTick(t, service)
	drainBlockIndex(t, service, "after the previous release's version and this build's next one")
	previousLeaves()
	awaitedBlockIndexTick(t, service)
	drainBlockIndex(t, service, "after the previous release left")

	if blocks, links := rowsNamingNoNode(t, database, document); blocks+links != 0 {
		t.Errorf("%d block row(s) and %d link row(s) of document %s name no node after this build's ticks, want none: the link the previous release added keeps naming no node",
			blocks, links, document)
	}
	var asserted bool
	if err := database.Pool.QueryRow(ctx, `
		select coalesce((select asserted from graph_tuples
			where from_kind = $1 and from_id like '%#' || $2 and kind = $3 and to_kind = $4 and to_id = $5
			order by seq desc limit 1), false)
	`, refs.BlockNodeKind, added.blockID, added.kind, added.toKind, added.toID).Scan(&asserted); err != nil || !asserted {
		t.Errorf("the store asserts no %s %s:%s out of block %s (%v), want the link the previous release added stated",
			added.kind, added.toKind, added.toID, added.blockID, err)
	}
	requireCensusReadsZero(t, database, "after this build wrote the version after the previous release's and ticked, with no second backfill")
}
