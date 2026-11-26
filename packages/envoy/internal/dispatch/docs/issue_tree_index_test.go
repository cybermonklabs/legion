package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The values the block index states of every block beside its parent, spelled out here rather
// than read off the writer, so a writer that renamed one is read as one that wrote nothing, and
// the size it states of a plan node whose opening line writes one.
const (
	treeRank      = "rank"
	treeText      = "text"
	treePlacement = "placement"
	treeSize      = "size"
)

// treeKinds are the tuple kinds the issue tree's renderer reads of a block (R15): its parent, its
// rank, its text and its placement, each dated at the pass that stated it.
var treeKinds = []string{graphstore.TuplePartOf, treeRank, treeText, treePlacement}

// The spec of the tree test, at two versions. Two callouts each open with a heading and hold
// typed blocks; version 2 moves the block mover out of the first under the second, after the block
// already there, and changes no character of it, so its text sum is the one stored and the index
// writes no document_blocks row for it. The block that stays cites another issue, so the document
// holds a block_refs row the drain must leave as it is.
const (
	treeVersion1 = "::::callout{#left kind=\"note\" title=\"\"}\n## The first heading\n\n" +
		":::callout{#mover kind=\"note\" title=\"\"}\nThe words that move.\n:::\n\n" +
		":::callout{#stays kind=\"note\" title=\"\"}\nThe words that stay cite dispatch://CORE-2.\n:::\n\n::::\n\n" +
		"::::callout{#right kind=\"note\" title=\"\"}\n## The second heading\n\n" +
		":::callout{#resident kind=\"note\" title=\"\"}\nThe words already here.\n:::\n\n::::\n"
	treeVersion2 = "::::callout{#left kind=\"note\" title=\"\"}\n## The first heading\n\n" +
		":::callout{#stays kind=\"note\" title=\"\"}\nThe words that stay cite dispatch://CORE-2.\n:::\n\n::::\n\n" +
		"::::callout{#right kind=\"note\" title=\"\"}\n## The second heading\n\n" +
		":::callout{#resident kind=\"note\" title=\"\"}\nThe words already here.\n:::\n\n" +
		":::callout{#mover kind=\"note\" title=\"\"}\nThe words that move.\n:::\n\n::::\n"
	treeVerification = "::::callout{#gate kind=\"note\" title=\"\"}\n## What proves it\n\n" +
		":::callout{#first-check kind=\"note\" title=\"\"}\nThe first check.\n:::\n\n" +
		":::callout{#second-check kind=\"note\" title=\"\"}\nThe second check.\n:::\n\n::::\n"
	// treePlan is an issue's plan: one plan node whose opening line writes its size and one whose
	// line writes none.
	treePlan = "## The plan\n\n:::plan_task{#sized size=\"1h30m\"}\nThe task that says how big it is.\n:::\n\n" +
		":::plan_task{#unsized}\nThe task that says nothing of its size.\n:::\n"
)

// treeDocument creates a markdown document of issue, the issue's spec where primary is set and the
// document of role where role is not empty, its live text seeded with markdown, and writes its
// first version through the real version write path.
func treeDocument(t *testing.T, database *store.Store, service *Service, issue, slug string, primary bool, role, markdown string) (string, VersionResult) {
	t.Helper()
	var id string
	var stored *string
	if role != "" {
		stored = &role
	}
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
		values ($1, 'CORE', $2, $2 || '.md', 'doc', $3, $4, '{"kind":"user","id":"alice"}')
		returning id::text
	`, issue, slug, primary, stored).Scan(&id); err != nil {
		t.Fatalf("create document %s: %v", slug, err)
	}
	seedServiceText(t, service, id, markdown)
	version, _ := writeVersionThroughTheRealPath(t, service, id, "The first version")
	settle(t, database, id)
	return id, version
}

// treeValue is a block's current value of attribute, and whether it holds one.
func treeValue(t *testing.T, database *store.Store, address, attribute string) (string, bool) {
	t.Helper()
	value, held, err := graphstore.CurrentValue(context.Background(), database.Pool, refs.BlockNodeKind, address, attribute)
	if err != nil {
		t.Fatalf("read the %s of %s: %v", attribute, address, err)
	}
	return value, held
}

// projectedParent is document_blocks.parent_block_id of one block, "" where it is null.
func projectedParent(t *testing.T, database *store.Store, artifactID, blockID string) string {
	t.Helper()
	var parent *string
	if err := database.Pool.QueryRow(context.Background(),
		`select parent_block_id from document_blocks where artifact_id = $1 and block_id = $2`, artifactID, blockID).Scan(&parent); err != nil {
		t.Fatalf("read the projected parent of %s: %v", blockID, err)
	}
	if parent == nil {
		return ""
	}
	return *parent
}

// requireTheTreeOfTheVersion holds every block of the document's latest version to the tree that
// version gives it: its current part_of is the block holding it, or for a block at the top the
// issue (an issue's spec, Verification or plan document, as every document of these tests is), and
// document_blocks.parent_block_id projects it; the children of each parent stand in increasing
// rank, compared byte for byte, every rank of the document's band; and each block holds its whole
// slice of the version as its text and the placement text. moved names a block whose rank a pass
// that wrote no row of it must have moved, with the rank it held before, so a failure says which.
func requireTheTreeOfTheVersion(t *testing.T, database *store.Store, service *Service, artifactID, issue string, band byte, moved map[string]string) {
	t.Helper()
	ctx := context.Background()
	var refKey string
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, artifactID).Scan(&refKey); err != nil {
		t.Fatalf("read the ref_key of %s: %v", artifactID, err)
	}
	markdown, blocks, err := service.TextWithBlocks(ctx, artifactID)
	if err != nil {
		t.Fatalf("read %s: %v", artifactID, err)
	}
	nodes, placed, parents := refs.PlaceBlocks(refKey, blocks)
	if len(placed) < 4 {
		t.Fatalf("the version of %s places %d blocks, want a document of nested blocks", refKey, len(placed))
	}
	children := map[int][]int{}
	for index, block := range placed {
		node := nodes[block]
		address := refKey + "#" + node.ID
		wantKind, wantID, wantProjected := graphstore.KindIssue, issue, ""
		if parents[index] >= 0 {
			wantKind, wantID, wantProjected = refs.BlockNodeKind, refKey+"#"+nodes[parents[index]].ID, nodes[parents[index]].ID
		}
		if held := currentPartOf(t, database, address); held.ToKind != wantKind || held.ToID != wantID {
			t.Errorf("block %s is part of %s/%s, want %s/%s, which its version's tree gives it", address, held.ToKind, held.ToID, wantKind, wantID)
		}
		if got := projectedParent(t, database, artifactID, node.ID); got != wantProjected {
			t.Errorf("block %s's parent_block_id is %q, want %q", address, got, wantProjected)
		}
		if text, _ := treeValue(t, database, address, treeText); text != markdown[node.From:node.To] {
			t.Errorf("block %s's text value is %q, want its slice of the version %q", address, text, markdown[node.From:node.To])
		}
		if placement, _ := treeValue(t, database, address, treePlacement); placement != "text" {
			t.Errorf("block %s's placement is %q, want text", address, placement)
		}
		children[parents[index]] = append(children[parents[index]], block)
	}
	for parent, held := range children {
		previous := ""
		for position, block := range held {
			address := refKey + "#" + nodes[block].ID
			rank, ok := treeValue(t, database, address, treeRank)
			switch {
			case !ok || rank == "" || rank[0] != band:
				t.Errorf("block %s is ranked %q (held %t), want a rank of band %c", address, rank, ok, band)
			case rank <= previous:
				next := "the band's end"
				if position+1 < len(held) {
					next, _ = treeValue(t, database, refKey+"#"+nodes[held[position+1]].ID, treeRank)
				}
				if kept, wasMoved := moved[nodes[block].ID]; wasMoved && kept == rank {
					t.Errorf("block %s kept its rank %q from before its move, and its version gives it a rank above %q and below %s",
						address, rank, previous, next)
				} else {
					t.Errorf("block %s is ranked %q, at or below its previous sibling's %q under parent %d", address, rank, previous, parent)
				}
			}
			previous = rank
		}
	}
}

// asTheReleaseBeforeLeftIt puts the document's blocks back in the shape the block index of the
// release before this one left them: no rank, text, placement or size value, every top-level
// block part of the document's own node, dated at the version and stated by its writer, and no
// record of a pass of this build's rule on the document's row (stated_version and stated_rule
// null), while its store_version stands at its version. That is what a database holds for a
// document an earlier build indexed, and what the backfill's marking must bring to this release's
// tree.
func asTheReleaseBeforeLeftIt(t *testing.T, database *store.Store, artifactID, issue string) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		var refKey string
		var at time.Time
		if err := tx.QueryRow(ctx, `
			select a.ref_key, v.created_at from artifacts a
			join artifact_versions v on v.artifact_id = a.id
			where a.id = $1 order by v.number desc limit 1
		`, artifactID).Scan(&refKey, &at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			delete from graph_tuples where from_kind = $1 and from_id like $2 || '#%'
			  and (kind = any($3) or (kind = $4 and to_kind = $5 and to_id = $6))
		`, refs.BlockNodeKind, refKey, []string{treeRank, treeText, treePlacement, treeSize},
			graphstore.TuplePartOf, graphstore.KindIssue, issue); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			select node_id from document_blocks where artifact_id = $1 and parent_block_id is null order by block_id
		`, artifactID)
		if err != nil {
			return err
		}
		tops, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		var parents []graphstore.Tuple
		for _, address := range tops {
			parents = append(parents, graphstore.Tuple{
				FromKind: refs.BlockNodeKind, FromID: address, Kind: graphstore.TuplePartOf,
				ToKind: graphstore.KindArtifact, ToID: refKey,
				At: at, Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: true,
			})
		}
		if err := graphstore.Write(ctx, tx, nil, parents); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			update document_block_versions set stated_version = null, stated_rule = null where artifact_id = $1
		`, artifactID)
		return err
	}); err != nil {
		t.Fatalf("put %s back as the release before left it: %v", artifactID, err)
	}
}

// drainOwed runs ticks of the block-index reconciliation until no document owes its index.
func drainOwed(t *testing.T, database *store.Store, service *Service) {
	t.Helper()
	for range 10 {
		reconcileBlockIndex(t, service, "")
		if owedCount(t, database) == 0 {
			return
		}
	}
	t.Fatalf("after ten ticks %d documents still owe their block index", owedCount(t, database))
}

// owedCount is how many documents owe their block index, by either mark the reconciliation lists
// a document by: a null version, or this release's own mark (store_owed_at), which the backfill
// sets. It counts refs.OwedBlockIndexDebts, the one text both readers of the backlog derive it
// from, at this build's marking rule (refs.BlockIndexMarkingRule), the rule the reconciliation's
// tick and `census --store --sizing` bind to it.
func owedCount(t *testing.T, database *store.Store) int {
	t.Helper()
	var owed int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from (`+refs.OwedBlockIndexDebts+`) owed`, refs.BlockIndexMarkingRule).Scan(&owed); err != nil {
		t.Fatalf("count the documents owed a block index: %v", err)
	}
	return owed
}

// backfillMark is when the document took the backfill's mark, document_block_versions.store_owed_at,
// which only this release reads or clears, nil where it holds none, and whether its version
// stands: the backfill leaves the version alone, since a release before this one reads a null
// version as its own debt and drains it with no value stated.
func backfillMark(t *testing.T, database *store.Store, artifactID string) (markedAt *time.Time, standing bool) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select store_owed_at, version is not null from document_block_versions where artifact_id = $1
	`, artifactID).Scan(&markedAt, &standing); err != nil {
		t.Fatalf("read the backfill's mark on %s: %v", artifactID, err)
	}
	return markedAt, standing
}

// drainedAsTheReleaseBefore stands in for a pod of the release before this one draining the
// document as that release drains a null version, the mark it reads as its own debt: the version
// goes to null, as rebuild-refs sets it, and that release's index pass sets it back to the
// document's latest version, as its fence writes it, and states nothing in the store - no rank, no
// text, no placement. The rows already stand at that version, so its pass rewrites none of them.
func drainedAsTheReleaseBefore(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `update document_block_versions set version = null where artifact_id = $1`, artifactID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into document_block_versions (artifact_id, version, windows_owed_at)
			select $1::uuid, max(number), now() from artifact_versions where artifact_id = $1::uuid and markdown is not null
			on conflict (artifact_id) do update set version = excluded.version,
				windows_owed_at = coalesce(document_block_versions.windows_owed_at, excluded.windows_owed_at)
		`, artifactID)
		return err
	}); err != nil {
		t.Fatalf("drain %s as a pod of the release before does: %v", artifactID, err)
	}
}

// snapshot is one reading of a table's rows for some documents, every column of each, in one
// order, so a reading taken again says whether any row moved.
func snapshot(t *testing.T, database *store.Store, table string, artifactIDs []string) string {
	t.Helper()
	var read string
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*)::text || '|' || coalesce(string_agg(row_to_json(r)::text, '|' order by row_to_json(r)::text), '')
		from `+table+` r where r.artifact_id = any($1::uuid[])
	`, artifactIDs).Scan(&read); err != nil {
		t.Fatalf("read the rows of %s: %v", table, err)
	}
	return read
}

// treeTuplesSince is every tuple of treeKinds appended after seq out of a block of the document.
func treeTuplesSince(t *testing.T, database *store.Store, artifactID string, seq int64) []graphstore.Tuple {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select t.from_id, t.kind, t.to_kind, t.to_id, t.at, t.asserted
		from graph_tuples t join artifacts a on a.id = $1::uuid
		where t.seq > $2 and t.from_kind = $3 and t.from_id like a.ref_key || '#%' and t.kind = any($4)
		order by t.seq
	`, artifactID, seq, refs.BlockNodeKind, treeKinds)
	if err != nil {
		t.Fatalf("read the tree's tuples of %s: %v", artifactID, err)
	}
	stated, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (graphstore.Tuple, error) {
		var tuple graphstore.Tuple
		err := row.Scan(&tuple.FromID, &tuple.Kind, &tuple.ToKind, &tuple.ToID, &tuple.At, &tuple.Asserted)
		return tuple, err
	})
	if err != nil {
		t.Fatalf("read the tree's tuples of %s: %v", artifactID, err)
	}
	return stated
}

// latestVersionAt is the created_at of the document's newest version.
func latestVersionAt(t *testing.T, database *store.Store, artifactID string) time.Time {
	t.Helper()
	var at time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select created_at from artifact_versions where artifact_id = $1 order by number desc limit 1`, artifactID).Scan(&at); err != nil {
		t.Fatalf("read the newest version of %s: %v", artifactID, err)
	}
	return at
}

// lateIndexWait is how long after a version's row is written the reconciliation indexes it, in
// the case of TestTheIssueTreeHoldsTheParentAndRankOfEveryBlockOfTheVersion that reads a pass
// dated at its own time rather than at the version's.
const lateIndexWait = 300 * time.Millisecond

// movingPass is what a block's move is held to (requireTheMove): the version that moved it, its
// row's created_at, the least wait between that row's write and the pass that indexed it, the
// version's writer and the event that recorded it, and the log's end before the pass, past which
// the pass's own tuples stand.
type movingPass struct {
	version   int
	versionAt time.Time
	wait      time.Duration
	writer    model.Actor
	event     int64
	after     int64
}

// requireTheMove holds the move of the block at address of the document from block from to block
// to (D2 of the issue-tree plan, as the graph-model root spec's architect ruled it): its newest
// part_of asserts to and the one before it retracts from, and the two share one time, the moving
// pass's own, which every parent, rank, text and placement that pass appended out of the document's
// blocks carries too, since the pass reads it once, after it holds the owner row and the fence.
// That time is no earlier than the version row's created_at, and later than it by at least the
// wait between the row's write and the pass; both tuples carry the version's writer and its event.
// Each failure names the block and both times.
func requireTheMove(t *testing.T, database *store.Store, artifactID, address, from, to string, pass movingPass) {
	t.Helper()
	history, err := graphstore.History(context.Background(), database.Pool, refs.BlockNodeKind, address, graphstore.TuplePartOf)
	if err != nil {
		t.Fatalf("read the part_of history of block %s: %v", address, err)
	}
	if len(history) < 2 {
		t.Fatalf("block %s's part_of history is %+v, want version %d's retraction of block/%s and assertion of block/%s",
			address, history, pass.version, from, to)
	}
	retracted, asserted := history[len(history)-2], history[len(history)-1]
	if retracted.ToKind != refs.BlockNodeKind || retracted.ToID != from || retracted.Asserted ||
		asserted.ToKind != refs.BlockNodeKind || asserted.ToID != to || !asserted.Asserted {
		t.Errorf("block %s's newest part_of tuples are %+v then %+v, want version %d's retraction of block/%s and assertion of block/%s",
			address, retracted, asserted, pass.version, from, to)
	}
	if !retracted.At.Equal(asserted.At) {
		t.Errorf("block %s's retraction of block/%s is dated %s and its assertion of block/%s %s, want one time, the pass's that indexed version %d",
			address, from, retracted.At, to, asserted.At, pass.version)
	}
	for _, tuple := range []struct {
		what  string
		tuple graphstore.Tuple
	}{{"retraction of block/" + from, retracted}, {"assertion of block/" + to, asserted}} {
		if lag := tuple.tuple.At.Sub(pass.versionAt); lag < pass.wait {
			t.Errorf("block %s's %s is dated %s, %s after version %d's created_at %s, want the pass's own time, no earlier than that row and at least %s after it",
				address, tuple.what, tuple.tuple.At, lag, pass.version, pass.versionAt, pass.wait)
		}
		if tuple.tuple.Who != pass.writer {
			t.Errorf("block %s's %s, dated %s, was stated by %#v, want version %d's writer %#v (its row's created_at %s)",
				address, tuple.what, tuple.tuple.At, tuple.tuple.Who, pass.version, pass.writer, pass.versionAt)
		}
		if tuple.tuple.SourceEvent != pass.event {
			t.Errorf("block %s's %s, dated %s, was caused by event %d, want version %d's own %d (its row's created_at %s)",
				address, tuple.what, tuple.tuple.At, tuple.tuple.SourceEvent, pass.version, pass.event, pass.versionAt)
		}
	}
	stated := treeTuplesSince(t, database, artifactID, pass.after)
	if len(stated) == 0 {
		t.Fatalf("the pass that indexed version %d stated no parent, rank, text or placement, so block %s's move is none of its own", pass.version, address)
	}
	for _, tuple := range stated {
		if !tuple.At.Equal(asserted.At) {
			t.Errorf("the pass that indexed version %d dated its %s tuple of %s at %s and block %s's move at %s, want the pass's one time",
				pass.version, tuple.Kind, tuple.FromID, tuple.At, address, asserted.At)
		}
	}
}

// The issue tree holds the parent and the rank of every block of the version, a block whose text
// no version changed included (c-parent-written; R1 of the issue-tree spec).
//
// A spec is indexed at one version, then at a second that moves a block out of one callout under
// another, after the block already there, and changes no character of it, so the index writes no
// document_blocks row for it and only reconcileBlockParents reaches it. Its part_of after the
// second index is the new holder, and it and the retraction of the old one share one time, the
// second pass's own, which every tuple that pass states carries, no earlier than the second
// version row's created_at, with that version's writer and event (requireTheMove);
// parent_block_id projects the new holder; and it and its new siblings stand in increasing rank.
// The move costs the block nothing else: its changed_version and its document_block_texts rows
// are the first version's, the Keep recorded on it before the move still stands, its change mark
// reads as before, and a citation of it resolves to the same words.
//
// Then the backfill: the spec and a Verification document are put back as the release before
// left them, the backfill's drain marks both owed, the reconciliation drains them as
// RunBlockIndexReconciliation's tick does, and every block holds the parent and the rank its
// version's tree gives it, with no row owed, no block id changed, no block_refs row moved and no
// version added; a second backfill marks nothing. Every tuple of the drain is dated at or after
// clock_timestamp() read just before it, and after its version's created_at. An older pass over
// the spec's blocks after the newer move is refused by the fence, and the newer parent stands.
//
// Last, the reconciliation indexes a version lateIndexWait after a build without the block index
// wrote its row, a version that moves the block back: the pass's retraction of the holder it
// leaves and its assertion of the one it reaches share one time, later than that row's
// created_at by at least the wait, with that version's writer and event; parent_block_id projects
// the new holder, and the block and its new siblings stand in increasing rank.
func TestTheIssueTreeHoldsTheParentAndRankOfEveryBlockOfTheVersion(t *testing.T) {
	ctx := context.Background()
	database, service := blockIndexService(t)
	alice := model.Actor{Kind: "user", ID: "alice"}
	spec, first := treeDocument(t, database, service, "CORE-1", "spec", true, "", treeVersion1)
	mover := blockAddress(t, database, spec, "mover")
	left, right := blockAddress(t, database, spec, "left"), blockAddress(t, database, spec, "right")
	if held := currentPartOf(t, database, mover); held.ToKind != refs.BlockNodeKind || held.ToID != left {
		t.Fatalf("at version 1 block mover is part of %s/%s, want block/%s: the next version must move it", held.ToKind, held.ToID, left)
	}
	if held := currentPartOf(t, database, left); held.ToKind != graphstore.KindIssue || held.ToID != "CORE-1" {
		t.Errorf("at version 1 the spec's top-level block left is part of %s/%s, want the issue CORE-1", held.ToKind, held.ToID)
	}

	// What the move must leave as it is: the Keep a person records on the block, its change mark,
	// the words a citation of it resolves to, and its texts.
	var sum string
	var changed int
	if err := database.Pool.QueryRow(ctx, `
		select text_sha256, changed_version from document_blocks where artifact_id = $1 and block_id = 'mover'
	`, spec).Scan(&sum, &changed); err != nil {
		t.Fatalf("read the moving block's row: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into decision_keeps (artifact_id, block_id, text_sha256, changed_version, kept_by) values ($1, 'mover', $2, $3, 'alice')
	`, spec, sum, changed); err != nil {
		t.Fatalf("record a Keep on the moving block: %v", err)
	}
	keep := snapshot(t, database, "decision_keeps", []string{spec})
	marks, err := refs.BlockChanges(ctx, database.Pool, spec)
	if err != nil {
		t.Fatalf("read the spec's change marks: %v", err)
	}
	target := refs.Target{Kind: refs.BlockNodeKind, ID: mover}
	cited, err := refs.ExcerptedNodes(ctx, database.Pool, alice, []refs.Target{target}, "")
	if err != nil || cited[target].Excerpt == nil || !strings.Contains(cited[target].Excerpt.Text, "The words that move.") {
		t.Fatalf("a citation of the moving block resolves to %+v (%v), want its words", cited[target], err)
	}
	texts := snapshot(t, database, "(select * from document_block_texts where block_id = 'mover')", []string{spec})
	rankBefore, _ := treeValue(t, database, mover, treeRank)

	if _, err := joinedReplaceText(service, spec, treeVersion2, alice); err != nil {
		t.Fatalf("move the block in the live document: %v", err)
	}
	beforeMove := highestTuple(t, database)
	second, secondEvent := writeVersionThroughTheRealPath(t, service, spec, "The move")
	settle(t, database, spec)
	if len(second.Version.Authors) == 0 {
		t.Fatalf("version %d records no author, so its pass's tuples can be held to no writer", second.Version.Number)
	}
	requireTheMove(t, database, spec, mover, left, right, movingPass{
		version: second.Version.Number, versionAt: second.Version.CreatedAt,
		writer: second.Version.Authors[0], event: secondEvent, after: beforeMove,
	})
	if now := currentPartOf(t, database, mover); now.ToID != right {
		t.Errorf("after the move block mover is part of %s, want block/%s", now.ToID, right)
	}
	if got := projectedParent(t, database, spec, "mover"); got != "right" {
		t.Errorf("after the move the moved block's parent_block_id is %q, want right", got)
	}
	requireTheTreeOfTheVersion(t, database, service, spec, "CORE-1", '1', map[string]string{"mover": rankBefore})

	if now := snapshot(t, database, "(select * from document_block_texts where block_id = 'mover')", []string{spec}); now != texts {
		t.Errorf("the move moved the block's document_block_texts rows from %s to %s", texts, now)
	}
	var sumNow string
	var changedNow int
	if err := database.Pool.QueryRow(ctx, `
		select text_sha256, changed_version from document_blocks where artifact_id = $1 and block_id = 'mover'
	`, spec).Scan(&sumNow, &changedNow); err != nil {
		t.Fatalf("read the moved block's row: %v", err)
	}
	if changedNow != first.Version.Number || sumNow != sum {
		t.Errorf("after the move the block's changed_version is %d and its sum %s, want version %d's %d and %s: a parent write cleared the Keep",
			changedNow, sumNow, first.Version.Number, changed, sum)
	}
	if now := snapshot(t, database, "decision_keeps", []string{spec}); now != keep {
		t.Errorf("the move moved the Keep recorded on the block from %s to %s", keep, now)
	}
	if now, err := refs.BlockChanges(ctx, database.Pool, spec); err != nil || now["mover"] != marks["mover"] {
		t.Errorf("the moved block's change mark reads %+v (%v), want the mark before the move %+v", now["mover"], err, marks["mover"])
	}
	if now, err := refs.ExcerptedNodes(ctx, database.Pool, alice, []refs.Target{target}, ""); err != nil ||
		now[target].Excerpt == nil || now[target].Excerpt.Text != cited[target].Excerpt.Text {
		t.Errorf("a citation of the moved block resolves to %+v (%v), want the words it resolved to before, %+v", now[target], err, cited[target])
	}

	// The backfill: documents stored before this work, which no pass of this build's rule has
	// stated, marked by the backfill's marking (markStaleDocuments, markStaleBatch) with their
	// versions as they stood, and indexed by the reconciliation, which clears the mark.
	verification, _ := treeDocument(t, database, service, "CORE-1", "verification-md", false, model.ArtifactRoleVerification, treeVerification)
	documents := []string{spec, verification}
	stood := map[string]int{}
	for _, document := range documents {
		asTheReleaseBeforeLeftIt(t, database, document, "CORE-1")
		version := blockIndexAt(t, database, document)
		if version == nil {
			t.Fatalf("document %s owes its block index before the backfill: the case proves nothing", document)
		}
		stood[document] = *version
	}
	ids, links, versions := snapshot(t, database, "(select artifact_id, block_id from document_blocks)", documents),
		snapshot(t, database, "block_refs", documents), snapshot(t, database, "(select artifact_id, number from artifact_versions)", documents)
	var clock time.Time
	if err := database.Pool.QueryRow(ctx, `select clock_timestamp()`).Scan(&clock); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	before := highestTuple(t, database)
	report, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("run the backfill: %v", err)
	}
	if blocks := backfilledKind(t, report, refs.BlockNodeKind); blocks.Owed != int64(len(documents)) || !blocks.Complete {
		t.Errorf("the backfill marked %d documents owed (complete %t), want the %d stored", blocks.Owed, blocks.Complete, len(documents))
	}
	for _, document := range documents {
		markedAt, _ := backfillMark(t, database, document)
		if version := blockIndexAt(t, database, document); markedAt == nil || version == nil || *version != stood[document] {
			t.Errorf("after the backfill document %s holds the backfill's mark %v at version %s, want it marked and its version %d as it stood: "+
				"the marking owes it this release's index by its own mark and leaves the version a release before this one reads alone",
				document, markedAt, standsText(version), stood[document])
		}
	}
	drainOwed(t, database, service)
	requireTheTreeOfTheVersion(t, database, service, spec, "CORE-1", '1', nil)
	requireTheTreeOfTheVersion(t, database, service, verification, "CORE-1", '3', nil)
	if now := snapshot(t, database, "(select artifact_id, block_id from document_blocks)", documents); now != ids {
		t.Errorf("the reconciliation moved the documents' block ids from %s to %s", ids, now)
	}
	if now := snapshot(t, database, "block_refs", documents); now != links || !strings.Contains(links, "CORE-2") {
		t.Errorf("the reconciliation moved the block_refs rows from %s to %s, which must hold the spec's citation of CORE-2", links, now)
	}
	if now := snapshot(t, database, "(select artifact_id, number from artifact_versions)", documents); now != versions {
		t.Errorf("the reconciliation moved the documents' versions from %s to %s", versions, now)
	}
	if now := snapshot(t, database, "(select * from document_block_texts where block_id = 'mover')", []string{spec}); now != texts {
		t.Errorf("the reconciliation moved the block's document_block_texts rows from %s to %s", texts, now)
	}
	for _, document := range documents {
		stated := treeTuplesSince(t, database, document, before)
		if len(stated) == 0 {
			t.Errorf("the reconciliation stated no parent, rank, text or placement of document %s", document)
		}
		versionAt := latestVersionAt(t, database, document)
		for _, tuple := range stated {
			if tuple.At.Before(clock) || !tuple.At.After(versionAt) {
				t.Errorf("the reconciliation's %s tuple of %s is dated %s, want at or after the clock read before it, %s, and after its version's %s",
					tuple.Kind, tuple.FromID, tuple.At, clock, versionAt)
			}
		}
	}
	again, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("run the backfill a second time: %v", err)
	}
	if blocks := backfilledKind(t, again, refs.BlockNodeKind); blocks.Owed != 0 || !blocks.Complete {
		t.Errorf("the second backfill marked %d documents owed, want none: the reconciliation stated each by this build's rule", blocks.Owed)
	}
	if owed := owedCount(t, database); owed != 0 {
		t.Errorf("after the second backfill %d documents owe their block index, want none", owed)
	}

	// An older pass over the same blocks after the newer move: the fence refuses it.
	stale := highestTuple(t, database)
	staleTx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the older pass: %v", err)
	}
	defer staleTx.Rollback(ctx)
	olderMarkdown, olderBlocks := renderedVersion(t, treeVersion1)
	_, won, write, _, err := refs.IndexBlocks(ctx, staleTx, spec, refs.OneVersion(spec, first.Version.Number, first.Version.CreatedAt), olderMarkdown, olderBlocks, nil, nil, "", refs.TypedFacts{})
	if err != nil {
		t.Fatalf("run the older pass: %v", err)
	}
	if err := graphstore.Caused(ctx, staleTx, write, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the older pass stated: %v", err)
	}
	if err := staleTx.Commit(ctx); err != nil {
		t.Fatalf("commit the older pass: %v", err)
	}
	if won || tuplesSince(t, database, spec, stale) != 0 {
		t.Errorf("the older pass at version %d stood (%t) or appended tuples over version %d, want it refused by the fence",
			first.Version.Number, won, second.Version.Number)
	}
	if now := currentPartOf(t, database, mover); now.ToID != right {
		t.Errorf("after the older pass block mover is part of %s, want the newer parent block/%s", now.ToID, right)
	}

	// The reconciliation indexes a version some time after its row was written. A build without
	// the block index writes version 3 of the spec, by another writer and with its event, which
	// moves mover out of right back under left, before stays, and changes no character of it; the
	// reconciliation reaches it lateIndexWait later. Its pass dates the move at its own time, so
	// the retraction of right and the assertion of left are one time later than the row's
	// created_at by at least that wait, and parent_block_id and the ranks follow the move.
	rankMoved, _ := treeValue(t, database, mover, treeRank)
	if _, err := joinedReplaceText(service, spec, treeVersion1, alice); err != nil {
		t.Fatalf("move the block back in the live document: %v", err)
	}
	settle(t, database, spec)
	live, _, err := service.TextWithBlocks(ctx, spec)
	if err != nil {
		t.Fatalf("read the spec's live text: %v", err)
	}
	bob := model.Actor{Kind: "user", ID: "bob"}
	late := movingPass{wait: lateIndexWait, writer: bob}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select $1, max(number) + 1, $2, '[{"kind":"user","id":"bob"}]' from artifact_versions where artifact_id = $1
			returning number, created_at
		`, spec, live).Scan(&late.version, &late.versionAt); err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{"artifact_id": spec, "version": map[string]any{"number": late.version}})
		if err != nil {
			return err
		}
		late.event, err = documentEvent(ctx, tx, "artifact.version", payload)
		return err
	}); err != nil {
		t.Fatalf("write the spec's next version as a build without the block index writes it: %v", err)
	}
	time.Sleep(lateIndexWait)
	late.after = highestTuple(t, database)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("the tick after version %d indexed %d documents, want the spec", late.version, tick.Documents)
	}
	requireTheMove(t, database, spec, mover, right, left, late)
	if got := projectedParent(t, database, spec, "mover"); got != "left" {
		t.Errorf("after version %d the moved block's parent_block_id is %q, want left", late.version, got)
	}
	requireTheTreeOfTheVersion(t, database, service, spec, "CORE-1", '1', map[string]string{"mover": rankMoved})
	// That build writes no version node, and its version leaves the spec owed by the null version
	// alone (the backfill's mark was drained before it): the tick's pass, which won on the spec it
	// listed as owed, writes the version's node, made by its first author at its row's created_at.
	var refKey string
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, spec).Scan(&refKey); err != nil {
		t.Fatalf("read the spec's ref_key: %v", err)
	}
	node := requirePresentVersionNode(t, database, refKey, late.version, "after the tick that indexed it")
	if !node.MadeBy.SameAs(bob) || !node.MadeAt.Equal(late.versionAt) {
		t.Errorf("the node of version %d was made at %s by %+v, want at its row's %s by its first author %+v",
			late.version, node.MadeAt, node.MadeBy, late.versionAt, bob)
	}
}

// renderedVersion is the canonical markdown of markdown and its blocks, as the block index takes
// them from a render.
func renderedVersion(t *testing.T, markdown string) (string, []model.ArtifactBlock) {
	t.Helper()
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("parse a version: %v", err)
	}
	rendered, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		t.Fatalf("render a version: %v", err)
	}
	return rendered, offsetBlocks(offsets, nil)
}

// The reconciliation's pass dates every parent, rank, text and placement it states at its own
// time, never at the older version's it indexes, and so does the request's pass: each reads the
// database's clock once it holds the owner row and the fence, and dates every such tuple of the
// pass there (D2 of the issue-tree plan, as the graph-model root spec's architect ruled it). The
// document's version is written by SQL, as a build without the block index writes one, so the
// reconciliation is what indexes it, after clock_timestamp() is read; the request's tuples are
// then one time, at or after its version's created_at.
func TestTheReconciliationDatesItsTuplesAtItsOwnTime(t *testing.T) {
	database, service := blockIndexService(t)
	document := createOwedDocument(t, database, service, "CORE-1", "", "dated", raceVersion2, "")
	settle(t, database, document)
	versionAt := latestVersionAt(t, database, document)
	var clock time.Time
	if err := database.Pool.QueryRow(context.Background(), `select clock_timestamp()`).Scan(&clock); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	before := highestTuple(t, database)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("the tick indexed %d documents, want the owed one", tick.Documents)
	}
	stated := treeTuplesSince(t, database, document, before)
	kinds := map[string]bool{}
	for _, tuple := range stated {
		kinds[tuple.Kind] = true
		if tuple.At.Before(clock) || !tuple.At.After(versionAt) {
			t.Errorf("the reconciliation's %s tuple of %s is dated %s, want its own time, at or after %s and after the version's %s",
				tuple.Kind, tuple.FromID, tuple.At, clock, versionAt)
		}
	}
	for _, kind := range treeKinds {
		if !kinds[kind] {
			t.Errorf("the reconciliation stated no %s tuple of the document, so its dating proves nothing of that kind: %+v", kind, stated)
		}
	}

	if _, err := joinedReplaceText(service, document, raceVersion1, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("edit the document: %v", err)
	}
	request := highestTuple(t, database)
	second, _ := writeVersionThroughTheRealPath(t, service, document, "The request's version")
	stated = treeTuplesSince(t, database, document, request)
	if len(stated) == 0 {
		t.Fatalf("the request's pass stated no parent, rank, text or placement")
	}
	for _, tuple := range stated {
		if !tuple.At.Equal(stated[0].At) || tuple.At.Before(second.Version.CreatedAt) {
			t.Errorf("the request's %s tuple of %s is dated %s, want the pass's one time (%s), at or after the version's created_at %s",
				tuple.Kind, tuple.FromID, tuple.At, stated[0].At, second.Version.CreatedAt)
		}
	}
}

// A request pass that waits at the document's owner row behind a reconciliation of the same
// document dates its tuples after the reconciliation's, so a tuple's at is ordered as its seq is
// (D2, as the graph-model root spec's architect ruled it). The request's transaction begins first,
// so its own now() is earlier than anything the reconciliation reads; the reconciliation then takes
// the owner row, writes, and is held at its commit (afterBlockIndexWrite) while the request, on a
// goroutine, queues at that row. The request's pass is the one the request path runs once it holds
// the row (indexRenderedVersion over a version the request writes). Released, the reconciliation
// commits and the request runs on. Read in seq order, the two passes' parents, ranks, texts and
// placements never go back in time, and each pass dates its own at one time.
func TestAPassThatWaitedForTheOwnerRowDatesItsTuplesAfterThePassItWaitedFor(t *testing.T) {
	ctx := context.Background()
	database, service := blockIndexService(t)
	document := createOwedDocument(t, database, service, "CORE-1", "", "raced", raceVersion1, "")
	settle(t, database, document)
	before := highestTuple(t, database)

	requestTx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the request's transaction: %v", err)
	}
	defer requestTx.Rollback(ctx)
	var began time.Time
	if err := requestTx.QueryRow(ctx, `select now()`).Scan(&began); err != nil {
		t.Fatalf("read the request's transaction time: %v", err)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	service.afterBlockIndexWrite = func(artifactID string) error {
		if artifactID == document {
			close(entered)
			<-release
		}
		return nil
	}
	defer func() { service.afterBlockIndexWrite = nil }()
	ticked := make(chan error, 1)
	go func() {
		_, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, liveAheadDocuments{})
		ticked <- err
	}()
	select {
	case <-entered:
	case <-time.After(fenceWait):
		t.Fatalf("the reconciliation did not reach its commit within %s", fenceWait)
	}

	markdown, blocks := renderedVersion(t, raceVersion2)
	indexed := make(chan error, 1)
	go func() {
		indexed <- func() error {
			if _, _, err := lockArtifactOwner(ctx, requestTx, document); err != nil {
				return fmt.Errorf("take the owner row: %w", err)
			}
			var versionAt time.Time
			if err := requestTx.QueryRow(ctx, `
				insert into artifact_versions (artifact_id, number, markdown, authors)
				values ($1, 2, $2, '[{"kind":"user","id":"alice"}]') returning created_at
			`, document, markdown).Scan(&versionAt); err != nil {
				return fmt.Errorf("write version 2: %w", err)
			}
			_, write, err := service.indexRenderedVersion(ctx, requestTx, document, refs.OneVersion(document, 2, versionAt), refs.OneVersion(document, 2, versionAt), false, markdown, blocks, nil, nil)
			if err != nil {
				return fmt.Errorf("index version 2: %w", err)
			}
			if err := graphstore.Caused(ctx, requestTx, write, graphstore.NoEvent); err != nil {
				return fmt.Errorf("append what version 2's pass stated: %w", err)
			}
			return requestTx.Commit(ctx)
		}()
	}()
	waitedAtTheRow := false
	for deadline := time.Now().Add(fenceWait); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var waiting int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and state = 'active'
		`).Scan(&waiting); err != nil {
			t.Fatalf("read whether the request waits at the owner row: %v", err)
		}
		if waiting > 0 {
			waitedAtTheRow = true
			break
		}
	}
	close(release)
	if err := <-ticked; err != nil {
		t.Fatalf("the reconciliation: %v", err)
	}
	if err := <-indexed; err != nil {
		t.Fatalf("the request's pass: %v", err)
	}
	if !waitedAtTheRow {
		t.Fatalf("the request's pass never waited at the owner row the reconciliation held, so the race was not run")
	}

	rows, err := database.Pool.Query(ctx, `
		select t.seq, t.from_id, t.kind, t.at
		from graph_tuples t join artifacts a on a.id = $1::uuid
		where t.seq > $2 and t.from_kind = $3 and t.from_id like a.ref_key || '#%' and t.kind = any($4)
		order by t.seq
	`, document, before, refs.BlockNodeKind, treeKinds)
	if err != nil {
		t.Fatalf("read the two passes' tuples: %v", err)
	}
	type dated struct {
		seq        int64
		from, kind string
		at         time.Time
	}
	stated, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (dated, error) {
		var tuple dated
		err := row.Scan(&tuple.seq, &tuple.from, &tuple.kind, &tuple.at)
		return tuple, err
	})
	if err != nil {
		t.Fatalf("read the two passes' tuples: %v", err)
	}
	times := map[time.Time]bool{}
	for index, tuple := range stated {
		times[tuple.at] = true
		if index > 0 && tuple.at.Before(stated[index-1].at) {
			t.Errorf("tuple %d (%s of %s) is dated %s, before tuple %d's %s: a pass that waited at the owner row dated its tuples before the pass it waited for",
				tuple.seq, tuple.kind, tuple.from, tuple.at, stated[index-1].seq, stated[index-1].at)
		}
	}
	if len(times) != 2 {
		t.Errorf("the two passes dated their tuples at %d distinct times, want one each: %v", len(times), times)
	}
	if last := stated[len(stated)-1]; !last.at.After(began) {
		t.Errorf("the request's last tuple is dated %s, not after its transaction began at %s", last.at, began)
	}
}

// The backfill marks each stored document no pass of this build's rule has stated owed its block
// index once, and the census of the rank and the text of every block and of the size of every plan
// node reads zero differences once the reconciliation has indexed them (graphcensus's block_rank,
// block_text and block_size).
//
// Four documents of an issue - its spec, its Verification document, its plan and an upload - are
// written through the real version write, whose index states the size the plan's first plan node
// writes and none for the second, which writes none, and are then put back as the release before
// left them: store_version at the version, and no record of a pass of this rule. The first
// backfill marks all four with this build's mark, store_owed_at, reports them under the block
// kind's Owed and leaves each one's version standing, and a second before the reconciliation runs
// marks nothing and moves no mark already standing. A pod of the release before then drains the
// Verification document as it drains a null version, the mark it reads as its own debt, and states
// none of its values: the document is still owed by this build's mark, which that release never
// reads, and while every document holds it the census reads no difference of the rank, the text
// and the size kinds, whose blocks are on neither side until the reconciliation states them. After
// the reconciliation has indexed them a third backfill marks nothing and leaves nothing owed, every
// block of the Verification document holds its parent, its rank, its text and its placement, and
// the plan's first plan node holds its size again and the second none. The census then reads rows
// and nodes of the block, the rank, the text and the size kinds and zero differences of any kind.
// It can fail: a block whose text value is retracted, one given a rank of another document's band,
// and a plan node whose size value is retracted are each one difference of their kind, named by
// the block.
func TestTheBackfillMarksEachUnstatedDocumentOnceAndTheCensusReadsZeroAfterIt(t *testing.T) {
	ctx := context.Background()
	database, service := blockIndexService(t)
	spec, _ := treeDocument(t, database, service, "CORE-1", "spec", true, "", treeVersion1)
	verification, _ := treeDocument(t, database, service, "CORE-1", "verification-md", false, model.ArtifactRoleVerification, treeVerification)
	plan, _ := treeDocument(t, database, service, "CORE-1", "plan-md", false, model.ArtifactRolePlan, treePlan)
	upload, _ := treeDocument(t, database, service, "CORE-1", "notes-md", false, "", treeVerification)
	sized, unsized := blockAddress(t, database, plan, "sized"), blockAddress(t, database, plan, "unsized")
	requireSizes := func(when string) {
		t.Helper()
		if size, held := treeValue(t, database, sized, treeSize); !held || size != "90" {
			t.Errorf("%s the plan node sized holds the size %q (held %t), want 90, the minutes its opening line's 1h30m writes", when, size, held)
		}
		if size, held := treeValue(t, database, unsized, treeSize); held {
			t.Errorf("%s the plan node unsized holds the size %q, want none: its opening line writes none", when, size)
		}
	}
	requireSizes("after the version write")
	documents := []string{spec, verification, plan, upload}
	for _, document := range documents {
		asTheReleaseBeforeLeftIt(t, database, document, "CORE-1")
	}

	first, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("run the backfill: %v", err)
	}
	if blocks := backfilledKind(t, first, refs.BlockNodeKind); blocks.Owed != int64(len(documents)) || !blocks.Complete {
		t.Errorf("the first backfill marked %d documents owed (complete %t), want all %d", blocks.Owed, blocks.Complete, len(documents))
	}
	if owed := owedCount(t, database); owed != len(documents) {
		t.Errorf("after the first backfill %d documents owe their block index, want %d", owed, len(documents))
	}
	marks := map[string]time.Time{}
	for _, document := range documents {
		markedAt, standing := backfillMark(t, database, document)
		if markedAt == nil || !standing {
			t.Errorf("after the first backfill document %s holds the backfill's mark %v and its version stands %t, want both", document, markedAt, standing)
			continue
		}
		marks[document] = *markedAt
	}
	second, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("run the backfill a second time: %v", err)
	}
	if blocks := backfilledKind(t, second, refs.BlockNodeKind); blocks.Owed != 0 {
		t.Errorf("the second backfill marked %d documents owed, want none: each holds the first run's mark", blocks.Owed)
	}
	for document, was := range marks {
		if markedAt, _ := backfillMark(t, database, document); markedAt == nil || !markedAt.Equal(was) {
			t.Errorf("after the second backfill document %s holds the mark %v, want the first run's, %s, kept as it stood", document, markedAt, was)
		}
	}

	// A pod of the release before drains the Verification document and states nothing.
	drainedAsTheReleaseBefore(t, database, verification)
	if markedAt, _ := backfillMark(t, database, verification); markedAt == nil {
		t.Errorf("after a pod of the release before drained the Verification document it holds no backfill mark: " +
			"nothing of this build owes its blocks the rank, the text and the placement that pod never stated")
	}
	if owed := owedCount(t, database); owed != len(documents) {
		t.Errorf("after a pod of the release before drained one document %d documents owe their block index, want all %d", owed, len(documents))
	}
	if _, held := treeValue(t, database, blockAddress(t, database, verification, "first-check"), treeText); held {
		t.Errorf("the Verification document's block first-check holds a text value before this build's reconciliation ran, so this case proves nothing")
	}
	if _, held := treeValue(t, database, sized, treeSize); held {
		t.Errorf("the plan node sized holds a size value before this build's reconciliation ran, so this case proves nothing")
	}
	owing := treeCensus(t, database)
	for _, name := range []string{"block_rank", "block_text", "block_size"} {
		if kind := treeCensusKind(t, owing, name); kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
			t.Errorf("while every document holds the backfill's mark the census reads %d row and %d node difference(s) of kind %s, %+v, "+
				"want none: a block of a document owed its index is on neither side", kind.RowDifferences, kind.NodeDifferences, name, kind.ReportedRows)
		}
	}
	drainOwed(t, database, service)
	requireTheTreeOfTheVersion(t, database, service, verification, "CORE-1", '3', nil)
	requireSizes("after the reconciliation")
	third, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("run the backfill a third time: %v", err)
	}
	if blocks := backfilledKind(t, third, refs.BlockNodeKind); blocks.Owed != 0 || owedCount(t, database) != 0 {
		t.Errorf("the third backfill marked %d documents owed and %d owe their index, want none of either",
			blocks.Owed, owedCount(t, database))
	}
	uploadRefKey := ""
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, upload).Scan(&uploadRefKey); err != nil {
		t.Fatalf("read the upload's ref_key: %v", err)
	}
	if held := currentPartOf(t, database, blockAddress(t, database, upload, "gate")); held.ToKind != graphstore.KindArtifact || held.ToID != uploadRefKey {
		t.Errorf("the upload's top-level block is part of %s/%s after the reconciliation, want the upload's own node artifact/%s",
			held.ToKind, held.ToID, uploadRefKey)
	}

	census := treeCensus(t, database)
	for _, kind := range census.Kinds {
		for _, difference := range append(append([]graphcensus.Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("after the reconciliation the census reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	for _, name := range []string{"block", "block_rank", "block_text", "block_size"} {
		if kind := treeCensusKind(t, census, name); kind.Rows == 0 || kind.Nodes == 0 {
			t.Errorf("the census read %d rows and %d nodes of kind %s after the reconciliation, so it compared nothing there", kind.Rows, kind.Nodes, name)
		}
	}

	// The controls: one block's text retracted, another's rank of another document's band, and the
	// sized plan node's size retracted.
	retractedAt, rankedAt := blockAddress(t, database, spec, "stays"), blockAddress(t, database, verification, "first-check")
	text, _ := treeValue(t, database, retractedAt, treeText)
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
			FromKind: refs.BlockNodeKind, FromID: retractedAt, Kind: treeText, ToKind: graphstore.KindValue, ToID: text,
			At: time.Now(), Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: false,
		}, {
			FromKind: refs.BlockNodeKind, FromID: rankedAt, Kind: treeRank, ToKind: graphstore.KindValue, ToID: "1U",
			At: time.Now(), Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: true,
		}, {
			FromKind: refs.BlockNodeKind, FromID: sized, Kind: treeSize, ToKind: graphstore.KindValue, ToID: "90",
			At: time.Now(), Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: false,
		}})
	}); err != nil {
		t.Fatalf("break one block's text, another's rank and a plan node's size: %v", err)
	}
	broken := treeCensus(t, database)
	for name, address := range map[string]string{"block_text": retractedAt, "block_rank": rankedAt, "block_size": sized} {
		kind := treeCensusKind(t, broken, name)
		if kind.RowDifferences != 1 || len(kind.ReportedRows) != 1 || !strings.Contains(kind.ReportedRows[0].At, address) {
			t.Errorf("with block %s broken the census reads %d row difference(s) of kind %s, %+v, want the one naming it",
				address, kind.RowDifferences, name, kind.ReportedRows)
		} else {
			t.Logf("the census reads kind %s at %s: %s", name, kind.ReportedRows[0].At, kind.ReportedRows[0].Cause)
		}
	}
}

// treeCensus takes the store census over database, on a connection of its own.
func treeCensus(t *testing.T, database *store.Store) *graphcensus.StoreReport {
	t.Helper()
	report, err := graphcensus.StoreCensus(context.Background(), database.Pool.Config().ConnString(), false, ReturnDeclined)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	return report
}

// treeCensusKind is the census's line for one kind.
func treeCensusKind(t *testing.T, report *graphcensus.StoreReport, name string) graphcensus.KindReport {
	t.Helper()
	index := slices.IndexFunc(report.Kinds, func(kind graphcensus.KindReport) bool { return kind.Kind == name })
	if index < 0 {
		t.Fatalf("the census reported no kind %s", name)
	}
	return report.Kinds[index]
}
