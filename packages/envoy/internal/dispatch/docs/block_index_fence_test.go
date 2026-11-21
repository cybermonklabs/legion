package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// fenceWait is how long a step of this test waits for another backend to queue behind a row it
// holds, and for a pass it started on a connection of its own to answer.
const fenceWait = 30 * time.Second

// highestTuple is the last sequence the tuple log holds: the mark a step's own appends are read
// back after, and the one that says a step appended nothing.
func highestTuple(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var highest int64
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(max(seq), 0) from graph_tuples`).Scan(&highest); err != nil {
		t.Fatalf("read the end of the tuple log: %v", err)
	}
	return highest
}

// tuplesSince counts what the log gained after seq out of the blocks of one document, whose
// addresses all start with the document's ref_key: a tick writes more than one document, so a
// count of the whole log would answer for passes this step is not watching.
func tuplesSince(t *testing.T, database *store.Store, artifactID string, seq int64) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples t
		join artifacts a on a.id = $1::uuid
		where t.seq > $2 and t.from_kind = $3 and t.from_id like a.ref_key || '#%'
	`, artifactID, seq, refs.BlockNodeKind).Scan(&count); err != nil {
		t.Fatalf("count the tuples a step appended for %s: %v", artifactID, err)
	}
	return count
}

// parentsNow is the far end the store asserts under part_of for each block of the document, by
// block id, read as a current relation is read (graphstore.CurrentOut).
func parentsNow(t *testing.T, database *store.Store, artifactID string) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx,
		`select block_id, node_id from document_blocks where artifact_id = $1 order by block_id`, artifactID)
	if err != nil {
		t.Fatalf("read the blocks of %s: %v", artifactID, err)
	}
	defer rows.Close()
	addresses := map[string]string{}
	for rows.Next() {
		var id, address string
		if err := rows.Scan(&id, &address); err != nil {
			t.Fatalf("scan a block of %s: %v", artifactID, err)
		}
		addresses[id] = address
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks of %s: %v", artifactID, err)
	}
	parents := map[string]string{}
	for id, address := range addresses {
		held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, address, []string{graphstore.TuplePartOf})
		if err != nil {
			t.Fatalf("read the parent of %s: %v", id, err)
		}
		if len(held) != 1 {
			t.Fatalf("block %s holds %d current part_of tuples, want exactly one", id, len(held))
		}
		parents[id] = held[0].ToKind + "/" + held[0].ToID
	}
	return parents
}

// sameParents compares two readings of a document's parents.
func sameParents(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for block, parent := range want {
		if got[block] != parent {
			return false
		}
	}
	return true
}

// flattenedPass is the blocks an indexing pass of the document's stored version would hand the
// index if every block of it sat at the document's top level: one byte of the markdown each, so
// no block's range holds another's. It is what the stale pass below would assert, and what the
// fence must refuse.
func flattenedPass(t *testing.T, database *store.Store, artifactID string) (string, []model.ArtifactBlock) {
	t.Helper()
	ctx := context.Background()
	var markdown string
	if err := database.Pool.QueryRow(ctx, `
		select markdown from artifact_versions where artifact_id = $1 and markdown is not null
		order by number desc limit 1
	`, artifactID).Scan(&markdown); err != nil {
		t.Fatalf("read the stored version of %s: %v", artifactID, err)
	}
	rows, err := database.Pool.Query(ctx,
		`select block_id from document_blocks where artifact_id = $1 order by block_id`, artifactID)
	if err != nil {
		t.Fatalf("read the blocks of %s: %v", artifactID, err)
	}
	defer rows.Close()
	var blocks []model.ArtifactBlock
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan a block of %s: %v", artifactID, err)
		}
		at := len(blocks)
		blocks = append(blocks, model.ArtifactBlock{ID: id, Type: "paragraph", From: at, To: at + 1})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks of %s: %v", artifactID, err)
	}
	if len(blocks) < 2 || len(markdown) < len(blocks) {
		t.Fatalf("the fixture document holds %d blocks in %d bytes, want a document of several blocks", len(blocks), len(markdown))
	}
	return markdown, blocks
}

// writeVersionThroughTheRealPath writes the document's next version the way a request writes
// one, in a transaction of its own (writeVersion). It answers the version and its event, the
// cause every tuple the version's index stated carries.
func writeVersionThroughTheRealPath(t *testing.T, service *Service, artifactID, summary string) (VersionResult, int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the version write: %v", err)
	}
	defer tx.Rollback(ctx)
	result, event, err := writeVersion(ctx, service, tx, artifactID, nil, summary)
	if err != nil {
		t.Fatal(err)
	}
	return result, event
}

// writeVersion is the version write a request runs, in tx: NamedVersion in a transaction joined
// to the document service, after ApplyOps of ops where there are any, as api's editArtifact runs
// an edit that names its version (createNamedVersion runs NamedVersion alone); the version's own
// event after it; and what the block index stated appended under that event, as api's
// commitArtifactVersionEvent does; then the commit. The routes take the owner's lock before those
// calls (requireOpenOwner, requireOpenOwnerStatus); writeVersion takes none, so the owner's lock
// its transaction holds is the one the document service's calls take themselves: ApplyOps'
// joinLiveWrite, or with no ops joinRead under NamedVersion's capture, and NamedVersion's own. The
// event is appended at once (documentEvent), where the route queues it for its write phase. It
// answers the version and that event, and an error rather than failing the test, so a write run
// off the test's goroutine can write a version too.
func writeVersion(ctx context.Context, service *Service, tx pgx.Tx, artifactID string, ops []model.EditOp, summary string) (VersionResult, int64, error) {
	alice := model.Actor{Kind: "user", ID: "alice"}
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if len(ops) > 0 {
		if _, err := service.ApplyOps(joined, artifactID, ops, alice, nil); err != nil {
			return VersionResult{}, 0, fmt.Errorf("apply the edit the version names: %w", err)
		}
	}
	result, err := service.NamedVersion(joined, artifactID, summary, alice)
	if err != nil {
		return VersionResult{}, 0, fmt.Errorf("write a version through the version write path: %w", err)
	}
	if !result.Wrote {
		return VersionResult{}, 0, errors.New("the version write recorded nothing; the live document must differ from the latest version")
	}
	payload, err := json.Marshal(map[string]any{
		"artifact_id": artifactID,
		"version":     map[string]any{"number": result.Version.Number},
	})
	if err != nil {
		return VersionResult{}, 0, fmt.Errorf("encode the version event's payload: %w", err)
	}
	event, err := documentEvent(ctx, tx, "artifact.version", payload)
	if err != nil {
		return VersionResult{}, 0, err
	}
	if err := causeInWritePhase(joined, tx, result.Blocks, event); err != nil {
		return VersionResult{}, 0, fmt.Errorf("append what the version's block index stated: %w", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		return VersionResult{}, 0, fmt.Errorf("commit the version write: %w", err)
	}
	return result, event, nil
}

// appendDocumentEvent records one event of issue CORE-1, where the fixture's documents live, in
// the write's own transaction (documentEvent), and fails the test where it cannot.
func appendDocumentEvent(t *testing.T, ctx context.Context, tx pgx.Tx, kind string, payload []byte) int64 {
	t.Helper()
	event, err := documentEvent(ctx, tx, kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

// documentEvent records one event of issue CORE-1 by alice in the write's own transaction
// (recordIssueEvent). It answers an error rather than failing the test, so a write run off the
// test's goroutine can record its event too.
func documentEvent(ctx context.Context, tx pgx.Tx, kind string, payload []byte) (int64, error) {
	return recordIssueEvent(ctx, tx, "CORE-1", kind, aliceActor, payload)
}

// aliceActor is alice, the fixture's person, as an event row stores its actor.
var aliceActor = []byte(`{"kind":"user","id":"alice"}`)

// recordIssueEvent records one event of issue key, of kind, by actor and with payload, each JSON,
// through q: the write's own transaction, or the pool for an event that is a write of its own.
// The sequence comes off the issue's own counter, as events.Append takes it, so the issue's next
// event does not collide with this one. It answers the event's id.
func recordIssueEvent(ctx context.Context, q Queryer, key, kind string, actor, payload []byte) (int64, error) {
	var event int64
	if err := q.QueryRow(ctx, `
		with bumped as (update issues set last_seq = last_seq + 1 where key = $1 returning last_seq)
		insert into events (issue_key, seq, type, actor, payload, notify)
		select $1, bumped.last_seq, $2, $3, $4, false
		from bumped
		returning id
	`, key, kind, actor, payload).Scan(&event); err != nil {
		return 0, fmt.Errorf("record the %s event of %s: %w", kind, key, err)
	}
	return event, nil
}

// versionEventPayload is the payload of the artifact.version event an older build or the release
// before this one records for version number of document artifactID, named name: the document,
// its name and the version's number.
func versionEventPayload(t *testing.T, artifactID, name string, number int) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"artifact_id": artifactID, "name": name, "version": map[string]any{"number": number},
	})
	if err != nil {
		t.Fatalf("encode the payload of version %d of %s: %v", number, artifactID, err)
	}
	return payload
}

// The fixture document's two versions. Version 1 holds two callouts side by side; version 2 moves
// the second, mover, under the first, holder, so mover's parent is the document at version 1 and
// holder at version 2: the parent a stale pass of version 1 would assert again, and the one the
// fence must keep it from asserting.
const (
	raceVersion1 = ":::callout{#holder kind=\"note\" title=\"\"}\nThe block a newer version moves another under.\n:::\n\n" +
		":::callout{#mover kind=\"note\" title=\"\"}\nThe block the newer version moves.\n:::\n"
	raceVersion2 = "::::callout{#holder kind=\"note\" title=\"\"}\nThe block a newer version moves another under.\n\n" +
		":::callout{#mover kind=\"note\" title=\"\"}\nThe block the newer version moves.\n:::\n\n::::\n"
)

// standsText is a document's stored revision as a failure prints it: the version its blocks
// stand at, or owed where the row holds none.
func standsText(stands *int) string {
	if stands == nil {
		return "owed (null)"
	}
	return fmt.Sprintf("version %d", *stands)
}

// blockAddress is the node address of one block of the document, read off its row, which fails
// the test where the document holds no block of that id.
func blockAddress(t *testing.T, database *store.Store, artifactID, blockID string) string {
	t.Helper()
	var address string
	if err := database.Pool.QueryRow(context.Background(),
		`select node_id from document_blocks where artifact_id = $1 and block_id = $2`, artifactID, blockID).Scan(&address); err != nil {
		t.Fatalf("read the address of block %s of %s: %v", blockID, artifactID, err)
	}
	return address
}

// currentPartOf is the one part_of tuple the store asserts for a block now, read as a current
// relation is read (graphstore.CurrentOut), with its sequence and its cause.
func currentPartOf(t *testing.T, database *store.Store, address string) graphstore.Tuple {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), database.Pool, refs.BlockNodeKind, address, []string{graphstore.TuplePartOf})
	if err != nil {
		t.Fatalf("read the parent of %s: %v", address, err)
	}
	if len(held) != 1 {
		t.Fatalf("block %s holds %d current part_of tuples, want exactly one: %+v", address, len(held), held)
	}
	return held[0]
}

// A write that read an older version of a document cannot state a parent a newer version has
// replaced. The race is built rather than waited for: a pass reads the document at the version
// it stands at and is held open before its commit, a newer version that moves one block under
// another parent is written through the real version write path, and the older pass then runs
// on to its commit. The fence refuses it - document_block_versions still holds the newer
// revision, the moved block's current part_of tuple is the one the newer version appended,
// under that version's own event, and the older pass appended no tuple at all - and the same
// pass at a revision above the stored one does state its parents, so it is the fence and not
// the pass's own work that refused.
//
// The fence is one conditional write under the document owner's lock. Each of the writers R23
// holds to it - a version's write, which a person's edit runs through NamedVersion and an upload
// through reference_excerpts.go, the reconciliation tick and the backfill command - decides
// whether it wins the fence in its read phase, under the owner's advisory lock (store.LockOwner,
// through lockArtifactOwner or events.LockOwnerRows), and its write phase writes only what that
// decision let it. Nothing can store a newer revision between that decision and the write: every
// writer of the revision takes the owner's lock before it reads the revision it writes against - a
// version's own index, the trigger a version insert fires, the tick and the backfill, as
// indexOwedDocument's comment states - and a process of the release before, which takes no
// advisory lock, takes the owner's row instead and holds it for its whole transaction, which this
// build takes too, right after its advisory lock, while that release is in the fleet (the owner
// lock's rollout mode, store.LockOwners).
//
// Each writer is driven through the same race, its holder standing for the newer pass: a writer
// of this build, which takes the owner's lock and then the document_block_versions row, and, for
// the two version writes and the tick again, a process of the release before, which takes the
// owner's row and then that row, with this build in the rollout mode. The two version writes are
// the ones production runs: a person's edit that names its version, through the document
// service's calls the edit route makes (ApplyOps, then NamedVersion), and an upload to a document
// that exists, through the calls the upload route makes to write the version and its index. The
// writer is read waiting at the owner's lock, or at the owner's row, rather than at a later step -
// for a version write by requireTheWriteIsFenced's requireWaitingAtTheOwner call, on the statement
// the write's own connection waits at (statementOf), and for the tick by the same check on its
// backend - and a newer revision is stored while it waits; the writer then decides against that
// revision and commits no tuple below it. The backfill takes the fence row after its owner lock
// and reads its rows again under it, so its holder takes that row alone. The source tree is read
// last: for any statement that writes a revision into document_block_versions outside the fence,
// and for a revision test in the erase or in refs.IndexOwedItemBlocks, which R23 reads as writers
// outside the fence.
func TestAStaleWriteCannotAssertAParentANewerEditReplaced(t *testing.T) {
	ctx := context.Background()
	database, service := blockIndexService(t)
	document := createOwedDocument(t, database, service, "CORE-1", "", "race", raceVersion1, "")
	settle(t, database, document)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("the first tick indexed %d documents, want the fixture's one", tick.Documents)
	}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != 1 {
		t.Fatalf("the document's blocks stand at %s, want version 1", standsText(stands))
	}
	mover, holder := blockAddress(t, database, document, "mover"), blockAddress(t, database, document, "holder")
	if was := currentPartOf(t, database, mover); was.ToKind != "artifact" {
		t.Fatalf("at version 1 block %s is part of %s/%s, want the document: the newer version must move it", mover, was.ToKind, was.ToID)
	}

	// The stale pass reads the document as it stands now and is held open before its commit.
	staleMarkdown, staleBlocks := flattenedPass(t, database, document)
	staleConn, err := database.Pool.Dedicated(ctx, "block-index-stale-pass")
	if err != nil {
		t.Fatalf("open the stale pass's connection: %v", err)
	}
	defer staleConn.Close(ctx)
	staleTx, err := staleConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the stale pass: %v", err)
	}
	defer staleTx.Rollback(ctx)

	// The newer edit, written through the real version write path while the stale pass is open:
	// it moves mover under holder.
	if _, err := joinedReplaceText(service, document, raceVersion2, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("edit the document the newer version records: %v", err)
	}
	newer, newerEvent := writeVersionThroughTheRealPath(t, service, document, "The newer edit")
	if newer.Version.Number != 2 {
		t.Fatalf("the newer version is %d, want 2", newer.Version.Number)
	}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != newer.Version.Number {
		t.Fatalf("after the newer version the document's blocks stand at %s, want version %d", standsText(stands), newer.Version.Number)
	}
	moved := currentPartOf(t, database, mover)
	if moved.ToKind != refs.BlockNodeKind || moved.ToID != holder || moved.SourceEvent != newerEvent {
		t.Fatalf("after version %d block %s is part of %s/%s under event %d, want block/%s under the version's event %d",
			newer.Version.Number, mover, moved.ToKind, moved.ToID, moved.SourceEvent, holder, newerEvent)
	}
	stated := parentsNow(t, database, document)

	// The stale pass runs on to its commit, and the fence refuses it.
	const staleVersion = 1
	before := highestTuple(t, database)
	_, won, write, err := refs.IndexBlocks(ctx, staleTx, document, staleVersion, time.Now(), staleMarkdown, staleBlocks, nil, nil)
	if err != nil {
		t.Fatalf("run the stale pass: %v", err)
	}
	if err := graphstore.Caused(ctx, staleTx, write, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the stale pass stated: %v", err)
	}
	if err := staleTx.Commit(ctx); err != nil {
		t.Fatalf("commit the stale pass: %v", err)
	}
	if won {
		t.Errorf("the stale index pass at version %d claimed the document's blocks stand at it, over the stored version %d",
			staleVersion, newer.Version.Number)
	}
	if appended := tuplesSince(t, database, document, before); appended != 0 {
		t.Errorf("the stale index pass at version %d appended %d tuples below the stored version %d, want none",
			staleVersion, appended, newer.Version.Number)
	}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != newer.Version.Number {
		t.Errorf("after the stale index pass at version %d the document's blocks stand at %s, want version %d",
			staleVersion, standsText(stands), newer.Version.Number)
	}
	if now := currentPartOf(t, database, mover); now.Seq != moved.Seq || now.SourceEvent != newerEvent {
		t.Errorf("after the stale index pass at version %d block %s is part of %s/%s (tuple %d, event %d), want the block/%s version %d appended (tuple %d, event %d)",
			staleVersion, mover, now.ToKind, now.ToID, now.Seq, now.SourceEvent, holder, newer.Version.Number, moved.Seq, newerEvent)
	}
	if now := parentsNow(t, database, document); !sameParents(now, stated) {
		t.Errorf("the stale index pass at version %d moved the parents version %d stated from %v to %v",
			staleVersion, newer.Version.Number, stated, now)
	}

	// The control: the same pass above the stored revision does state its parents, so what
	// refused it was the fence and not a pass with nothing to say.
	third := nextVersionRow(t, database, document, staleMarkdown)
	controlTx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the control pass: %v", err)
	}
	defer controlTx.Rollback(ctx)
	_, won, write, err = refs.IndexBlocks(ctx, controlTx, document, third, time.Now(), staleMarkdown, staleBlocks, nil, nil)
	if err != nil {
		t.Fatalf("run the control pass: %v", err)
	}
	if err := graphstore.Caused(ctx, controlTx, write, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the control pass stated: %v", err)
	}
	if err := controlTx.Commit(ctx); err != nil {
		t.Fatalf("commit the control pass: %v", err)
	}
	if !won {
		t.Fatalf("the control pass at version %d, above the stored version %d, did not stand", third, newer.Version.Number)
	}
	flattened := parentsNow(t, database, document)
	for block, parent := range flattened {
		if !strings.HasPrefix(parent, "artifact/") {
			t.Errorf("the control pass left block %s part of %s, want the document: it had work the fence refused the stale pass", block, parent)
		}
	}
	if sameParents(flattened, stated) {
		t.Errorf("the control pass changed nothing, so the stale pass proves nothing: parents %v", flattened)
	}

	requireTheVersionWriteIsFenced(t, database, service, thisBuildRace)
	requireTheUploadIsFenced(t, database, service, thisBuildRace)
	requireTheTickIsFenced(t, database, service, thisBuildRace)
	requireTheBackfillIsFenced(t, database)
	// From here on the release before is in the fleet, and every owner lock of this build takes
	// the owner's row beside it.
	registerEarlierBuildProcess(t, database)
	requireTheVersionWriteIsFenced(t, database, service, earlierBuildRace)
	requireTheUploadIsFenced(t, database, service, earlierBuildRace)
	requireTheTickIsFenced(t, database, service, earlierBuildRace)
	requireOnlyTheFenceWritesTheOwedRow(t)
	requireNoRevisionTestOutsideTheFence(t)
}

// A mark of a higher marking rule that a writer taking no owner lock commits between a lower-rule
// pass's read phase and its stand survives the stand, the pass still states its version, and the
// higher rule's build then states that version itself.
//
// Four writers of document_block_versions take no owner lock (R23): the tick's marking
// (markVersionedDocuments), oweDocumentIndex, the re-index of a scope and refs.RebuildAll. Each may
// write store_owed_rule while a pass that has read the row is planning, so the stand decides in
// its own statement, against the row as it stands then, whether it clears the mark
// (refs.BlockIndexing.queueStand): from what the read phase saw it would clear a mark of a higher
// rule written meanwhile, and the higher rule's build would then list the document no more. The
// race is built rather than waited for, with the harness of
// TestAStaleWriteCannotAssertAParentANewerEditReplaced: a pass of the lower rule on a connection of
// its own reads the document's index (refs.ReadBlockIndex), the version's row and its render and
// plans the pass (refs.PlanBlocks), on a document whose row stands at the version it indexes, below
// it in store_version, with a mark of the pass's own rule; a statement of the higher rule then
// commits on that row, one statement at a time; and only then does the pass queue its write
// phase (store.RunWrite of refs.BlockIndexing.Queue) and commit.
func TestAHigherRulesMarkWrittenDuringALowerRulesPassSurvivesItsStand(t *testing.T) {
	ctx := context.Background()
	database, lower := blockIndexService(t)
	higher := markingRuleBuild(t, database, nextBuildCommit, lower.markingRule+1)
	v, w := lower.markingRule, higher.markingRule
	for _, between := range []struct {
		name  string
		write func(document string) error
	}{
		{"markVersionedDocuments", func(document string) error {
			_, err := database.Pool.Exec(ctx, markVersionedDocuments, []string{document}, w)
			return err
		}},
		{"oweDocumentIndexStatement", func(document string) error {
			return oweDocumentIndex(ctx, database.Pool, document, w)
		}},
	} {
		slug := "raced-" + strings.ToLower(between.name)
		document := createOwedDocument(t, database, lower, "CORE-1", "", slug, nestedSpec(slug), "")
		settle(t, database, document)
		drainBlockIndex(t, lower, "seed of "+slug)
		version := restateAsThePreviousRelease(t, database, lower, document)
		var marked int64
		if err := database.Pool.QueryRow(ctx, markVersionedDocuments, []string{document}, v).Scan(&marked); err != nil || marked != 1 {
			t.Fatalf("the marking of rule %d marked %d document(s) (%v), want %s", v, marked, err, document)
		}
		markdown, blocks, err := lower.TextWithBlocks(ctx, document)
		if err != nil {
			t.Fatalf("read %s: %v", document, err)
		}

		// The lower rule's pass, through its read phase.
		passConn, err := database.Pool.Dedicated(ctx, "block-index-lower-rule-pass")
		if err != nil {
			t.Fatalf("open the pass's connection: %v", err)
		}
		defer passConn.Close(ctx)
		pass, err := passConn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the pass: %v", err)
		}
		defer pass.Rollback(ctx)
		index, err := refs.ReadBlockIndex(ctx, pass, document)
		if err != nil {
			t.Fatal(err)
		}
		record, err := refs.ReadIndexedVersion(ctx, pass, document, version)
		if err != nil {
			t.Fatal(err)
		}
		var versionAt, now time.Time
		if err := pass.QueryRow(ctx, `
			select created_at, now() from artifact_versions where artifact_id = $1 and number = $2
		`, document, version).Scan(&versionAt, &now); err != nil {
			t.Fatalf("read version %d of %s: %v", version, document, err)
		}
		indexing, err := refs.PlanBlocks(ctx, pass, index, v, now, refs.VersionBlocks{
			ArtifactID: document, Record: record, Number: version, At: versionAt, Markdown: markdown, Blocks: blocks,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !indexing.Won() {
			t.Fatalf("the pass of rule %d at version %d of %s lost the fence in its read phase: this test proves nothing", v, version, document)
		}

		// The higher rule's statement commits on the row between the read phase and the stand.
		if err := between.write(document); err != nil {
			t.Fatalf("commit %s of rule %d: %v", between.name, w, err)
		}
		if err := store.RunWrite(ctx, pass, func(b *store.WriteBatch) error {
			indexing.Queue(b)
			return nil
		}); err != nil {
			t.Fatalf("run the pass's write phase: %v", err)
		}
		if err := graphstore.Caused(ctx, pass, indexing.Stated(), graphstore.NoEvent); err != nil {
			t.Fatalf("append what the pass stated: %v", err)
		}
		if err := pass.Commit(ctx); err != nil {
			t.Fatalf("commit the pass: %v", err)
		}

		raced := fmt.Sprintf("%s of rule %d committed between the read phase and the stand of a pass of rule %d", between.name, w, v)
		row := blockRowOf(t, database, document)
		if !row.standsAt(version) {
			t.Errorf("%s: the pass left %s at version %s and stand %s, want version %d stated",
				raced, document, standsText(row.version), standsText(row.stand), version)
		}
		if !row.marked || row.rule != w {
			t.Errorf("%s: the stand cleared the mark of rule %d on %s, want it left", raced, w, document)
		}
		if _, written := tickWrites(t, higher); !written[document] || !blockRowOf(t, database, document).clearedAt(version) {
			t.Errorf("%s: a build of rule %d then left the version %d of %s unwritten, want it written", raced, w, version, document)
		}
	}
}

// nextVersionRow writes one more artifact_versions row holding markdown and answers its number,
// so a pass can be run at a revision above the one the document stands at.
func nextVersionRow(t *testing.T, database *store.Store, artifactID, markdown string) int {
	t.Helper()
	var number int
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1, coalesce(max(number), 0) + 1, $2, '[{"kind":"user","id":"alice"}]'
		from artifact_versions where artifact_id = $1
		returning number
	`, artifactID, markdown).Scan(&number); err != nil {
		t.Fatalf("write one more version row: %v", err)
	}
	return number
}

// statementOf is the statement backend pid is running, read through the pool, whose
// pg_stat_activity is a fresh view on each read: once the backend has queued behind a lock, it is
// the statement it waits at.
func statementOf(t *testing.T, database *store.Store, pid uint32) string {
	t.Helper()
	var statement string
	if err := database.Pool.QueryRow(context.Background(),
		`select query from pg_stat_activity where pid = $1`, pid).Scan(&statement); err != nil {
		t.Fatalf("read the statement backend %d runs: %v", pid, err)
	}
	return strings.Join(strings.Fields(statement), " ")
}

// fenceRace is who stands for the newer pass in a fenced writer's race, and so what that writer
// queues behind: a writer of this build, which takes the document owner's advisory lock first, or
// a process of the release before, which takes the owner's row and holds it for its whole
// transaction, beside which this build's writer takes that row right after its advisory lock.
type fenceRace struct {
	// name says who the holder stands for, slug is the race's own document's, and earlier is
	// the release before.
	name, slug string
	earlier    bool
}

var (
	thisBuildRace    = fenceRace{name: "a newer pass of this build", slug: "fenced"}
	earlierBuildRace = fenceRace{name: "a newer pass of the release before", slug: "fenced-earlier", earlier: true}
)

// holdTheOwner takes, in holder, what the newer pass takes before the document_block_versions
// row: the owner's advisory lock, or for the release before the owner's row `for no key update`,
// as that release takes it. Every document of these races is issue CORE-1's.
func (race fenceRace) holdTheOwner(t *testing.T, ctx context.Context, holder pgx.Tx) {
	t.Helper()
	if race.earlier {
		if _, err := holder.Exec(ctx, `select 1 from issues where key = 'CORE-1' for no key update`); err != nil {
			t.Fatalf("hold issue CORE-1's row as %s: %v", race.name, err)
		}
		return
	}
	if err := store.LockOwner(ctx, holder, store.IssueOwner("CORE-1")); err != nil {
		t.Fatalf("hold issue CORE-1's owner lock as %s: %v", race.name, err)
	}
}

// requireWaitingAtTheOwner fails unless writer, queued behind the holder, waits at the owner's
// lock - beside the release before, at the owner's row this build takes right after it - and at
// no projection or store write further in: the writer decides whether it wins the fence only
// once it holds the owner, so it reaches no projection row and no tuple before.
func requireWaitingAtTheOwner(t *testing.T, writer string, race fenceRace, running string) {
	t.Helper()
	t.Logf("%s waits at: %s", writer, running)
	want, at := "as owner_lock", "the owner's advisory lock"
	if race.earlier {
		want, at = `from issues where key = any($1::text[]) order by key collate "C" for no key update`, "the owner's row"
	}
	if !strings.Contains(running, want) {
		t.Errorf("%s waits at %q, want it waiting at %s", writer, running, at)
	}
	if strings.Contains(running, "document_blocks ") || strings.Contains(running, "block_refs") || strings.Contains(running, "graph_") {
		t.Errorf("%s waits at %q, a projection or store write: the owner comes before any of them", writer, running)
	}
}

// registerEarlierBuildProcess writes a server_processes row as a process of the release before
// the owner lock protocol leaves it: no owner_lock_protocol, so the column's default 0, which
// keeps that release in the owner lock's fleet (api's twin of this helper says the same).
func registerEarlierBuildProcess(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into server_processes (instance, commit, signing_keys)
		values (gen_random_uuid(), 'earlier0build', '{fingerprint}')`); err != nil {
		t.Fatalf("register a process of the release before: %v", err)
	}
}

// requireWaitingAtTheOwedRow fails unless writer, queued behind the holder of a document's
// document_block_versions row, waits at a statement of that row and at no projection write
// further in: the fence is the first write of the backfill, so it reaches no projection row and
// no tuple before it knows whether it won.
func requireWaitingAtTheOwedRow(t *testing.T, writer, running string) {
	t.Helper()
	t.Logf("%s waits behind the held document_block_versions row at: %s", writer, running)
	if !strings.Contains(running, "document_block_versions") {
		t.Errorf("%s waits at %q, want it waiting at the document_block_versions row", writer, running)
	}
	if strings.Contains(running, "document_blocks ") || strings.Contains(running, "block_refs") || strings.Contains(running, "graph_") {
		t.Errorf("%s waits at %q, a projection or store write: the fence must be reached before any of them", writer, running)
	}
}

// requireNothingBelowTheRevision fails if writer committed any tuple or any block row of the
// document after seq, which it may not do once a revision above its own is stored.
func requireNothingBelowTheRevision(t *testing.T, database *store.Store, writer, artifactID string, seq int64, own, stored int) {
	t.Helper()
	if appended := tuplesSince(t, database, artifactID, seq); appended != 0 {
		t.Errorf("%s committed %d tuples at version %d, below the stored revision %d, want none", writer, appended, own, stored)
	}
	if rows := countRows(t, database, "document_blocks", artifactID); rows != 0 {
		t.Errorf("%s committed %d block rows at version %d, below the stored revision %d, want none", writer, rows, own, stored)
	}
}

// versionWriter is one production write of a document's next version that a race drives: it
// runs in tx, on the race's own connection, to its commit, and answers the number of the version
// it wrote, which it picks itself, as a request's write does.
type versionWriter func(ctx context.Context, tx pgx.Tx, document string) (int, error)

// requireTheWriteIsFenced drives one production write of a document's version (write) through the
// race, on a connection of its own. The document stands at version 1 as a build without the block
// index writes it (createOwedDocument): it holds no block row, and its revision row is owed
// (null). The version the write numbers itself, the next one, is 2. The holder, standing for a
// newer pass (race), takes the owner and the document_block_versions row. The write is read
// waiting at the owner before it has planned anything: storetest.WaitForQueuedBehind sees its
// connection queue behind the holder, and the line after it,
// `requireWaitingAtTheOwner(t, writer, race, statementOf(t, database, pid))`, fails unless the
// statement the connection waits at is the owner's lock, or beside the release before the owner's
// row. The holder then stores revision 3, above the write's version, while the write waits.
// Released, the write commits its version, and no tuple and no block row: it decided under the
// owner, against the revision the holder stored.
//
// The revision row then reads owed (null), not the holder's 3. The write's read phase decided that
// it lost the fence (refs' blockStand.admits: 3 is above 2), so its block index queues nothing
// (BlockIndexing.Queue), the fence's own statement (queueStand) included. Its write phase still
// inserts its version, and that insert fires document_block_versions_owe (migration 0122, as
// 20261108070000 rewrote it), which sets the row's version to null for every version a build
// inserts with markdown. Nothing records a revision over that mark afterwards, so the document is
// left owed an index, which the reconciliation tick pays at its latest version
// (indexOwedDocument).
func requireTheWriteIsFenced(t *testing.T, database *store.Store, service *Service, race fenceRace, writer, slug string, write versionWriter) {
	t.Helper()
	ctx := context.Background()
	const own, newer = 2, 3
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, "- alpha\n- beta\n", "")
	settle(t, database, document)

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder of the owner and the owed row of %s: %v", writer, err)
	}
	defer holder.Rollback(ctx)
	race.holdTheOwner(t, ctx, holder)
	if _, err := holder.Exec(ctx,
		`select 1 from document_block_versions where artifact_id = $1 for no key update`, document); err != nil {
		t.Fatalf("hold the owed row of %s: %v", writer, err)
	}

	before := highestTuple(t, database)
	writeConn, err := database.Pool.Dedicated(ctx, "block-index-fenced-"+slug)
	if err != nil {
		t.Fatalf("open the connection of %s: %v", writer, err)
	}
	defer writeConn.Close(ctx)
	pid := writeConn.PgConn().PID()
	type written struct {
		version int
		err     error
	}
	answered := make(chan written, 1)
	go func() {
		// The send is the last thing this goroutine does, and the rollback has already run by
		// then: the caller closes writeConn as soon as it reads this channel, and a rollback
		// still in flight on that same connection races that close (pgx answers it "BUG: slow
		// write timer already active"). Deferring the rollback inside the inner function is
		// what orders the two.
		answered <- func() written {
			tx, err := writeConn.Begin(ctx)
			if err != nil {
				return written{err: err}
			}
			defer tx.Rollback(ctx)
			version, err := write(ctx, tx, document)
			return written{version, err}
		}()
	}()
	if answer, early := storetest.WaitForQueuedBehind(t, holder, pid, answered, fenceWait); early {
		t.Fatalf("%s answered version %d, %v, without ever waiting on the owner", writer, answer.version, answer.err)
	}
	requireWaitingAtTheOwner(t, writer, race, statementOf(t, database, pid))

	// A newer pass wins the row while the write waits.
	if _, err := holder.Exec(ctx,
		`update document_block_versions set version = $2 where artifact_id = $1`, document, newer); err != nil {
		t.Fatalf("store revision %d from under %s: %v", newer, writer, err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the newer pass's revision: %v", err)
	}
	answer := <-answered
	if answer.err != nil {
		t.Fatalf("%s that waited on the owner: %v", writer, answer.err)
	}
	if answer.version != own {
		t.Fatalf("%s wrote version %d, want %d, the version after the one the document stands at", writer, answer.version, own)
	}
	requireNothingBelowTheRevision(t, database, writer, document, before, own, newer)
	if stands := blockIndexAt(t, database, document); stands != nil {
		t.Errorf("after %s at version %d, which lost the fence to revision %d, the document's blocks stand at %s, want owed (null): the write's own version owes the document an index, and a write that lost the fence records no revision",
			writer, own, newer, standsText(stands))
	}
}

// requireTheVersionWriteIsFenced drives the version write of a person's edit that names its
// version through the race (requireTheWriteIsFenced): ApplyOps and then NamedVersion in one
// joined transaction, the version's event after them, and what the version's block index stated
// appended under that event, as api's editArtifact and commitArtifactVersionEvent run them
// (writeVersion, which writeVersionThroughTheRealPath runs too). The route takes the owner's lock
// before those calls (requireOpenOwnerStatus); the race takes no lock of its own in the write's
// transaction, so the owner it reads the write waiting at, in requireTheWriteIsFenced's
// requireWaitingAtTheOwner call, is the one the document service's calls take: ApplyOps'
// joinLiveWrite takes it first, and NamedVersion takes it again, which a transaction that holds
// it does without a wait. A write neither call took it for would wait first at its version's
// insert, whose trigger (document_block_versions_owe) waits behind the holder's
// document_block_versions row, or beside the release before at its event's update of the issue's
// row, and fail that check.
func requireTheVersionWriteIsFenced(t *testing.T, database *store.Store, service *Service, race fenceRace) {
	t.Helper()
	edit := []model.EditOp{{Op: "replace", Find: "alpha", With: "gamma"}}
	requireTheWriteIsFenced(t, database, service, race, "the version write (ApplyOps, then NamedVersion) behind "+race.name,
		race.slug+"-version", func(ctx context.Context, tx pgx.Tx, document string) (int, error) {
			result, _, err := writeVersion(ctx, service, tx, document, edit, "The edit the race names")
			return result.Version.Number, err
		})
}

// requireTheUploadIsFenced drives the other production write of a version through the race
// (requireTheWriteIsFenced): an upload to a document the issue holds (api's storeArtifact). The
// route is api's, so the race makes the route's calls that write the version and its index, in
// the route's order: the owner's lock before any document work (requireOpenOwnerStatus, which
// takes the lock lockArtifactOwner takes for an issue's document, taken here through
// lockArtifactOwner), the text's replacement (ReplaceText), the version (Ledger.QueueVersion),
// its index through reference_excerpts.go (IndexDocumentReferences), the version's event,
// appended at once where the route queues it, and what the index stated under that event. It
// leaves out the route's writes that touch no block index: the task counts, the approval
// request's move and carry, and the diff. The upload is read waiting at that lock of its own, in
// requireTheWriteIsFenced's requireWaitingAtTheOwner call, before ReplaceText's joinLiveWrite
// takes the lock again. An upload that creates its document races nothing: no other transaction
// can hold the revision row of a document before the transaction that creates it commits, so
// the race drives the route's upload to a document that exists.
func requireTheUploadIsFenced(t *testing.T, database *store.Store, service *Service, race fenceRace) {
	t.Helper()
	alice := model.Actor{Kind: "user", ID: "alice"}
	requireTheWriteIsFenced(t, database, service, race, "the upload (IndexDocumentReferences) behind "+race.name,
		race.slug+"-upload", func(ctx context.Context, tx pgx.Tx, document string) (int, error) {
			joined, ledger := service.Join(ctx, tx)
			defer ledger.Discard()
			if _, _, err := lockArtifactOwner(joined, tx, document); err != nil {
				return 0, fmt.Errorf("take the document's owner: %w", err)
			}
			precondition, err := replacementPrecondition(joined, service, document)
			if err != nil {
				return 0, fmt.Errorf("read the document's token: %w", err)
			}
			stored, err := service.ReplaceText(joined, document, "- alpha\n- gamma\n", alice, precondition)
			if err != nil {
				return 0, fmt.Errorf("replace the document's text: %w", err)
			}
			version, err := ledger.QueueVersion(ctx, document, stored, []model.Actor{alice}, nil)
			if err != nil {
				return 0, fmt.Errorf("queue the upload's version: %w", err)
			}
			ledger.WroteVersion(document, version, stored)
			_, write, err := service.IndexDocumentReferences(joined, document, version.Number, stored)
			if err != nil {
				return 0, fmt.Errorf("index version %d: %w", version.Number, err)
			}
			payload, err := json.Marshal(map[string]any{
				"artifact_id": document,
				"version":     map[string]any{"number": version.Number},
			})
			if err != nil {
				return 0, err
			}
			event, err := documentEvent(ctx, tx, "artifact.version", payload)
			if err != nil {
				return 0, err
			}
			if err := causeInWritePhase(joined, tx, write, event); err != nil {
				return 0, fmt.Errorf("append what the index stated: %w", err)
			}
			return version.Number, ledger.Commit(ctx)
		})
}

// requireTheTickIsFenced drives the block-index reconciliation through the same race: the holder,
// standing for a newer pass (race), takes the owner and the document's owed row while the tick
// runs, the tick is read waiting at the owner, the row is raised to a revision above the version
// the tick listed from under it, and released. The tick must commit no tuple below that stored
// revision.
func requireTheTickIsFenced(t *testing.T, database *store.Store, service *Service, race fenceRace) {
	t.Helper()
	ctx := context.Background()
	writer := "the reconciliation tick (ReconcileBlockIndex) behind " + race.name
	const own = 1
	fenced := createOwedDocument(t, database, service, "CORE-1", "", race.slug, "- alpha\n- beta\n", "")
	settle(t, database, fenced)
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder of the tick's owner and owed row: %v", err)
	}
	defer holder.Rollback(ctx)
	race.holdTheOwner(t, ctx, holder)
	var holderPID uint32
	if err := holder.QueryRow(ctx, `
		select pg_backend_pid() from document_block_versions where artifact_id = $1 for no key update
	`, fenced).Scan(&holderPID); err != nil {
		t.Fatalf("hold the tick's owed row: %v", err)
	}

	before := highestTuple(t, database)
	ticked := make(chan BlockIndexTick, 1)
	go func() {
		tick, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, liveAheadDocuments{})
		if err != nil {
			t.Errorf("the fenced tick: %v", err)
		}
		ticked <- tick
	}()
	if waiters := storetest.WaitForLockWaiters(t, holder, 1, fenceWait); waiters < 1 {
		t.Fatalf("the tick never queued behind %s", race.name)
	}
	// The tick runs on the pool, so its backend is the one the holder blocks.
	var tickPID uint32
	if err := database.Pool.QueryRow(ctx, `
		select pid from pg_stat_activity where $1 = any(pg_blocking_pids(pid)) order by query_start limit 1
	`, holderPID).Scan(&tickPID); err != nil {
		t.Fatalf("find the backend the holder of the tick's owner blocks: %v", err)
	}
	requireWaitingAtTheOwner(t, writer, race, statementOf(t, database, tickPID))
	// A newer pass won the race while the tick waited: the row stands at a revision above the
	// version the tick listed.
	const newer = 99
	if _, err := holder.Exec(ctx,
		`update document_block_versions set version = $2 where artifact_id = $1`, fenced, newer); err != nil {
		t.Fatalf("raise the owed row from under the tick: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the newer pass's mark: %v", err)
	}
	<-ticked
	// What the tick must not do is commit the work of a revision below the stored one: no tuple
	// and no projection row. The stored revision itself is not read back here, because the same
	// tick's grammar arm owes the document an index again (oweDocumentIndex) and sets it null.
	requireNothingBelowTheRevision(t, database, writer, fenced, before, own, newer)
}

// requireTheBackfillIsFenced drives the backfill command (graphcensus.Backfill) through the
// race R23 holds it to. Its race is a block a version deletes while the backfill waits: a
// document whose blocks were written before the store existed stands at version 1, and a second
// transaction - the version write that indexes version 2 and drops one of those blocks - takes
// the document's document_block_versions row, stores version 2 there and deletes the block's
// row. The backfill starts while that transaction is open, so its read of the batch still sees
// the block, and it is read waiting at the owed row. Released, it must commit no node and no
// tuple for the block version 2 deleted, and write the block version 2 kept.
func requireTheBackfillIsFenced(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	const writer = "the backfill command (graphcensus.Backfill)"
	const own, newer = 1, 2
	var document, refKey string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'fenced-backfill', 'fenced-backfill.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`).Scan(&document, &refKey); err != nil {
		t.Fatalf("create the backfill's document: %v", err)
	}
	// Version 1 and its blocks as a build before the store wrote them: rows naming no node.
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, $2, 'Written before the store existed.', '[{"kind":"user","id":"alice"}]')
	`, document, own); err != nil {
		t.Fatalf("write version %d of the backfill's document: %v", own, err)
	}
	for _, blockID := range []string{"deleted", "kept"} {
		if _, err := database.Pool.Exec(ctx, `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			values ($1::uuid, $2, $3, 'paragraph', '', $4, $5, now())
		`, document, blockID, refKey+"#"+blockID, "Block "+blockID+".", own); err != nil {
			t.Fatalf("write block %s of the backfill's document: %v", blockID, err)
		}
	}
	deleted, kept := refKey+"#deleted", refKey+"#kept"

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the version write the backfill races: %v", err)
	}
	defer holder.Rollback(ctx)
	for _, statement := range []struct {
		what, sql string
		args      []any
	}{
		{"take the owed row", `select 1 from document_block_versions where artifact_id = $1 for no key update`, []any{document}},
		{"write version 2", `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, $2, 'Version 2 drops a block.', '[{"kind":"user","id":"alice"}]')`, []any{document, newer}},
		{"store version 2 as the revision", `update document_block_versions set version = $2 where artifact_id = $1`, []any{document, newer}},
		{"delete the block version 2 drops", `delete from document_blocks where artifact_id = $1 and block_id = 'deleted'`, []any{document}},
	} {
		if _, err := holder.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("the version write could not %s: %v", statement.what, err)
		}
	}

	// One connection, so every statement of the run is that backend's and the test can watch it.
	backfiller := storetest.SingleConnection(t, database)
	var pid uint32
	if err := backfiller.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the backfill's backend: %v", err)
	}
	type backfillAnswer struct {
		report graphcensus.BackfillReport
		err    error
	}
	answered := make(chan backfillAnswer, 1)
	go func() {
		report, err := graphcensus.Backfill(ctx, backfiller, io.Discard)
		answered <- backfillAnswer{report, err}
	}()
	if early, done := storetest.WaitForQueuedBehind(t, holder, pid, answered, fenceWait); done {
		t.Fatalf("%s answered (report %#v, err %v) without waiting on the owed row", writer, early.report, early.err)
	}
	requireWaitingAtTheOwedRow(t, writer, statementOf(t, database, pid))
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the version write the backfill races: %v", err)
	}
	var answer backfillAnswer
	select {
	case answer = <-answered:
	case <-time.After(fenceWait):
		t.Fatalf("%s did not finish within %s of the owed row's release", writer, fenceWait)
	}
	if answer.err != nil {
		t.Fatalf("%s: %v", writer, answer.err)
	}

	var nodes, tuples int64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from graph_nodes where kind = $1 and id = $2),
		       (select count(*) from graph_tuples where from_kind = $1 and from_id = $2)
	`, refs.BlockNodeKind, deleted).Scan(&nodes, &tuples); err != nil {
		t.Fatalf("read what the store holds of %s: %v", deleted, err)
	}
	if nodes != 0 || tuples != 0 {
		t.Errorf("%s committed %d node(s) and %d tuple(s) for block %s at version %d, below the stored revision %d, which deleted it while the backfill waited",
			writer, nodes, tuples, deleted, own, newer)
	}
	held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, kept, []string{refs.TupleInDocument})
	if err != nil {
		t.Fatalf("read the document %s is in: %v", kept, err)
	}
	if len(held) != 1 || held[0].ToID != refKey {
		t.Errorf("%s left block %s, which version %d kept, in %+v, want one in_document tuple to %s: the race proves nothing if the backfill wrote no block",
			writer, kept, newer, held, refKey)
	}
}

// fenceWriter names where a statement that writes document_block_versions may live: the one
// fenced writer, the block index's record of the version it indexes on its write phase
// (BlockIndexing.queueStand), which the plan queues only where it won the fence. A statement that
// only owes a document an index writes no revision at all, so it is told apart by what it sets
// rather than by where it is.
const (
	fenceWriterFile     = "refs/block_stand.go"
	fenceWriterFunction = "func (b BlockIndexing) queueStand("
)

// owedRowWrite matches one statement of a Go source file that writes document_block_versions,
// up to the backtick that ends it.
var owedRowWrite = regexp.MustCompile("(?is)(insert into|update)\\s+document_block_versions\\b[^`]*")

// requireOnlyTheFenceWritesTheOwedRow reads the source tree and fails on any statement that
// writes a revision into document_block_versions outside the fenced writer. Each part of a
// statement is held on its own (writesTheRevision): an insert's column list, and the assignments
// of an update's set or of an insert's `on conflict ... do update set`; the statement writes a
// revision where any part does. A set that assigns the revision null owes the document an index
// and asserts nothing, so it is allowed wherever it is, and it excuses no other part of its
// statement: an insert whose column list names `version` is the fence's alone even where its
// conflict arm sets `version = null`. A backfill or a repair that wrote one would be caught here
// rather than in production.
func requireOnlyTheFenceWritesTheOwedRow(t *testing.T) {
	t.Helper()
	found := 0
	for _, file := range dispatchSourceFiles(t) {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := string(body)
		for _, match := range owedRowWrite.FindAllStringIndex(text, -1) {
			statement := text[match[0]:match[1]]
			if !writesTheRevision(statement) {
				// A statement of this table that assigns no revision claims none: the
				// window pass clears `windows_owed_at` on a row the fence already wrote,
				// which says nothing about which pass owns the version. What this scan
				// holds a statement to is what it sets, not which table it touches, which
				// is the same rule that lets a part setting `version = null` stand anywhere.
				continue
			}
			found++
			enclosing := enclosingFunction(text, match[0])
			if !strings.HasSuffix(filepath.ToSlash(file), fenceWriterFile) || !strings.HasPrefix(enclosing, fenceWriterFunction) {
				t.Errorf("%s writes a revision into document_block_versions in %q, outside the fenced writer %s in %s:\n%s",
					file, enclosing, fenceWriterFunction, fenceWriterFile, statement)
			}
		}
	}
	if found == 0 {
		t.Errorf("the source read found no statement writing a revision into document_block_versions, so it proves nothing")
	}
}

// outsideTheFence is the two writers R23 reads as writers outside the fence: the erase, which
// deletes under its own transaction and asserts no current set, and the item block pass, which
// writes no store row in this child. Each is named by its file and its `func` line.
var outsideTheFence = []struct{ file, function string }{
	{"grapherase/erase.go", "func (e *Erasure) apply("},
	{"refs/item_blocks.go", "func IndexOwedItemBlocks("},
}

// revisionTest matches what a revision test is made of in a function's source: a read or a
// write of document_block_versions, the row that holds a document's revision, whose `version`
// is what such a test compares against, or a call of the fence that compares it
// (queueStand) or of the pass that runs that fence (IndexBlocks).
var revisionTest = regexp.MustCompile(`(?is)\bdocument_block_versions\b|\bqueueStand\(|\bIndexBlocks\(`)

// requireNoRevisionTestOutsideTheFence reads the source of each writer outside the fence and
// fails if either carries a revision test. A writer that compared a revision it does not own
// would refuse a write the fence never meant to refuse, or claim the fence's row for a set it
// does not compute. The comments are taken out first, since the tree writes its reasoning
// about the fence in them.
func requireNoRevisionTestOutsideTheFence(t *testing.T) {
	t.Helper()
	for _, writer := range outsideTheFence {
		path := filepath.Join("..", filepath.FromSlash(writer.file))
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := goLineComment.ReplaceAllString(string(source), "")
		start := strings.Index(text, "\n"+writer.function)
		if start < 0 {
			t.Fatalf("%s holds no %q: the writer R23 reads as outside the fence is not where this test looks", writer.file, writer.function)
		}
		body := functionBody(text, start+1+len(writer.function))
		t.Logf("%s %s: %d lines read for a revision test", writer.file, writer.function, strings.Count(body, "\n")+1)
		for _, found := range revisionTest.FindAllString(body, -1) {
			t.Errorf("%s %s carries a revision test (%q): R23 reads it as a writer outside the fence, which compares no revision",
				writer.file, writer.function, found)
		}
	}
}

// writesTheRevision reports whether a statement of document_block_versions writes a revision
// into its `version` column, reading each part of the statement on its own; the statement writes
// one where any part does. `version` is that column alone: store_version, windows_owed_at and
// the row's other columns write none.
//
//   - An insert writes one where its column list names `version`, whatever value fills it; its
//     values and its select list are not read. The fence's is the one Go statement of the tree
//     whose column list names `version`, and the inserts that write it as null, the triggers of
//     migrations 0122 and 20261108070000, are SQL migrations, which this scan does not read. An
//     insert with no column list, which fills every column by position, or whose list cannot be
//     read, is held to write one.
//   - An update's set, and an insert's `on conflict ... do update set`, write one in each
//     assignment to `version` of anything but the literal null. A parenthesized target list that
//     names `version` writes one whatever it assigns, and a set or an assignment that cannot be
//     read is held to write one.
//
// A statement that touches the row for any other reason writes no revision, so the fence has
// nothing to say about where it lives.
func writesTheRevision(statement string) bool {
	lowered := strings.ToLower(statement)
	rest := lowered[strings.Index(lowered, "document_block_versions")+len("document_block_versions"):]
	if !strings.HasPrefix(lowered, "insert") {
		set, found := sqlTopLevel(rest, "set")
		return !found || setWritesTheRevision(rest[set+len("set"):])
	}
	columns, listed := sqlParenthesized(rest[len(sqlInsertAlias.FindString(rest)):])
	if !listed || slices.ContainsFunc(sqlList(columns), isVersionColumn) {
		return true
	}
	conflict, found := sqlTopLevel(rest, "conflict")
	if !found {
		return false
	}
	rest = rest[conflict:]
	arm, found := sqlTopLevel(rest, "set", "nothing")
	if !found {
		return true
	}
	return strings.HasPrefix(rest[arm:], "set") && setWritesTheRevision(rest[arm+len("set"):])
}

// sqlInsertAlias is the alias an insert may give its table before its column list.
var sqlInsertAlias = regexp.MustCompile(`^\s*as\s+\w+`)

// setWritesTheRevision reports whether the assignments of a set, s being the lowered statement
// after its `set` and the list running to its from, where or returning, write a revision
// (writesTheRevision).
func setWritesTheRevision(s string) bool {
	for _, assignment := range sqlList(s, "from", "where", "returning") {
		target, value, found := strings.Cut(assignment, "=")
		if !found {
			return true
		}
		target = strings.TrimSpace(target)
		if !strings.HasPrefix(target, "(") {
			if isVersionColumn(target) && strings.TrimSpace(value) != "null" {
				return true
			}
			continue
		}
		names, listed := sqlParenthesized(target)
		if !listed || slices.ContainsFunc(sqlList(names), isVersionColumn) {
			return true
		}
	}
	return false
}

// isVersionColumn reports whether name, trimmed, is the column `version`.
func isVersionColumn(name string) bool {
	return strings.Trim(name, `"`) == "version"
}

// sqlParenthesized is the inside of the parenthesis s opens, after any spaces, or false where s
// opens none or the parenthesis does not close.
func sqlParenthesized(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "(") {
		return "", false
	}
	end, _ := sqlTopLevel(s[1:])
	if 1+end == len(s) || s[1+end] != ')' {
		return "", false
	}
	return s[1 : 1+end], true
}

// sqlList is the items of the list s begins with, split at its top-level commas and each
// trimmed. The list runs to the first of ends at its top level, or to where s ends
// (sqlTopLevel).
func sqlList(s string, ends ...string) []string {
	end, _ := sqlTopLevel(s, ends...)
	s = s[:end]
	var items []string
	for {
		comma, found := sqlTopLevel(s, ",")
		items = append(items, strings.TrimSpace(s[:comma]))
		if !found {
			return items
		}
		s = s[comma+1:]
	}
}

// sqlTopLevel returns the offset in s of the first of words that stands at the top level of s -
// outside parentheses, quotes and `--` comments, and as a whole word where it is one - and true.
// Where none does before s ends, at a `;`, at a `)` that closes a parenthesis opened before s, or
// at the end of s, it returns the offset of that end and false.
func sqlTopLevel(s string, words ...string) (int, bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'' || c == '"':
			end := strings.IndexByte(s[i+1:], c)
			if end < 0 {
				return len(s), false
			}
			i += end + 1
		case strings.HasPrefix(s[i:], "--"):
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				return len(s), false
			}
			i += end
		case c == '(':
			depth++
		case depth == 0 && (c == ')' || c == ';'):
			return i, false
		case c == ')':
			depth--
		case depth == 0:
			for _, word := range words {
				if sqlWordAt(s, i, word) {
					return i, true
				}
			}
		}
	}
	return len(s), false
}

// sqlWordAt reports whether word stands at offset i of s, not run into a word before or after
// it where word itself starts or ends with a word character.
func sqlWordAt(s string, i int, word string) bool {
	if !strings.HasPrefix(s[i:], word) {
		return false
	}
	if sqlWordByte(word[0]) && i > 0 && sqlWordByte(s[i-1]) {
		return false
	}
	end := i + len(word)
	return !sqlWordByte(word[len(word)-1]) || end == len(s) || !sqlWordByte(s[end])
}

// sqlWordByte reports whether c can be part of an SQL word: a letter, a digit, `_` or `$`.
func sqlWordByte(c byte) bool {
	return c == '_' || c == '$' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// dispatchSourceFiles is every non-test Go file of the Dispatch tree and of the commands that
// write to it, read from this package's own directory.
func dispatchSourceFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, root := range []string{"..", "../../../cmd"} {
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files = append(files, path)
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(files) == 0 {
		t.Fatalf("the source read found no Go file; the tree is not where this test looks")
	}
	return files
}

// enclosingFunction is the `func` line above offset in a gofmt-clean file, which is the
// function a statement written there lives in.
func enclosingFunction(text string, offset int) string {
	if offset < 0 {
		return ""
	}
	start := strings.LastIndex(text[:offset], "\nfunc ")
	if start < 0 {
		return ""
	}
	line := text[start+1:]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	return line
}
