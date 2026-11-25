package docs

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A link the release before this one adds, and a block it drops, after the backfill marked their
// document reach the store when this build's reconciliation drains the mark, and the census
// reaches zero.
//
// The order is the one a rollback gives, where the README calls the backfill safe to run: the
// release before this one serves, the backfill runs and marks each document it named the nodes
// of (store_owed_at), and a person then edits one of those documents through that release. The
// edit drops the innermost callout, whose paragraph holds the document's mention of CORE-2, and
// gives the mention to the middle paragraph instead. That release's index pass inserts the
// block_refs row of the link the version adds naming no node, deletes the rows of the blocks the
// version drops and of the dropped paragraph's link, and states nothing in the store. Once this
// build serves again, its reconciliation indexes the marked document, states every block's parent
// and clears the mark. The added link's row must then name its block's node and the store must
// assert that link, or the census refuses with `the row names no node`; and the dropped paragraph
// must read gone and its link must be retracted, or the census refuses a live node and an asserted
// link no row projects. Either lasts until someone runs the backfill again, which nothing tells
// anyone to do.
//
// The tester drove exactly that on 2026-11-18 at 696a9727c4fe with legion-envoy-v30.45.0 and
// this build on one database: after this build drained the mark, `census --store` refused with
// one difference, `block_ref CORE-4/drive-md#<block> mentions -> issue CORE-3: the row names no
// node`, and the block's tuples held its part_of and no mentions.
//
// The stand-in for that release is indexAsThePreviousRelease, as in
// TestTheBlockAndTheLinkThePreviousReleaseDroppedAreGoneAfterTheBackfill, run here after the
// backfill rather than before it.
func TestALinkThePreviousReleaseAddsAfterTheBackfillReachesTheStore(t *testing.T) {
	t.Parallel()
	link := writePreviousReleaseLink(t)
	database, document, refKey, middle, added := link.database, link.document, link.refKey, link.middle, link.added
	ctx := context.Background()

	// This build serves again and drains the mark.
	drainBlockIndex(t, link.service, "after the previous release's version")

	middleAddress := refKey + "#" + middle
	var named bool
	if err := database.Pool.QueryRow(ctx, `
		select node_kind is not null from block_refs
		where artifact_id = $1 and block_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
	`, document, middle, added.kind, added.toKind, added.toID).Scan(&named); err != nil {
		t.Fatalf("read the middle paragraph's link row: %v", err)
	}
	if !named {
		t.Errorf("the block_refs row of %s %s %s:%s names no node after this build drained the backfill's mark",
			middleAddress, added.kind, added.toKind, added.toID)
	}
	held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, middleAddress, []string{added.kind})
	if err != nil {
		t.Fatalf("read the links the store states out of %s: %v", middleAddress, err)
	}
	if len(held) != 1 || held[0].ToKind != added.toKind || held[0].ToID != added.toID {
		t.Errorf("the store states %#v out of %s, want the %s %s:%s the version holds", held, middleAddress, added.kind, added.toKind, added.toID)
	}
	// The previous release wrote its version 2 with no version node, as it writes every version,
	// and the drain of the mark writes it (graphcensus.MissingVersionNodes), so the census
	// below reads zero with no second backfill.
	requirePresentVersionNode(t, database, refKey, previousReleaseVersion, "after this build drained the backfill's mark")
	// The inner paragraph the previous release's version dropped reads gone, and its link is
	// retracted, once this build drained the mark.
	requireDroppedBlockGone(t, database, link.innerAddress, link.dropped)
	requireNoCensusDifference(t, database, "after this build drained the backfill's mark over a link the previous release added and a block it dropped")
}

// A version write of this build that drains the mark gives the previous release's version its node
// beside its own, with no tick between them.
//
// The order is TestALinkThePreviousReleaseAddsAfterTheBackfillReachesTheStore's up to the previous
// release's version 2, which holds no version node and leaves the document holding the backfill's
// mark. A person then writes version 3 through this build before any tick reaches the document:
// that write's index pass is the one that drains the mark, and once the mark is cleared no tick
// reaches the document again, so version 2 would hold no node and the census would refuse until
// someone ran the backfill again. The write gives version 2 its node in its own transaction, and
// leaves version 3's to the version write itself (WriteVersionNode), so version 3's node is the
// one every version write states: dated at its own row, made by its writer, its facts stated once,
// under the event that recorded it.
func TestAVersionWriteThatDrainsTheMarkGivesThePreviousReleasesVersionItsNode(t *testing.T) {
	link := writePreviousReleaseLink(t)
	database, document, refKey := link.database, link.document, link.refKey
	ctx := context.Background()

	writer := model.Actor{Kind: "user", ID: "bob"}
	const ownVersion = previousReleaseVersion + 1
	event := writeVersionThroughThisBuild(t, link, writer, ownVersion)
	var marked int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from document_block_versions where artifact_id = $1 and store_owed_at is not null`, document).Scan(&marked); err != nil || marked != 0 {
		t.Fatalf("the document holds the backfill's mark %d time(s) (%v) after this build's version write, want it drained by that write's pass: this test proves nothing", marked, err)
	}

	requirePresentVersionNode(t, database, refKey, previousReleaseVersion, "after this build's version write drained the backfill's mark")
	requireOwnVersionNode(t, database, document, refKey, ownVersion, writer, event)
	requireNoCensusDifference(t, database, "after this build's version write drained the backfill's mark over a link the previous release added")
}

// A version write of this build that drains the mark writes no more of the previous release's
// version nodes than missingVersionNodesBound in its own request: where more are missing it writes
// none of them, owes the document its block index again in the same transaction, and the
// reconciliation's next tick writes every one.
//
// The order is TestAVersionWriteThatDrainsTheMarkGivesThePreviousReleasesVersionItsNode's, with
// the previous release writing missingVersionNodesBound more versions after its version 2, each
// with no node, so one more version than the bound holds none when this build writes its own.
func TestAVersionWriteLeavesMoreMissingVersionNodesThanItsBoundToTheTick(t *testing.T) {
	link := writePreviousReleaseLink(t)
	database, service, document, refKey := link.database, link.service, link.document, link.refKey
	ctx := context.Background()
	const missing = missingVersionNodesBound + 1
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1, $2::int + n, v.markdown, v.authors
		from generate_series(1, $3::int) n, artifact_versions v
		where v.artifact_id = $1 and v.number = $2
	`, document, previousReleaseVersion, missing-1); err != nil {
		t.Fatalf("write the previous release's further versions: %v", err)
	}
	missingVersions := func() []int {
		t.Helper()
		var numbers []int
		for number := previousReleaseVersion; number < previousReleaseVersion+missing; number++ {
			if _, stands, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindVersion, graphstore.VersionID(refKey, number)); err != nil {
				t.Fatalf("read the node of version %d: %v", number, err)
			} else if !stands {
				numbers = append(numbers, number)
			}
		}
		return numbers
	}
	if held := missingVersions(); len(held) != missing {
		t.Fatalf("before this build's write %d of the previous release's versions hold no node, want %d: this test proves nothing", len(held), missing)
	}

	writer := model.Actor{Kind: "user", ID: "bob"}
	ownVersion := previousReleaseVersion + missing
	event := writeVersionThroughThisBuild(t, link, writer, ownVersion)
	if held := missingVersions(); len(held) != missing {
		t.Errorf("after this build's write %d of the previous release's %d versions hold no node, want all %d left to the tick: the request wrote more than %d",
			len(held), missing, missing, missingVersionNodesBound)
	}
	var version *int
	var owed bool
	if err := database.Pool.QueryRow(ctx, `
		select version, store_owed_at is not null from document_block_versions where artifact_id = $1
	`, document).Scan(&version, &owed); err != nil {
		t.Fatalf("read the document's block index marks: %v", err)
	}
	if version != nil || !owed {
		t.Errorf("after this build's write the document's block index stands at %s with this build's mark %t, want it owed by both marks, in the write's transaction",
			describeVersion(version), owed)
	}
	requireOwnVersionNode(t, database, document, refKey, ownVersion, writer, event)

	drainBlockIndex(t, service, "after this build's write left the versions to the tick")
	if held := missingVersions(); len(held) != 0 {
		t.Errorf("after the tick versions %v hold no node, want the tick to write every one", held)
	}
	requireOwnVersionNode(t, database, document, refKey, ownVersion, writer, event)
	requireNoCensusDifference(t, database, "after the tick wrote the previous release's version nodes")
}

// A version the release before writes of a document of an issue room, with no version node, has
// its node at the room's next drain, as one document's version has, through either drain of the
// room: the reconciliation's tick, which indexes the room whole on the null version that
// version's row left (indexOwedRoom), and a settlement of the room, which writes a version of
// another of its documents while the backfill's mark stands (commitRoomVersions). After either,
// every version of every document of the room holds its node, the previous release's made by its
// first author at its row's time, and the settlement's own version's node is the one its write
// states: dated at its row, made by its writer, under the event that recorded it.
//
// The room is createIssueRoom's, run through the backfill, which names its documents' nodes and
// their first versions' and marks each document (store_owed_at). The release before then writes
// the plan's version 2 with no node: in the tick's case after a tick drained the backfill's
// marks, so that version leaves the null version alone; in the settlement's case with the marks
// standing, and a browser's paragraph in the spec's fragment is what the settlement saves.
func TestAVersionThePreviousReleaseWritesInAnIssueRoomHasItsNodeAtTheRoomsNextDrain(t *testing.T) {
	for _, through := range []string{"the tick", "a settlement of the room"} {
		t.Run(through, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			service, server := newTestServiceInstance(t, database)
			room := createIssueRoom(t, database, 1)
			if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
				t.Fatalf("backfill the store: %v", err)
			}
			tick := through == "the tick"
			if tick {
				drainBlockIndex(t, service, "the backfill's marks")
			}
			bob := model.Actor{Kind: "user", ID: "bob"}
			var previousAt time.Time
			if err := database.Pool.QueryRow(ctx, `
				insert into artifact_versions (artifact_id, number, markdown, authors)
				values ($1, 2, $2, '[{"kind":"user","id":"bob"}]') returning created_at
			`, room.plan, planSeed).Scan(&previousAt); err != nil {
				t.Fatalf("write the plan's version 2 as the release before writes it: %v", err)
			}
			refKeys := map[string]string{}
			for _, document := range []string{room.spec, room.verification, room.plan, room.tree} {
				var refKey string
				if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, document).Scan(&refKey); err != nil {
					t.Fatalf("read the ref_key of %s: %v", document, err)
				}
				refKeys[document] = refKey
			}
			if _, stands, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindVersion, graphstore.VersionID(refKeys[room.plan], 2)); err != nil || stands {
				t.Fatalf("the plan's version 2 holds a node (%t, %v) before the drain: this test proves nothing", stands, err)
			}

			if tick {
				drainBlockIndex(t, service, "the previous release's version")
			} else {
				peer := connectPeer(t, server.URL, room.spec, "alice")
				stored := roomUpdateCount(t, database, room.tree)
				if _, err := peer.Send(writeParagraph(peer.Doc, room.spec, "Spec words.")); err != nil {
					t.Fatalf("write into the spec's fragment: %v", err)
				}
				waitForStoredUpdates(t, database, room.tree, stored)
				if err := service.waitForDurableAppends(ctx, room.tree); err != nil {
					t.Fatalf("wait for the room's appends: %v", err)
				}
				if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
					t.Fatalf("settle the issue's room: %v", err)
				}
			}

			for document, refKey := range refKeys {
				var numbers []int
				if err := database.Pool.QueryRow(ctx, `
					select array_agg(number order by number) from artifact_versions where artifact_id = $1
				`, document).Scan(&numbers); err != nil {
					t.Fatalf("read the versions of %s: %v", refKey, err)
				}
				for _, number := range numbers {
					requirePresentVersionNode(t, database, refKey, number, "after "+through+" drained the room")
				}
			}
			previous := requirePresentVersionNode(t, database, refKeys[room.plan], 2, "after "+through+" drained the room")
			if !previous.MadeBy.SameAs(bob) || !previous.MadeAt.Equal(previousAt) {
				t.Errorf("the node of the plan's version 2 was made at %s by %+v, want at its row's %s by its first author %+v",
					previous.MadeAt, previous.MadeBy, previousAt, bob)
			}
			if tick {
				return
			}
			// The settlement's own version of the spec, and the event that recorded it.
			var own int
			var event int64
			var actor []byte
			if err := database.Pool.QueryRow(ctx, `
				select v.number, e.id, e.actor from artifact_versions v
				join events e on e.type = 'artifact.version' and e.payload->>'artifact_id' = v.artifact_id::text
				 and e.payload->'version'->>'number' = v.number::text
				where v.artifact_id = $1 order by v.number desc limit 1
			`, room.spec).Scan(&own, &event, &actor); err != nil {
				t.Fatalf("read the settlement's version of the spec and its event: %v", err)
			}
			var writer model.Actor
			if err := json.Unmarshal(actor, &writer); err != nil {
				t.Fatalf("read the actor of the spec's version %d's event: %v", own, err)
			}
			if own != 2 {
				t.Fatalf("the settlement wrote the spec's version %d, want its version 2", own)
			}
			requireOwnVersionNode(t, database, room.spec, refKeys[room.spec], own, writer, event)
		})
	}
}

// writeVersionThroughThisBuild writes version want of link's document through this build, as the
// route that snapshots a document writes it: a live edit, then the version write in the route's
// transaction, the version's artifact.version event, and what the write stated appended under
// that event. It answers the event.
func writeVersionThroughThisBuild(t *testing.T, link previousReleaseLink, writer model.Actor, want int) int64 {
	t.Helper()
	database, service, document := link.database, link.service, link.document
	ctx := context.Background()
	editLiveTree(t, service, document, func(tree *pmdoc.Node) *pmdoc.Node {
		replaceText(tree, "Outer of linked.", "Outer of linked, edited through this build.")
		return tree
	})
	waitForLandedAppends(t, service, document)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the version write: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	written, err := service.SnapshotVersion(joined, document, writer)
	if err != nil {
		t.Fatalf("write version %d through this build: %v", want, err)
	}
	if !written.Wrote || written.Version.Number != want {
		t.Fatalf("the version write wrote %t at version %d, want version %d", written.Wrote, written.Version.Number, want)
	}
	// The version's event takes its seq off the issue's own counter, as the route's append does
	// (recordIssueEvent), so a later event of the issue, such as a tick's artifact.indexed, does not
	// take the same seq.
	event, err := recordIssueEvent(ctx, tx, "CORE-1", "artifact.version", []byte(`{"kind":"user","id":"bob"}`),
		versionEventPayload(t, document, "linked.md", want))
	if err != nil {
		t.Fatalf("append version %d's event: %v", want, err)
	}
	// The version write queues its writes on the ledger, its node's among them, so what it stated
	// is caused in the same write phase, after them, and the head write of the nodes it stated
	// follows, which writes their node_search rows, as the route queues both
	// (commitArtifactVersionEvent).
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		graphstore.QueueCaused(b, written.Blocks, event)
		graphstore.QueueHeads(b, graphstore.HeldHeads(written.Blocks))
		return nil
	}); err != nil {
		t.Fatalf("queue what version %d's write stated: %v", want, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit version %d: %v", want, err)
	}
	return event
}

// requireOwnVersionNode holds the node of version number of document, whose ref_key is refKey, to
// the one every version write states: dated at its own row, made by its writer, under the event
// that recorded it, with its part_of stated once.
func requireOwnVersionNode(t *testing.T, database *store.Store, document, refKey string, number int, writer model.Actor, event int64) {
	t.Helper()
	ctx := context.Background()
	own := requirePresentVersionNode(t, database, refKey, number, "after its version write")
	var ownAt time.Time
	if err := database.Pool.QueryRow(ctx, `select created_at from artifact_versions where artifact_id = $1 and number = $2`,
		document, number).Scan(&ownAt); err != nil {
		t.Fatalf("read the row of version %d: %v", number, err)
	}
	if !own.MadeAt.Equal(ownAt) || !own.MadeBy.SameAs(writer) || own.SourceEvent != event {
		t.Errorf("the node of version %d was made at %s by %+v under event %d, want at its row's %s by its writer %+v under its event %d",
			number, own.MadeAt, own.MadeBy, own.SourceEvent, ownAt, writer, event)
	}
	var partOf int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3
	`, graphstore.KindVersion, own.ID, graphstore.TuplePartOf).Scan(&partOf); err != nil {
		t.Fatalf("read the part_of tuples of version %d: %v", number, err)
	}
	if partOf != 1 {
		t.Errorf("the node of version %d states %d part_of tuple(s), want one: a write other than the version's own stated it", number, partOf)
	}
}

// previousReleaseVersion is the version the release before this one writes of the document of
// writePreviousReleaseLink, with no version node.
const previousReleaseVersion = 2

// previousReleaseLink is the fixture this file's tests share (writePreviousReleaseLink): the
// document, its ref_key, the middle paragraph the previous release's version adds a link to, that
// link, and the inner paragraph that version drops, with the link its row held.
type previousReleaseLink struct {
	database     *store.Store
	service      *Service
	document     string
	refKey       string
	middle       string
	added        blockLinkRow
	innerAddress string
	dropped      blockLinkRow
}

// writePreviousReleaseLink writes the document as a build before the store wrote it, with rows of
// every other kind the census reads, runs the backfill, which names its nodes and marks it
// (store_owed_at), and then writes the previous release's version 2 of it
// (indexAsThePreviousRelease), which holds no version node, adds the middle paragraph's link
// naming no node, and leaves the mark standing.
func writePreviousReleaseLink(t *testing.T) previousReleaseLink {
	t.Helper()
	database, service := blockIndexService(t)
	ctx := context.Background()

	const slug = "linked"
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	// Rows of every other kind the census reads, as TestTheBlockParentCensusReachesZeroAfterTheBackfill
	// seeds them, so the census can pass only on agreement and never refuses an empty kind here.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-link-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-link-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	// The document as a build before the store wrote it: its rows, and nothing of the store.
	drainBlockIndex(t, service, "seed")
	takeTheStoreOff(t, database)
	var refKey string
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, document).Scan(&refKey); err != nil {
		t.Fatalf("read the document's ref_key: %v", err)
	}

	report, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	if blocks := backfilledKind(t, report, refs.BlockNodeKind); blocks.Owed != 1 || !blocks.Complete {
		t.Fatalf("the backfill wrote %#v for the blocks, want the one document marked and the cutover complete", blocks)
	}

	inner := holderOf(t, service, document, "dispatch://CORE-2")
	middle := holderOf(t, service, document, "Middle of "+slug)
	dropped := blockLinkRow{blockID: inner}
	if err := database.Pool.QueryRow(ctx, `
		select kind, to_kind, to_id from block_refs where artifact_id = $1 and block_id = $2
	`, document, inner).Scan(&dropped.kind, &dropped.toKind, &dropped.toID); err != nil {
		t.Fatalf("read the inner paragraph's link: %v", err)
	}
	added := blockLinkRow{blockID: middle, kind: dropped.kind, toKind: dropped.toKind, toID: dropped.toID}
	innerAddress := refKey + "#" + inner

	// The previous release's version 2, written after the backfill: the innermost callout and its
	// paragraph go, and the middle paragraph starts citing CORE-2. Its index pass deletes the
	// dropped blocks' rows and the inner paragraph's link row, and inserts the middle paragraph's
	// link naming no node.
	indexAsThePreviousRelease(t, database, service, document, func(tree *pmdoc.Node) *pmdoc.Node {
		if !cutInnermostHolding(tree, "callout", "Inner of "+slug) {
			t.Fatalf("no callout of %s holds %q, so the edit drops no block", slug, "Inner of "+slug)
		}
		replaceText(tree, "Middle of "+slug+".", "Middle of "+slug+", after dispatch://CORE-2.")
		return tree
	}, []blockLinkRow{dropped}, []blockLinkRow{added})
	if after := holderOf(t, service, document, "dispatch://CORE-2"); after != middle {
		t.Fatalf("the edit moved the mention to block %s, want the middle paragraph %s: the room minted new ids and this test proves nothing", after, middle)
	}
	if rows := blockRowsOf(t, database, document, inner); rows != 0 {
		t.Fatalf("the inner paragraph keeps %d row(s) after the previous release's version, want its row dropped: the edit dropped nothing and this test proves nothing", rows)
	}
	if state := nodeState(t, database, innerAddress); state != graphstore.StateLive {
		t.Fatalf("the inner paragraph's node is %q after the previous release dropped it, want it left live, as that release leaves it", state)
	}
	var marked int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from document_block_versions where artifact_id = $1 and store_owed_at is not null`, document).Scan(&marked); err != nil || marked != 1 {
		t.Fatalf("the document holds the backfill's mark %d time(s) (%v) after the previous release's version, want it kept", marked, err)
	}
	return previousReleaseLink{database: database, service: service, document: document, refKey: refKey, middle: middle, added: added,
		innerAddress: innerAddress, dropped: dropped}
}

// requirePresentVersionNode answers the node of version number of the document refKey addresses,
// read through the store, and fails the test, naming the version, unless the store holds it
// present.
func requirePresentVersionNode(t *testing.T, database *store.Store, refKey string, number int, when string) graphstore.Node {
	t.Helper()
	address := graphstore.VersionID(refKey, number)
	node, stands, err := graphstore.ReadNode(context.Background(), database.Pool, graphstore.KindVersion, address)
	if err != nil {
		t.Fatalf("read the node of version %d (%s): %v", number, address, err)
	}
	if !stands || node.State == graphstore.StateGone {
		t.Errorf("the store holds no present node of version %d (%s) %s (held %t, state %q): the drain left the version with no node",
			number, address, when, stands, node.State)
	}
	return node
}

// requireNoCensusDifference takes the store census and fails the test on each difference it
// reads, and on its refusal.
func requireNoCensusDifference(t *testing.T, database *store.Store, when string) {
	t.Helper()
	census := takeStoreCensus(t, database)
	for _, kind := range census.Kinds {
		for _, difference := range append(append([]graphcensus.Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("the census reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	if census.Refused() {
		t.Fatalf("the census refused with %d difference(s) %s, want zero", census.Differences(), when)
	}
}

// cutInnermostHolding removes from the tree under node the innermost block of type kind whose
// text holds words, and reports whether it removed one.
func cutInnermostHolding(node *pmdoc.Node, kind, words string) bool {
	for index, child := range node.Children {
		if !nodeHoldsText(child, words) {
			continue
		}
		if cutInnermostHolding(child, kind, words) {
			return true
		}
		if child.Type == kind {
			node.Children = slices.Delete(node.Children, index, index+1)
			return true
		}
		return false
	}
	return false
}

// blockRowsOf is how many document_blocks rows document holds of block.
func blockRowsOf(t *testing.T, database *store.Store, document, block string) int {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from document_blocks where artifact_id = $1 and block_id = $2
	`, document, block).Scan(&rows); err != nil {
		t.Fatalf("count the rows of block %s in %s: %v", block, document, err)
	}
	return rows
}

// requireDroppedBlockGone holds the block at address, which a version of the release before this
// one dropped, to what this build's drain of its document states of it: its node gone, stated by
// a state tuple as a block a version drops is, and link, the link the block held, retracted.
func requireDroppedBlockGone(t *testing.T, database *store.Store, address string, link blockLinkRow) {
	t.Helper()
	ctx := context.Background()
	if state := nodeState(t, database, address); state != graphstore.StateGone {
		t.Errorf("the node of the dropped block %s is %q after this build's drain, want it gone: the previous release dropped it", address, state)
	}
	var newestState string
	if err := database.Pool.QueryRow(ctx, `
		select to_id from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and asserted
		order by seq desc limit 1
	`, refs.BlockNodeKind, address, graphstore.AttributeState).Scan(&newestState); err != nil || newestState != graphstore.StateGone {
		t.Errorf("the newest state the log holds for %s is %q (%v), want %q, as a block a version drops is stated",
			address, newestState, err, graphstore.StateGone)
	}
	var asserted bool
	if err := database.Pool.QueryRow(ctx, `
		select asserted from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
		order by seq desc limit 1
	`, refs.BlockNodeKind, address, link.kind, link.toKind, link.toID).Scan(&asserted); err != nil || asserted {
		t.Errorf("the newest tuple of %s %s %s:%s asserts=%t (%v), want the link retracted",
			address, link.kind, link.toKind, link.toID, asserted, err)
	}
}
