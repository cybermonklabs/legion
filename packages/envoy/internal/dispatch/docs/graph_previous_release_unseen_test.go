package docs

import (
	"context"
	"io"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
)

// A version the release before this one writes while it serves beside this build reaches the
// store even when no tick of this build runs while that release serves.
//
// The two builds share one block-index lease (BlockIndexLease), and each period goes to the
// process whose tick asks for it first. A process of the release before whose ticks fall earlier
// in each 30 s period than this build's takes every period for as long as it serves, so this
// build ticks before that process starts and next after it has left, and never while it serves.
// That tick finds this build serving alone, after a tick that found it alone too, and is not its
// process's first, so it reads no event (markVersionsAnotherBuildWrote) and moves the cursor to
// the safe horizon, past the version events that release wrote. Their documents hold no mark,
// their rows name no node, and the census refuses until someone runs the backfill again.
//
// The tester drove exactly that on 2026-11-18 at 460793b3ef29 with legion-envoy-v31.2.0 and this
// build on one database: this build ticked alone, the previous release joined timed so its ticks
// came first in each period and took every period while it served, it edited a document, created
// one and created an issue, and it left; this build's next ticks read `events=0 marked=0
// another_build=false`, and `census --store` refused at every row it had written.
//
// This test is TestAVersionThePreviousReleaseWritesAfterTheDrainReachesTheStore's roll with the
// tick taken out from between the previous release's writes and its leaving: the tick this build
// does not get while that release holds the lease.
func TestAVersionThePreviousReleaseWritesWhileItHoldsTheLeaseReachesTheStore(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	appendIssueEvent(t, database)
	const slug = "unseen"
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	// Rows of every other kind the census reads, so the census can pass only on agreement.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-unseen-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-unseen-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	drainBlockIndex(t, service, "seed")
	takeTheStoreOff(t, database)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	drainBlockIndex(t, service, "the backfill's mark")
	requireCensusReadsZero(t, database, "after this build drained the backfill's mark")

	// This build serving alone: its tick reads no event, and it has joined the log.
	if tick := awaitedBlockIndexTick(t, service); tick.AnotherBuild || tick.Events != 0 {
		t.Fatalf("the tick with this build serving alone read %d event(s) and found another build %t, want neither", tick.Events, tick.AnotherBuild)
	}

	// A process of the release before joins and takes every period of the lease: it edits the
	// document, creates one and creates an issue, and leaves, and no tick of this build runs
	// meanwhile.
	registerProcess(t, database, "")
	previousLeaves := registerProcess(t, database, previousReleaseCommit)
	added := moveLinkAsThePreviousRelease(t, database, service, document, slug)
	created, createdLink, _ := createAsThePreviousRelease(t, database, service, "created-unseen", added)
	spec, specLink, _ := createIssueAsThePreviousRelease(t, database, service, "CORE-3", 3, added)
	for _, written := range []string{document, created, spec} {
		if blocks, links := rowsNamingNoNode(t, database, written); blocks+links == 0 {
			t.Fatalf("every row the previous release wrote of document %s names its node, so this test proves nothing", written)
		}
	}
	previousLeaves()

	// This build's next ticks, serving alone again.
	tick := awaitedBlockIndexTick(t, service)
	t.Logf("the first tick after the previous release left read %d event(s), marked %d document(s), indexed %d, another build %t",
		tick.Events, tick.Marked, tick.Documents, tick.AnotherBuild)
	drainBlockIndex(t, service, "after the previous release left")

	for _, written := range []string{document, created, spec} {
		if blocks, links := rowsNamingNoNode(t, database, written); blocks+links != 0 {
			t.Errorf("document %s holds %d block row(s) and %d link row(s) naming no node after this build's ticks, want none: no tick of this build ran while the previous release served",
				written, blocks, links)
		}
	}
	requireVersionInTheStore(t, database, document, added)
	requireVersionInTheStore(t, database, created, createdLink)
	requireVersionInTheStore(t, database, spec, specLink)
	requireCensusReadsZero(t, database, "after the previous release, which held the block-index lease while it served, left, with no second backfill")
}

// A document the release before this one creates after this build's first boot and before this
// build's first tick reaches the store, where no build of this code has run before.
//
// R13 starts the cursor at this build's first boot. The code starts it at the build's first tick
// that reads a proven horizon (readEventsAfterCursor), at that horizon where no build has
// left a row, which is every first deploy of this code. A process of this build boots during a
// roll while the release before holds the shared block-index lease, so its first tick comes only
// once that release has left, and everything that release wrote since this build booted is below
// the horizon that tick starts the cursor at. The backfill, run during that roll, marks only the
// documents whose rows it names, so a document that release creates after the backfill holds no
// mark, its rows name no node, and the census refuses until someone runs the backfill again.
func TestADocumentThePreviousReleaseCreatesBeforeThisBuildsFirstTickReachesTheStore(t *testing.T) {
	database, seeder := blockIndexService(t)
	ctx := context.Background()

	appendIssueEvent(t, database)
	const slug = "first"
	document := createOwedDocument(t, database, seeder, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-first-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-first-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	drainBlockIndex(t, seeder, "seed")
	takeTheStoreOff(t, database)
	// The seed stands for a build before this code, which keeps no event cursor: no build of this
	// code has run on this database.
	if _, err := database.Pool.Exec(ctx, `delete from block_index_event_cursors`); err != nil {
		t.Fatalf("take the seed's event cursor off: %v", err)
	}

	// The roll: this build's process boots while the release before serves and holds the lease.
	const firstBuild = "3333333333333333333333333333333333333333"
	booted := blockIndexProcess(t, database, firstBuild)
	registerProcess(t, database, firstBuild)
	previousLeaves := registerProcess(t, database, previousReleaseCommit)

	// The backfill runs during the roll; the release before then edits the backfilled document
	// and creates one.
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	added := moveLinkAsThePreviousRelease(t, database, seeder, document, slug)
	created, createdLink, _ := createAsThePreviousRelease(t, database, seeder, "created-first", added)
	if blocks, links := rowsNamingNoNode(t, database, created); blocks+links == 0 {
		t.Fatalf("every row of the document the previous release created names its node, so this test proves nothing")
	}
	previousLeaves()

	// This build's first tick, after the release before has left, and its drain.
	tick := awaitedBlockIndexTick(t, booted)
	t.Logf("this build's first tick read %d event(s), marked %d document(s), indexed %d, another build %t",
		tick.Events, tick.Marked, tick.Documents, tick.AnotherBuild)
	drainBlockIndex(t, booted, "after the previous release left")

	if blocks, links := rowsNamingNoNode(t, database, created); blocks+links != 0 {
		t.Errorf("the document the previous release created after this build booted holds %d block row(s) and %d link row(s) naming no node after this build's first ticks, want none",
			blocks, links)
	}
	requireVersionInTheStore(t, database, document, added)
	requireVersionInTheStore(t, database, created, createdLink)
	requireCensusReadsZero(t, database, "after this build's first ticks, with no second backfill")
}
