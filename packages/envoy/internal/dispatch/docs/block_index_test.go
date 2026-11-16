package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// blockIndexService is a document service whose settle delay is an hour, so no timer settles a
// document during a test, on a database holding project CORE and its issues CORE-1, where the
// documents live unless a test puts one on CORE-2, and CORE-2, which they cite.
func blockIndexService(t *testing.T) (*store.Store, *Service) {
	t.Helper()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U'),
			('CORE-2', 'CORE', 2, 'Two', '{"kind":"user","id":"alice"}', 'V');
	`); err != nil {
		t.Fatalf("seed project CORE: %v", err)
	}
	service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour})
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = service.Shutdown(stop)
	})
	return database, service
}

// createOwedDocument creates document slug of issue (CORE-1 or CORE-2), id its uuid (empty: one
// the database draws), whose live text is markdown and whose version 1 stores stored (empty: the
// live text), as a build without the block index writes it: no block index, so the document is
// owed one. Its seed leaves it owing a settlement, as an unsettled document does.
func createOwedDocument(t *testing.T, database *store.Store, service *Service, issue, id, slug, markdown, stored string) string {
	t.Helper()
	ctx := context.Background()
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by)
		values (coalesce(nullif($1, '')::uuid, gen_random_uuid()), $3, 'CORE', $2, $2 || '.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text
	`, id, slug, issue).Scan(&id); err != nil {
		t.Fatalf("create document %s: %v", slug, err)
	}
	seedServiceText(t, service, id, markdown)
	if stored == "" {
		live, _, err := service.TextWithBlocks(ctx, id)
		if err != nil {
			t.Fatalf("read document %s: %v", slug, err)
		}
		stored = live
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
	`, id, stored); err != nil {
		t.Fatalf("write version 1 of %s: %v", slug, err)
	}
	return id
}

// settle clears the document's pending settlement, as the settlement that wrote its latest
// version does.
func settle(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `delete from doc_settlements_pending where artifact_id = $1`, artifactID); err != nil {
		t.Fatalf("settle %s: %v", artifactID, err)
	}
}

// blockIndexAt is the version the document's blocks are indexed at; nil while owed.
func blockIndexAt(t *testing.T, database *store.Store, artifactID string) *int {
	t.Helper()
	var version *int
	if err := database.Pool.QueryRow(context.Background(),
		`select version from document_block_versions where artifact_id = $1`, artifactID).Scan(&version); err != nil {
		t.Fatalf("read the block index of %s: %v", artifactID, err)
	}
	return version
}

// reconcileBlockIndex runs one tick of a loop that has found no document live ahead before,
// from cursor over the owed documents and from the start for every other of the tick's reads.
func reconcileBlockIndex(t *testing.T, service *Service, cursor string) BlockIndexTick {
	t.Helper()
	tick, err := service.ReconcileBlockIndex(context.Background(), BlockIndexCursors{Documents: cursor}, liveAheadDocuments{})
	if err != nil {
		t.Fatalf("block index tick: %v", err)
	}
	return tick
}

// holderOf is the id of the innermost block of the document's live text holding reference.
func holderOf(t *testing.T, service *Service, artifactID, reference string) string {
	t.Helper()
	markdown, blocks, err := service.TextWithBlocks(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read %s: %v", artifactID, err)
	}
	offset := strings.Index(markdown, reference)
	holder := -1
	for index, block := range blocks {
		if block.From <= offset && offset < block.To && (holder < 0 || block.To-block.From <= blocks[holder].To-blocks[holder].From) {
			holder = index
		}
	}
	if offset < 0 || holder < 0 {
		t.Fatalf("no block of %s holds %s: %q %#v", artifactID, reference, markdown, blocks)
	}
	return blocks[holder].ID
}

type blockPair struct {
	BlockID, ToKind, ToID string
	CreatedVersion        int
	CreatedAt             time.Time
	SourceSeq             *int64
}

func blockPairsOf(t *testing.T, database *store.Store, artifactID string) []blockPair {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, to_kind, to_id, created_version, created_at, source_seq
		from block_refs where artifact_id = $1 order by block_id, to_kind, to_id
	`, artifactID)
	if err != nil {
		t.Fatalf("read block_refs: %v", err)
	}
	pairs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (blockPair, error) {
		var pair blockPair
		err := row.Scan(&pair.BlockID, &pair.ToKind, &pair.ToID, &pair.CreatedVersion, &pair.CreatedAt, &pair.SourceSeq)
		return pair, err
	})
	if err != nil {
		t.Fatalf("scan block_refs: %v", err)
	}
	return pairs
}

type refRow struct {
	ToKind, ToID string
	CreatedAt    time.Time
	SourceSeq    *int64
}

func refsFrom(t *testing.T, database *store.Store, fromKind, fromID string) []refRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select to_kind, to_id, created_at, source_seq from refs
		where from_kind = $1 and from_id = $2 order by to_kind, to_id
	`, fromKind, fromID)
	if err != nil {
		t.Fatalf("read refs: %v", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (refRow, error) {
		var ref refRow
		err := row.Scan(&ref.ToKind, &ref.ToID, &ref.CreatedAt, &ref.SourceSeq)
		return ref, err
	})
	if err != nil {
		t.Fatalf("scan refs: %v", err)
	}
	return found
}

// shipItSpec is a spec whose callout ship_it holds a paragraph citing CORE-2, beside another
// paragraph.
const shipItSpec = ":::callout{#ship_it kind=\"note\" title=\"\"}\nShip it after dispatch://CORE-2.\n:::\n\nOther paragraph.\n"

// An owed document is indexed whole by one tick: its blocks with their texts at its latest
// version, the mention a block holds, dated from the document's own reference to that target on
// this first indexing, the mention's excerpt, and the version recorded indexed.
func TestBlockIndexIndexesAnOwedDocument(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	spec := createOwedDocument(t, database, service, "CORE-1", "", "spec", shipItSpec, "")
	settle(t, database, spec)
	// The document's own reference, as the build that wrote the version indexed it.
	written := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := database.Pool.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id, created_at, source_seq) values ('artifact', $1, 'issue', 'CORE-2', $2, 7)
	`, spec, written); err != nil {
		t.Fatalf("write the document's reference: %v", err)
	}
	markdown, blocks, err := service.TextWithBlocks(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.Documents != 1 || tick.LiveAhead != 0 || tick.OwedDocuments != 0 || tick.Full || tick.Cursor.Documents != "" {
		t.Fatalf("tick = %#v, want the one document indexed and nothing owed", tick)
	}
	if version := blockIndexAt(t, database, spec); version == nil || *version != 1 {
		t.Fatalf("blocks indexed at %v, want version 1", version)
	}
	var recorded bool
	if err := database.Pool.QueryRow(ctx, `
		select exists (select 1 from document_indexed_versions where artifact_id = $1 and version = 1)
	`, spec).Scan(&recorded); err != nil || !recorded {
		t.Fatalf("version 1 recorded indexed = %v (%v)", recorded, err)
	}
	if len(blocks) < 2 {
		t.Fatalf("the spec has blocks %#v, want the callout and its paragraphs", blocks)
	}
	for _, block := range blocks {
		var changed int
		var text string
		if err := database.Pool.QueryRow(ctx, `
			select d.changed_version, t.text from document_blocks d
			join document_block_texts t on t.artifact_id = d.artifact_id and t.block_id = d.block_id and t.version = 1
			where d.artifact_id = $1 and d.block_id = $2
		`, spec, block.ID).Scan(&changed, &text); err != nil {
			t.Fatalf("block %s is not indexed: %v", block.ID, err)
		}
		if changed != 1 || text != markdown[block.From:block.To] {
			t.Fatalf("block %s changed at %d with text %q, want version 1 and %q", block.ID, changed, text, markdown[block.From:block.To])
		}
	}
	holder := holderOf(t, service, spec, "dispatch://CORE-2")
	seq := int64(7)
	want := []blockPair{{BlockID: holder, ToKind: "issue", ToID: "CORE-2", CreatedVersion: 1, CreatedAt: written, SourceSeq: &seq}}
	if pairs := blockPairsOf(t, database, spec); len(pairs) != 1 || pairs[0].BlockID != holder || pairs[0].ToID != "CORE-2" ||
		!pairs[0].CreatedAt.Equal(written) || pairs[0].SourceSeq == nil || *pairs[0].SourceSeq != seq {
		t.Fatalf("block_refs = %#v, want %#v", pairs, want)
	}
	var excerptVersion *int
	var excerptBlock *string
	if err := database.Pool.QueryRow(ctx, `
		select excerpt_version, excerpt_block_id from refs where from_kind = 'artifact' and from_id = $1
	`, spec).Scan(&excerptVersion, &excerptBlock); err != nil {
		t.Fatalf("read the mention's excerpt: %v", err)
	}
	if excerptVersion == nil || *excerptVersion != 1 || excerptBlock == nil || *excerptBlock != holder {
		t.Fatalf("the mention's excerpt is block %v at version %v, want block %s at version 1", excerptBlock, excerptVersion, holder)
	}
}

// A document owed a settlement whose live text differs from its latest version is ahead of it:
// indexing the live text at that version would date the later edit's blocks to it. It is skipped,
// counted, and left owed.
func TestBlockIndexSkipsADocumentWhoseLiveTextIsAheadOfItsVersion(t *testing.T) {
	database, service := blockIndexService(t)
	spec := createOwedDocument(t, database, service, "CORE-1", "", "spec", shipItSpec, "Ship it.\n")

	tick := reconcileBlockIndex(t, service, "")
	if tick.Documents != 0 || tick.LiveAhead != 1 || tick.OwedDocuments != 1 {
		t.Fatalf("tick = %#v, want the document skipped as live ahead and still owed", tick)
	}
	if version := blockIndexAt(t, database, spec); version != nil {
		t.Fatalf("blocks indexed at %d, want the document still owed", *version)
	}
	var blocks, indexed int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from document_blocks where artifact_id = $1),
		       (select count(*) from document_indexed_versions where artifact_id = $1)
	`, spec).Scan(&blocks, &indexed); err != nil {
		t.Fatal(err)
	}
	if blocks != 0 || indexed != 0 {
		t.Fatalf("a skipped document has %d blocks and %d indexed versions, want none", blocks, indexed)
	}
}

// A closed issue's document is indexed as an open issue's is: owing a settlement, with its live
// text its latest version's byte for byte, it is indexed at that version, where the index is
// exact. The issue's close changes nothing of that.
func TestBlockIndexIndexesAClosedDocumentWhoseLiveTextEqualsItsVersion(t *testing.T) {
	database, service := blockIndexService(t)
	spec := createOwedDocument(t, database, service, "CORE-1", "", "spec", shipItSpec, "")
	var pending bool
	if err := database.Pool.QueryRow(context.Background(), `
		update issues set closed_at = now() where key = 'CORE-1'
		returning exists (select 1 from doc_settlements_pending where artifact_id = $1)
	`, spec).Scan(&pending); err != nil || !pending {
		t.Fatalf("close CORE-1 with its document owing a settlement: pending %v (%v)", pending, err)
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.Documents != 1 || tick.LiveAhead != 0 || tick.OwedDocuments != 0 {
		t.Fatalf("tick = %#v, want the closed document indexed", tick)
	}
	if version := blockIndexAt(t, database, spec); version == nil || *version != 1 {
		t.Fatalf("blocks indexed at %v, want version 1", version)
	}
	holder := holderOf(t, service, spec, "dispatch://CORE-2")
	if pairs := blockPairsOf(t, database, spec); len(pairs) != 1 || pairs[0].BlockID != holder {
		t.Fatalf("block_refs = %#v, want the mention from block %s", pairs, holder)
	}
}

// A closed issue's document is owed its block index as an open issue's is: the index reads no
// issue's status, and a closed issue's document settles like any other. The spec of CORE-1, which
// is closed, and its twin on CORE-2, which is open, each hold an edit no version holds. The first
// tick, before any settlement, finds both live ahead, indexes neither and leaves both owed, since
// indexing either at version 1 would date the edit's block to a version that lacks it. Each
// document's settlement then writes version 2, the closed issue's as the open one's, and indexes
// it there: the block the edit made is dated version 2, with its text.
//
// The control is the closed branch this replaced, indexOwedDocument's `if open` that indexed a
// closed issue's document at its latest version from that version's rendering
// (heldVersionBlocks): with it, the first tick indexes the closed document at version 1, and the
// tick's assertion reads Documents 1 and LiveAhead 1.
func TestAClosedIssuesDocumentIsLeftOwedLikeAnOpenOneUntilItSettles(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	const stored = "The paragraph version 1 holds.\n"
	const edit = "The line no version holds yet."
	type twin struct{ issue, id string }
	twins := []twin{{issue: "CORE-1"}, {issue: "CORE-2"}}
	for index := range twins {
		twins[index].id = createOwedDocument(t, database, service, twins[index].issue, "", "spec", stored+"\n"+edit+"\n", stored)
	}
	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'CORE-1'`); err != nil {
		t.Fatalf("close CORE-1: %v", err)
	}
	for _, document := range twins {
		closeDocumentRoom(t, service, document.id)
		live, _, err := service.TextWithBlocks(ctx, document.id)
		if err != nil {
			t.Fatalf("read the live document of %s: %v", document.issue, err)
		}
		var pending bool
		if err := database.Pool.QueryRow(ctx, `select exists (select 1 from doc_settlements_pending where artifact_id = $1)`, document.id).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(live, edit) || !pending {
			t.Fatalf("%s's document reads %q (settlement pending %v), want the edit ahead of version 1 %q and a settlement owed", document.issue, live, pending, stored)
		}
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.LiveAhead != 2 || tick.Documents != 0 || tick.OwedDocuments != 2 {
		t.Fatalf("tick = %#v, want both documents counted live ahead, neither indexed and both owed", tick)
	}
	for _, document := range twins {
		if version := blockIndexAt(t, database, document.id); version != nil {
			t.Fatalf("%s's document is indexed at version %d before its settlement, want it owed", document.issue, *version)
		}
	}

	for _, document := range twins {
		if err := service.SettleIssueDocuments(ctx, document.issue); err != nil {
			t.Fatalf("settle the documents of %s: %v", document.issue, err)
		}
	}
	var closed bool
	if err := database.Pool.QueryRow(ctx, `select closed_at is not null from issues where key = 'CORE-1'`).Scan(&closed); err != nil || !closed {
		t.Fatalf("CORE-1 reads closed %v (%v) after its settlement, want it still closed", closed, err)
	}
	for _, document := range twins {
		var latest int
		var markdown string
		if err := database.Pool.QueryRow(ctx, `
			select number, markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
		`, document.id).Scan(&latest, &markdown); err != nil {
			t.Fatalf("read the latest version of %s's document: %v", document.issue, err)
		}
		if latest != 2 || !strings.Contains(markdown, edit) {
			t.Fatalf("%s's latest version is %d %q, want version 2 holding the edit", document.issue, latest, markdown)
		}
		if version := blockIndexAt(t, database, document.id); version == nil || *version != 2 {
			t.Fatalf("%s's document is indexed at %v, want version 2", document.issue, version)
		}
		typed := holderOf(t, service, document.id, edit)
		var changed int
		var blockText string
		if err := database.Pool.QueryRow(ctx, `
			select d.changed_version, coalesce(t.text, '') from document_blocks d
			join document_block_texts t on t.artifact_id = d.artifact_id and t.block_id = d.block_id and t.version = 2
			where d.artifact_id = $1 and d.block_id = $2
		`, document.id, typed).Scan(&changed, &blockText); err != nil {
			t.Fatalf("read block %s, which the edit made in %s's document: %v", typed, document.issue, err)
		}
		if changed != 2 || !strings.Contains(blockText, edit) {
			t.Fatalf("block %s, which the edit made in %s's document, is dated version %d with text %q at version 2, want version 2 with the edit", typed, document.issue, changed, blockText)
		}
	}
}

// Twenty-five documents of an open issue whose ids sort first are skipped as live ahead, owed a
// settlement that has not run. The keyset moves past them, so the document that sorts last is
// indexed in the second batch rather than waiting behind them; they stay owed, and the next tick
// starts over.
func TestBlockIndexIsNotHeldByDocumentsItSkips(t *testing.T) {
	database, service := blockIndexService(t)
	skipped := make([]string, 25)
	for index := range skipped {
		id := fmt.Sprintf("00000000-0000-0000-0000-0000000000%02d", index+1)
		skipped[index] = createOwedDocument(t, database, service, "CORE-1", id, fmt.Sprintf("draft-%02d", index+1),
			fmt.Sprintf("Live text %d.\n", index+1), fmt.Sprintf("Earlier text %d.\n", index+1))
	}
	last := createOwedDocument(t, database, service, "CORE-1", "ffffffff-ffff-ffff-ffff-ffffffffffff", "spec", shipItSpec, "")
	settle(t, database, last)

	first := reconcileBlockIndex(t, service, "")
	if first.Documents != 0 || first.LiveAhead != blockIndexDocumentBatch || !first.Full || first.Cursor.Documents != skipped[blockIndexDocumentBatch-1] {
		t.Fatalf("first tick = %#v, want a full batch of skipped documents and the cursor past them", first)
	}
	second := reconcileBlockIndex(t, service, first.Cursor.Documents)
	if second.Documents != 1 || second.LiveAhead != len(skipped)-blockIndexDocumentBatch || second.Full || second.Cursor.Documents != "" || second.OwedDocuments != len(skipped) {
		t.Fatalf("second tick = %#v, want the last document indexed past the skipped ones, which stay owed", second)
	}
	if version := blockIndexAt(t, database, last); version == nil || *version != 1 {
		t.Fatalf("the last document's blocks are indexed at %v, want version 1", version)
	}
	for _, id := range skipped {
		if version := blockIndexAt(t, database, id); version != nil {
			t.Fatalf("skipped document %s indexed at %d, want it owed", id, *version)
		}
	}
	if again := reconcileBlockIndex(t, service, second.Cursor.Documents); again.LiveAhead != blockIndexDocumentBatch || again.Documents != 0 {
		t.Fatalf("third tick = %#v, want the batch to start over at the skipped documents", again)
	}
}

// A loop remembers the documents it found live ahead: one listed again at the same version, with
// the same pending-settlement mark and no room resident, is counted live ahead without being read,
// so a document whose settlement waits, on an open issue or a closed one, is not re-read every
// tick. A live update, a new version or a resident room has the next tick read it again, and a
// document indexed elsewhere is forgotten.
func TestBlockIndexDoesNotRereadALiveAheadDocumentUntilItMoves(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	edited := createOwedDocument(t, database, service, "CORE-1", "", "edited", "Live text one.\n", "Earlier text one.\n")
	versioned := createOwedDocument(t, database, service, "CORE-1", "", "versioned", "Live text two.\n", "Earlier text two.\n")
	opened := createOwedDocument(t, database, service, "CORE-1", "", "opened", "Live text three.\n", "Earlier text three.\n")
	indexedElsewhere := createOwedDocument(t, database, service, "CORE-1", "", "indexed", "Live text four.\n", "Earlier text four.\n")
	documents := []string{edited, versioned, opened, indexedElsewhere}
	for _, id := range documents {
		closeDocumentRoom(t, service, id)
	}
	liveAhead := liveAheadDocuments{}
	tick := func() BlockIndexTick {
		t.Helper()
		tick, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, liveAhead)
		if err != nil {
			t.Fatalf("block index tick: %v", err)
		}
		return tick
	}
	live := func(id string) string {
		t.Helper()
		markdown, _, err := service.TextWithBlocks(ctx, id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return markdown
	}
	storeAsVersion1 := func(id string) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `update artifact_versions set markdown = $2 where artifact_id = $1 and number = 1`, id, live(id)); err != nil {
			t.Fatalf("store the live text of %s as version 1: %v", id, err)
		}
	}
	settlementMark := func(id string) time.Time {
		t.Helper()
		var markedAt time.Time
		if err := database.Pool.QueryRow(ctx, `select marked_at from doc_settlements_pending where artifact_id = $1`, id).Scan(&markedAt); err != nil {
			t.Fatalf("read the pending settlement of %s: %v", id, err)
		}
		return markedAt
	}

	if first := tick(); first.LiveAhead != 4 || first.Documents != 0 {
		t.Fatalf("first tick = %#v, want the four documents live ahead", first)
	}
	// Each version 1 now holds its document's live text, which a read would index.
	for _, id := range documents {
		storeAsVersion1(id)
	}
	if second := tick(); second.LiveAhead != 4 || second.Documents != 0 || second.OwedDocuments != 4 {
		t.Fatalf("second tick = %#v, want the four documents counted live ahead unread", second)
	}

	// A live update moves the settlement mark; version 1 then holds the edited text.
	later, err := pmdoc.Parse("Later text one.\n")
	if err != nil {
		t.Fatal(err)
	}
	editLiveTree(t, service, edited, func(*pmdoc.Node) *pmdoc.Node { return later })
	waitForLandedAppends(t, service, edited)
	closeDocumentRoom(t, service, edited)
	storeAsVersion1(edited)
	// A version 2 holding the live text, with the settlement mark as it was.
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 2, $2, '[{"kind":"user","id":"alice"}]')
	`, versioned, live(versioned)); err != nil {
		t.Fatalf("write version 2 of %s: %v", versioned, err)
	}
	// A resident room, which moves neither.
	mark := settlementMark(opened)
	if err := service.warmLiveDocument(ctx, opened); err != nil {
		t.Fatalf("open the room of %s: %v", opened, err)
	}
	if service.srv.GetDoc(opened) == nil || !settlementMark(opened).Equal(mark) {
		t.Fatalf("opening the room of %s left none resident or moved its settlement mark", opened)
	}
	if _, err := database.Pool.Exec(ctx, `update document_block_versions set version = 1 where artifact_id = $1`, indexedElsewhere); err != nil {
		t.Fatalf("index %s elsewhere: %v", indexedElsewhere, err)
	}

	if third := tick(); third.Documents != 3 || third.LiveAhead != 0 || third.OwedDocuments != 0 {
		t.Fatalf("third tick = %#v, want the three documents read again and indexed", third)
	}
	if len(liveAhead) != 0 {
		t.Fatalf("the loop still remembers %v, want nothing", liveAhead)
	}
}

// writeAsAnOlderBuild writes the document's next version as a build without the block index does:
// the edit (from replaced by to in a text of the live document) reaches the store through the room,
// and its settlement writes the version, the document's reference to CORE-2 with the old grammar,
// and the version's artifact.version event, and clears the pending settlement. It indexes no block,
// and nothing here marks the document owed. It returns the version and its event's id.
func writeAsAnOlderBuild(t *testing.T, database *store.Store, service *Service, artifactID, from, to string) (model.Version, int64) {
	t.Helper()
	ctx := context.Background()
	replaced := false
	var replace func(node *pmdoc.Node)
	replace = func(node *pmdoc.Node) {
		if node.Type == "text" && strings.Contains(node.Text, from) {
			node.Text, replaced = strings.Replace(node.Text, from, to, 1), true
		}
		for _, child := range node.Children {
			replace(child)
		}
	}
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		replace(tree)
		return tree
	})
	if !replaced {
		t.Fatalf("no text of %s holds %q", artifactID, from)
	}
	waitForLandedAppends(t, service, artifactID)
	live, _, err := service.TextWithBlocks(ctx, artifactID)
	if err != nil || !strings.Contains(live, to) {
		t.Fatalf("live text after the edit = %q (%v), want it to hold %q", live, err, to)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var version model.Version
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1, max(number) + 1, $2, '[{"kind":"user","id":"alice"}]' from artifact_versions where artifact_id = $1
		returning number, created_at
	`, artifactID, live).Scan(&version.Number, &version.CreatedAt); err != nil {
		t.Fatalf("write the version: %v", err)
	}
	var eventID int64
	if err := tx.QueryRow(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'CORE-1', coalesce(max(seq), 0) + 1, 'artifact.version', '{"kind":"user","id":"alice"}',
		       jsonb_build_object('artifact_id', $1::text, 'name', 'spec.md', 'version', jsonb_build_object('number', $2::int)), false
		from events where issue_key = 'CORE-1'
		returning id
	`, artifactID, version.Number).Scan(&eventID); err != nil {
		t.Fatalf("append the version's event: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id, source_seq) values ('artifact', $1, 'issue', 'CORE-2', $2)
		on conflict do nothing
	`, artifactID, eventID); err != nil {
		t.Fatalf("index the version's references: %v", err)
	}
	if _, err := tx.Exec(ctx, `delete from doc_settlements_pending where artifact_id = $1`, artifactID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return version, eventID
}

// citedSpec is a spec whose callout cites nothing yet; writeAsAnOlderBuild adds the citation.
const citedSpec = ":::callout{#ship_it kind=\"note\" title=\"\"}\nShip it.\n:::\n\nOther paragraph.\n"

// indexedSpec is CORE-1's spec, indexed at version 1 by a tick.
func indexedSpec(t *testing.T, database *store.Store, service *Service) string {
	t.Helper()
	spec := createOwedDocument(t, database, service, "CORE-1", "", "spec", citedSpec, "")
	settle(t, database, spec)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("indexing version 1: tick = %#v", tick)
	}
	return spec
}

// A version a build without the block index writes during a roll is indexed by the next tick, with
// no command run: the trigger on artifact_versions marked the document owed. The mention the version
// added is dated by the version and the event that recorded it.
func TestAnOlderBuildsVersionWriteIsIndexedWithNoCommand(t *testing.T) {
	database, service := blockIndexService(t)
	spec := indexedSpec(t, database, service)
	version, eventID := writeAsAnOlderBuild(t, database, service, spec, "Ship it.", "Ship it after dispatch://CORE-2.")
	if indexed := blockIndexAt(t, database, spec); indexed != nil {
		t.Fatalf("after the older build's version the blocks stand at %d, want the document owed", *indexed)
	}

	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 || tick.OwedDocuments != 0 {
		t.Fatalf("tick = %#v, want the older build's version indexed", tick)
	}
	if indexed := blockIndexAt(t, database, spec); indexed == nil || *indexed != version.Number {
		t.Fatalf("blocks indexed at %v, want version %d", indexed, version.Number)
	}
	holder := holderOf(t, service, spec, "dispatch://CORE-2")
	pairs := blockPairsOf(t, database, spec)
	if len(pairs) != 1 || pairs[0].BlockID != holder || pairs[0].ToID != "CORE-2" || pairs[0].CreatedVersion != version.Number ||
		!pairs[0].CreatedAt.Equal(version.CreatedAt) || pairs[0].SourceSeq == nil || *pairs[0].SourceSeq != eventID {
		t.Fatalf("block_refs = %#v, want the mention from %s created by version %d at %s, event %d", pairs, holder, version.Number, version.CreatedAt, eventID)
	}
	var changed int
	if err := database.Pool.QueryRow(context.Background(), `
		select changed_version from document_blocks where artifact_id = $1 and block_id = $2
	`, spec, holder).Scan(&changed); err != nil || changed != version.Number {
		t.Fatalf("the edited block changed at %d (%v), want version %d", changed, err, version.Number)
	}
}

// The link a tick moves from the document onto its block keeps its place in an incremental read:
// GET /api/v1/references?since= lists it past the event before its version's, and not past its own.
func TestAReconciledLinkKeepsItsPlaceInSince(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	spec := indexedSpec(t, database, service)
	_, eventID := writeAsAnOlderBuild(t, database, service, spec, "Ship it.", "Ship it after dispatch://CORE-2.")
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("tick = %#v, want the version indexed", tick)
	}
	holder := holderOf(t, service, spec, "dispatch://CORE-2")

	before := eventID - 1
	edges, err := refs.Edges(ctx, database.Pool, refs.Query{Direction: "in", Kind: "issue", ID: "CORE-2", Since: &before})
	if err != nil {
		t.Fatalf("read the references to CORE-2 since %d: %v", before, err)
	}
	if len(edges) != 1 || edges[0].Node.Kind != "block" || edges[0].Node.ID != "CORE-1/spec#"+holder ||
		edges[0].SourceSeq == nil || *edges[0].SourceSeq != eventID {
		t.Fatalf("references to CORE-2 since %d = %#v, want the block %s's mention from event %d", before, edges, holder, eventID)
	}
	edges, err = refs.Edges(ctx, database.Pool, refs.Query{Direction: "in", Kind: "issue", ID: "CORE-2", Since: &eventID})
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Fatalf("references to CORE-2 since its own event %d = %#v, want none", eventID, edges)
	}
}

// A comment and an issue message an older build wrote cite a block: the comment by its dashboard
// URL, which that build indexed as the whole spec, the message by a pinned dispatch:// reference,
// which it dropped. One tick indexes each with the block grammar: one edge each, to the block,
// dated when the text was written and by no event. Nothing is left owed, and a second tick changes
// nothing.
func TestOlderItemTextCitingABlockIsReindexed(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	commentAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	messageAt := time.Date(2026, 3, 5, 6, 7, 8, 0, time.UTC)
	var comment, message string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'See https://dispatch.example/issues/CORE-1/spec#b-ship%5Fit please.', $1)
		returning id::text
	`, commentAt).Scan(&comment); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Read dispatch://CORE-1/spec#b-ship%5Fit@v1 first.', $1)
		returning id::text
	`, messageAt).Scan(&message); err != nil {
		t.Fatalf("write the message: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id, created_at, source_seq) values ('comment', $1, 'artifact', 'CORE-1/spec', $2, 3)
	`, comment, commentAt); err != nil {
		t.Fatalf("index the comment with the old grammar: %v", err)
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.Items != 2 || tick.OwedItems != 0 {
		t.Fatalf("tick = %#v, want both items indexed and none owed", tick)
	}
	for _, item := range []struct {
		kind, id string
		at       time.Time
	}{{"comment", comment, commentAt}, {"message", message, messageAt}} {
		got := refsFrom(t, database, item.kind, item.id)
		if len(got) != 1 || got[0].ToKind != "block" || got[0].ToID != "CORE-1/spec#ship_it" || !got[0].CreatedAt.Equal(item.at) || got[0].SourceSeq != nil {
			t.Fatalf("refs from the %s = %#v, want one edge to block CORE-1/spec#ship_it created at %s by no event", item.kind, got, item.at)
		}
	}
	commentRefs, messageRefs := refsFrom(t, database, "comment", comment), refsFrom(t, database, "message", message)

	if again := reconcileBlockIndex(t, service, ""); again.Items != 0 || again.OwedItems != 0 {
		t.Fatalf("second tick = %#v, want nothing owed", again)
	}
	if got := refsFrom(t, database, "comment", comment); !reflect.DeepEqual(got, commentRefs) {
		t.Fatalf("the second tick changed the comment's refs: %#v -> %#v", commentRefs, got)
	}
	if got := refsFrom(t, database, "message", message); !reflect.DeepEqual(got, messageRefs) {
		t.Fatalf("the second tick changed the message's refs: %#v -> %#v", messageRefs, got)
	}
}

// An edit of a comment that lands while the tick indexes it is not lost: the tick waits on the row
// the edit holds, finds the edit moved the comment's owed row once it commits, and so writes
// nothing and leaves the comment owed; the next tick indexes the edited text and settles it.
// Neither waits on the other for good.
func TestADrainedItemKeepsAConcurrentEdit(t *testing.T) {
	database, service := blockIndexService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var comment string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'See https://dispatch.example/issues/CORE-1/spec#b-ship%5Fit.')
		returning id::text
	`).Scan(&comment); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	writer, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	if _, err := writer.Exec(ctx, `
		update comments set body = 'Read dispatch://CORE-1/spec#b-other first.', edited_at = now() where id = $1
	`, comment); err != nil {
		t.Fatalf("edit the comment: %v", err)
	}

	ticked := make(chan error, 1)
	var tick BlockIndexTick
	go func() {
		var err error
		tick, err = service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, liveAheadDocuments{})
		ticked <- err
	}()
	waitForLockWait(t, ctx, database, "%from comments where id%", ticked)
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit the edit: %v", err)
	}
	select {
	case err := <-ticked:
		if err != nil {
			t.Fatalf("tick beside the edit: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the tick never finished after the edit committed")
	}
	if tick.Items != 0 || tick.OwedItems != 1 {
		t.Fatalf("tick beside the edit = %#v, want nothing written and the comment still owed", tick)
	}
	next, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, liveAheadDocuments{})
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if next.Items != 1 || next.OwedItems != 0 {
		t.Fatalf("second tick = %#v, want the comment indexed and nothing owed", next)
	}
	if got := refsFrom(t, database, "comment", comment); len(got) != 1 || got[0].ToKind != "block" || got[0].ToID != "CORE-1/spec#other" {
		t.Fatalf("refs from the comment = %#v, want the edited text's one edge to block CORE-1/spec#other", got)
	}
	var stillOwed bool
	err = database.Pool.QueryRow(ctx, `select true from block_grammar_owed where id = $1::uuid`, comment).Scan(&stillOwed)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the comment is still owed (%v), want its owed row gone", err)
	}
}

// itemBlockRow is one block of a comment or message as stored: a message's has changed revision 0.
type itemBlockRow struct {
	ID              string
	Position        *int
	ChangedRevision int
	ChangedAt       time.Time
	MadeAt          *time.Time
	RemovedAt       *time.Time
}

// itemBlocksOf is every block comment or message id ever held, by block id.
func itemBlocksOf(t *testing.T, database *store.Store, kind, id string) map[string]itemBlockRow {
	t.Helper()
	query := `select block_id, position, changed_revision, changed_at, made_at, removed_at
		from comment_blocks where comment_id = $1::uuid`
	if kind == "message" {
		query = `select block_id, position, 0, changed_at, made_at, removed_at
			from message_blocks where message_id = $1::uuid`
	}
	rows, err := database.Pool.Query(context.Background(), query, id)
	if err != nil {
		t.Fatalf("read the blocks of %s %s: %v", kind, id, err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (itemBlockRow, error) {
		var block itemBlockRow
		err := row.Scan(&block.ID, &block.Position, &block.ChangedRevision, &block.ChangedAt, &block.MadeAt, &block.RemovedAt)
		return block, err
	})
	if err != nil {
		t.Fatalf("scan the blocks of %s %s: %v", kind, id, err)
	}
	blocks := make(map[string]itemBlockRow, len(found))
	for _, block := range found {
		blocks[block.ID] = block
	}
	return blocks
}

// itemBlockRefsOf is each link a block of comment or message id holds, as "<block id> <to kind>
// <to id>", with its created_at.
func itemBlockRefsOf(t *testing.T, database *store.Store, kind, id string) map[string]time.Time {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id || ' ' || to_kind || ' ' || to_id, created_at from `+kind+`_block_refs where `+kind+`_id = $1::uuid
	`, id)
	if err != nil {
		t.Fatalf("read the block links of %s %s: %v", kind, id, err)
	}
	defer rows.Close()
	links := map[string]time.Time{}
	for rows.Next() {
		var link string
		var at time.Time
		if err := rows.Scan(&link, &at); err != nil {
			t.Fatalf("scan a block link of %s %s: %v", kind, id, err)
		}
		links[link] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the block links of %s %s: %v", kind, id, err)
	}
	return links
}

// checkItemBlock fails unless the block is current at position, last changed at revision and at,
// and made at madeAt.
func checkItemBlock(t *testing.T, label string, blocks map[string]itemBlockRow, id string, position, revision int, at, madeAt time.Time) {
	t.Helper()
	block, ok := blocks[id]
	if !ok || block.Position == nil || *block.Position != position || block.ChangedRevision != revision ||
		!block.ChangedAt.Equal(at) || block.MadeAt == nil || !block.MadeAt.Equal(madeAt) || block.RemovedAt != nil {
		t.Fatalf("%s block %s = %#v (found %v), want current at position %d, changed at revision %d at %s, made at %s",
			label, id, block, ok, position, revision, at, madeAt)
	}
}

// A comment, an issue message and a direct message a build without item blocks inserted, and a
// comment it edited after this build indexed it, are owed their blocks by the triggers, which no
// write of theirs cleared. One tick indexes each, at the time its body was written (a comment's
// coalesce(edited_at, created_at), a message's created_at): its blocks, the links each holds, and
// for the comment and the issue message their own references; the direct message holds no link.
// The edit is indexed at the comment's next revision under ids aligned with the ones it held: the
// moved blocks keep theirs and their changed revision, the reworded block keeps its id at the new
// revision, and the new block takes the next id. Nothing is left owed.
func TestTheBlockIndexIndexesItemsAnOlderBuildWrote(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	commentAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	messageAt := time.Date(2026, 3, 5, 6, 7, 8, 0, time.UTC)
	directAt := time.Date(2026, 3, 6, 7, 8, 9, 0, time.UTC)
	editedFirstAt := time.Date(2026, 3, 7, 8, 9, 10, 0, time.UTC)
	editedAt := time.Date(2026, 3, 8, 9, 10, 11, 0, time.UTC)
	insert := func(label, query string, args ...any) string {
		t.Helper()
		var id string
		if err := database.Pool.QueryRow(ctx, query, args...).Scan(&id); err != nil {
			t.Fatalf("write the %s: %v", label, err)
		}
		return id
	}
	comment := insert("comment", `
		insert into comments (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"user","id":"alice"}', E'First paragraph.\n\nSecond cites dispatch://CORE-2.', $1)
		returning id::text`, commentAt)
	message := insert("issue message", `
		insert into messages (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Read dispatch://CORE-2 now.', $1)
		returning id::text`, messageAt)
	direct := insert("direct message", `
		insert into messages (author, body, target, created_at)
		values ('{"kind":"user","id":"alice"}', E'Hello there.\n\nSee dispatch://CORE-2.', 'session:s1', $1)
		returning id::text`, directAt)
	edited := insert("comment to edit", `
		insert into comments (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"user","id":"alice"}', E'Alpha.\n\nBeta.\n\nGamma.', $1)
		returning id::text`, editedFirstAt)

	if tick := reconcileBlockIndex(t, service, ""); tick.ItemBlocks != 4 || tick.OwedItemBlocks != 0 || tick.Items != 0 {
		t.Fatalf("first tick = %#v, want the four items' blocks indexed and none owed", tick)
	}
	blocks := itemBlocksOf(t, database, "comment", comment)
	if len(blocks) != 2 {
		t.Fatalf("comment blocks = %#v, want two", blocks)
	}
	checkItemBlock(t, "comment", blocks, "b1", 0, 1, commentAt, commentAt)
	checkItemBlock(t, "comment", blocks, "b2", 1, 1, commentAt, commentAt)
	if links := itemBlockRefsOf(t, database, "comment", comment); len(links) != 1 || !links["b2 issue CORE-2"].Equal(commentAt) {
		t.Fatalf("comment block links = %#v, want b2's link to CORE-2 at %s", links, commentAt)
	}
	if got := refsFrom(t, database, "comment", comment); len(got) != 1 || got[0].ToID != "CORE-2" || !got[0].CreatedAt.Equal(commentAt) || got[0].SourceSeq != nil {
		t.Fatalf("refs from the comment = %#v, want one edge to CORE-2 at %s by no event", got, commentAt)
	}
	blocks = itemBlocksOf(t, database, "message", message)
	if len(blocks) != 1 {
		t.Fatalf("message blocks = %#v, want one", blocks)
	}
	checkItemBlock(t, "message", blocks, "b1", 0, 0, messageAt, messageAt)
	if links := itemBlockRefsOf(t, database, "message", message); len(links) != 1 || !links["b1 issue CORE-2"].Equal(messageAt) {
		t.Fatalf("message block links = %#v, want b1's link to CORE-2 at %s", links, messageAt)
	}
	if got := refsFrom(t, database, "message", message); len(got) != 1 || got[0].ToID != "CORE-2" || !got[0].CreatedAt.Equal(messageAt) {
		t.Fatalf("refs from the message = %#v, want one edge to CORE-2 at %s", got, messageAt)
	}
	blocks = itemBlocksOf(t, database, "message", direct)
	if len(blocks) != 2 {
		t.Fatalf("direct message blocks = %#v, want two", blocks)
	}
	checkItemBlock(t, "direct message", blocks, "b1", 0, 0, directAt, directAt)
	checkItemBlock(t, "direct message", blocks, "b2", 1, 0, directAt, directAt)
	if links := itemBlockRefsOf(t, database, "message", direct); len(links) != 0 {
		t.Fatalf("direct message block links = %#v, want none", links)
	}
	if got := refsFrom(t, database, "message", direct); len(got) != 0 {
		t.Fatalf("refs from the direct message = %#v, want none", got)
	}
	blocks = itemBlocksOf(t, database, "comment", edited)
	if len(blocks) != 3 {
		t.Fatalf("blocks of the comment to edit = %#v, want three", blocks)
	}
	for position, id := range []string{"b1", "b2", "b3"} {
		checkItemBlock(t, "comment to edit", blocks, id, position, 1, editedFirstAt, editedFirstAt)
	}

	// The older build's edit moves Gamma first, rewords Beta and adds Delta.
	if _, err := database.Pool.Exec(ctx, `
		update comments set body = E'Gamma.\n\nAlpha.\n\nBeta, reworded, cites dispatch://CORE-2.\n\nDelta.', edited_at = $2
		where id = $1::uuid
	`, edited, editedAt); err != nil {
		t.Fatalf("edit the comment: %v", err)
	}
	if tick := reconcileBlockIndex(t, service, ""); tick.ItemBlocks != 1 || tick.OwedItemBlocks != 0 {
		t.Fatalf("tick after the edit = %#v, want the edited comment's blocks indexed and none owed", tick)
	}
	blocks = itemBlocksOf(t, database, "comment", edited)
	if len(blocks) != 4 {
		t.Fatalf("blocks of the edited comment = %#v, want four", blocks)
	}
	checkItemBlock(t, "edited comment", blocks, "b3", 0, 1, editedFirstAt, editedFirstAt)
	checkItemBlock(t, "edited comment", blocks, "b1", 1, 1, editedFirstAt, editedFirstAt)
	checkItemBlock(t, "edited comment", blocks, "b2", 2, 2, editedAt, editedFirstAt)
	checkItemBlock(t, "edited comment", blocks, "b4", 3, 2, editedAt, editedAt)
	var revision, next int
	var owedAt *time.Time
	if err := database.Pool.QueryRow(ctx, `
		select revision, next_block, owed_at from comment_block_state where comment_id = $1::uuid
	`, edited).Scan(&revision, &next, &owedAt); err != nil {
		t.Fatalf("read the edited comment's block state: %v", err)
	}
	if revision != 2 || next != 5 || owedAt != nil {
		t.Fatalf("edited comment's state = revision %d, next block %d, owed at %v; want revision 2, next block 5, not owed", revision, next, owedAt)
	}
	if links := itemBlockRefsOf(t, database, "comment", edited); len(links) != 1 || !links["b2 issue CORE-2"].Equal(editedAt) {
		t.Fatalf("edited comment block links = %#v, want b2's link to CORE-2 at %s", links, editedAt)
	}
	if got := refsFrom(t, database, "comment", edited); len(got) != 1 || got[0].ToID != "CORE-2" || !got[0].CreatedAt.Equal(editedAt) {
		t.Fatalf("refs from the edited comment = %#v, want one edge to CORE-2 at %s", got, editedAt)
	}

	if again := reconcileBlockIndex(t, service, ""); again.ItemBlocks != 0 || again.OwedItemBlocks != 0 {
		t.Fatalf("third tick = %#v, want nothing owed", again)
	}
}

// A comment or message rebuild-refs marks owed, already indexed, is re-indexed by the next tick
// with nothing about its blocks changed: the same ids at the same revision.
func TestTheBlockIndexReindexesItemsRebuildRefsOwed(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	var comment string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"alice"}', E'One.\n\nTwo cites dispatch://CORE-2.')
		returning id::text
	`).Scan(&comment); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	if tick := reconcileBlockIndex(t, service, ""); tick.ItemBlocks != 1 {
		t.Fatalf("first tick = %#v, want the comment indexed", tick)
	}
	before := itemBlocksOf(t, database, "comment", comment)
	report, err := refs.RebuildAll(ctx, database.Pool, "https://dispatch.example")
	if err != nil {
		t.Fatalf("rebuild refs: %v", err)
	}
	if report.ItemsOwed != 1 {
		t.Fatalf("rebuild report = %#v, want the one comment owed", report)
	}
	if tick := reconcileBlockIndex(t, service, ""); tick.ItemBlocks != 1 || tick.OwedItemBlocks != 0 {
		t.Fatalf("tick after rebuild-refs = %#v, want the comment indexed and none owed", tick)
	}
	if after := itemBlocksOf(t, database, "comment", comment); !reflect.DeepEqual(after, before) {
		t.Fatalf("re-indexing changed the comment's blocks: %#v -> %#v", before, after)
	}
	var revision int
	if err := database.Pool.QueryRow(ctx, `select revision from comment_block_state where comment_id = $1::uuid`, comment).Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("revision after the re-index = %d (%v), want 1", revision, err)
	}
}

// windowsOf is each embeddings window row of owner ownerKey of kind kind, by id, with the
// blocks it covers.
func windowsOf(t *testing.T, database *store.Store, kind, ownerKey string) map[string][]string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select id, covers from embeddings where kind = $1 and split_part(id, '#', 1) = $2
	`, kind, ownerKey)
	if err != nil {
		t.Fatalf("read the windows of %s %s: %v", kind, ownerKey, err)
	}
	defer rows.Close()
	windows := map[string][]string{}
	for rows.Next() {
		var id string
		var covers []string
		if err := rows.Scan(&id, &covers); err != nil {
			t.Fatalf("scan a window of %s %s: %v", kind, ownerKey, err)
		}
		windows[id] = covers
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the windows of %s %s: %v", kind, ownerKey, err)
	}
	return windows
}

// windowTextsOf is the text of each embedding window row of owner ownerKey of kind kind, by id.
func windowTextsOf(t *testing.T, database *store.Store, kind, ownerKey string) map[string]string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select id, text_snapshot from embeddings where kind = $1 and split_part(id, '#', 1) = $2
	`, kind, ownerKey)
	if err != nil {
		t.Fatalf("read the windows of %s %s: %v", kind, ownerKey, err)
	}
	defer rows.Close()
	windows := map[string]string{}
	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			t.Fatalf("scan a window of %s %s: %v", kind, ownerKey, err)
		}
		windows[id] = text
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the windows of %s %s: %v", kind, ownerKey, err)
	}
	return windows
}

// seedItemsWithBlocks writes one comment, one issue message and one ask of CORE-1 and returns
// their ids, in that order.
func seedItemsWithBlocks(t *testing.T, database *store.Store) (comment, message, ask string) {
	t.Helper()
	ctx := context.Background()
	for _, item := range []struct {
		into *string
		sql  string
	}{
		{&comment, `insert into comments (issue_key, author, body)
			values ('CORE-1', '{"kind":"user","id":"alice"}', E'The first paragraph.\n\nThe second paragraph.') returning id::text`},
		{&message, `insert into messages (issue_key, author, body)
			values ('CORE-1', '{"kind":"user","id":"alice"}', 'A message of one block.') returning id::text`},
		{&ask, `insert into asks (issue_key, author, question, options)
			values ('CORE-1', '{"kind":"session","id":"asker"}', 'Which region?', '[{"label":"eu"}]') returning id::text`},
	} {
		if err := database.Pool.QueryRow(ctx, item.sql).Scan(item.into); err != nil {
			t.Fatalf("seed an item: %v", err)
		}
	}
	return comment, message, ask
}

// A document, a comment, a message and an ask whose blocks are indexed and whose windows are
// not - every item written before meaning search moved to windows - take their windows from one
// tick, and a tick after it rewrites none of them.
func TestAnItemWrittenBeforeThisChangeGetsWindowsFromOnePass(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	document := createOwedDocument(t, database, service, "CORE-1", "", "notes", "# Notes\n\nA paragraph of the document.\n", "")
	settle(t, database, document)
	comment, message, ask := seedItemsWithBlocks(t, database)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 || tick.ItemBlocks != 3 {
		t.Fatalf("first tick = %#v, want the document and the three items indexed", tick)
	}
	owners := map[string]string{
		refs.DocumentWindowKind: document,
		"comment_block":         "comment/" + comment,
		"message_block":         "message/" + message,
		"ask_part":              "ask/" + ask,
	}
	for kind, owner := range owners {
		if windows := windowsOf(t, database, kind, owner); len(windows) == 0 {
			t.Fatalf("%s %s holds no window after the tick that indexed its blocks", kind, owner)
		}
	}
	// Every window row as a build before this change left it: none, and every owner marked, as
	// a route that removes a window row must mark its owner (refs.ItemsOwedWindows,
	// refs.DocumentsOwedWindows) and as each marker's backfill marked every owner whose
	// windows were already missing.
	if _, err := database.Pool.Exec(ctx, `
		delete from embeddings where kind in ('document_block', 'comment_block', 'message_block', 'ask_part');
		update document_block_versions set windows_owed_at = now();
		update comment_block_state set windows_owed_at = now();
		update message_block_state set windows_owed_at = now();
		update ask_block_state set windows_owed_at = now();
	`); err != nil {
		t.Fatalf("delete the windows: %v", err)
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.Windows != 4 {
		t.Fatalf("the window arm gave windows to %d owners (%#v), want the document, the comment, the message and the ask", tick.Windows, tick)
	}
	windows := map[string]map[string][]string{}
	for kind, owner := range owners {
		windows[kind] = windowsOf(t, database, kind, owner)
		if len(windows[kind]) == 0 {
			t.Errorf("%s %s holds no window after one drift pass", kind, owner)
		}
		for id, covers := range windows[kind] {
			if len(covers) == 0 {
				t.Errorf("window %s covers no block", id)
			}
		}
	}
	if next := reconcileBlockIndex(t, service, ""); next.Windows != 0 {
		t.Fatalf("the next tick windowed %d owners (%#v), want none left", next.Windows, next)
	}
	for kind, owner := range owners {
		if got := windowsOf(t, database, kind, owner); !reflect.DeepEqual(got, windows[kind]) {
			t.Errorf("the windows of %s %s moved: %v -> %v", kind, owner, windows[kind], got)
		}
	}
}

// A document whose live text is ahead of its latest version, with a settlement pending that
// will not run, is windowed all the same, and for the text that version holds: the arm reads
// the stored markdown of the latest version whatever the settlement state, and gives its
// blocks the ids the index holds for them (windowDocument, closedVersionBlocks).
//
// This is what lets a version write leave its windows to the tick (refs.IndexBlocks,
// INFRA-988). Before it the arm refused such a document - `case pending && markdown !=
// stored: return false, nil` - which was safe only because the version write had written its
// windows itself; with the write no longer writing them, that refusal would leave a document
// under continuous editing with no window at all.
//
// The control is that refusal: with it, the last assertion reads
//
//	the window arm gave windows to 0 owners (…), want the document
//
// and the document is left listed by refs.DocumentsOwedWindows for ever.
func TestWindowArmWindowsTheStoredVersionOfADocumentWhoseLiveTextIsAhead(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	const stored = "# Notes\n\nThe stored paragraph.\n"
	document := createOwedDocument(t, database, service, "CORE-1", "", "notes", stored, "")
	settle(t, database, document)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("first tick = %#v, want the document's blocks indexed", tick)
	}

	// An edit that no settlement has written a version for: the live text is ahead of version
	// 1, and the document owes a settlement this service never runs.
	if err := joinedWrite(service, func(ctx context.Context) error {
		_, err := service.ReplaceText(ctx, document, stored+"\nThe live paragraph.\n",
			model.Actor{Kind: "user", ID: "alice"})
		return err
	}); err != nil {
		t.Fatalf("edit the document past its latest version: %v", err)
	}
	live, _, err := service.TextWithBlocks(ctx, document)
	if err != nil {
		t.Fatalf("read the live text of the document: %v", err)
	}
	if !strings.Contains(live, "The live paragraph.") {
		t.Fatalf("the live text holds no edit: %q", live)
	}
	var pending bool
	if err := database.Pool.QueryRow(ctx,
		`select exists (select 1 from doc_settlements_pending where artifact_id = $1)`, document,
	).Scan(&pending); err != nil || !pending {
		t.Fatalf("the document owes a settlement = %v (%v), want it owing one: this test is for that case", pending, err)
	}
	// Its window rows are lost and its owner marked, as a route that removes one must mark it,
	// so the arm is owed them again while the live text is ahead.
	if _, err := database.Pool.Exec(ctx,
		`delete from embeddings where kind = $1 and split_part(id, '#', 1) = $2`,
		refs.DocumentWindowKind, document); err != nil {
		t.Fatalf("delete the document's windows: %v", err)
	}
	if _, err := database.Pool.Exec(ctx,
		`update document_block_versions set windows_owed_at = coalesce(windows_owed_at, now())
		 where artifact_id = $1::uuid`, document); err != nil {
		t.Fatalf("mark the document owed its windows: %v", err)
	}
	owed, err := refs.DocumentsOwedWindows(ctx, database.Pool, "", 10)
	if err != nil {
		t.Fatalf("list the documents owed their windows: %v", err)
	}
	if !slices.Contains(owed, document) {
		t.Fatalf("the window arm is not owed document %s, which holds no window: %v", document, owed)
	}

	if tick := reconcileBlockIndex(t, service, ""); tick.Windows != 1 {
		t.Fatalf("the window arm gave windows to %d owners (%#v), want the document", tick.Windows, tick)
	}
	windows := windowTextsOf(t, database, refs.DocumentWindowKind, document)
	if len(windows) == 0 {
		t.Fatalf("one pass left document %s unwindowed", document)
	}
	held := false
	for id, text := range windows {
		if strings.Contains(text, "The live paragraph.") {
			t.Fatalf("window %s holds the unsettled edit's text, which no version holds: %q", id, text)
		}
		held = held || strings.Contains(text, "The stored paragraph.")
	}
	if !held {
		t.Fatalf("no window of document %s holds its stored version's text: %v", document, windows)
	}
	if owed, err := refs.DocumentsOwedWindows(ctx, database.Pool, "", 10); err != nil {
		t.Fatalf("list the documents owed their windows: %v", err)
	} else if slices.Contains(owed, document) {
		t.Fatalf("the windowed document %s is still owed its windows: %v", document, owed)
	}
}

// An edit a build without the window path makes during the deploy re-indexes an item's blocks
// and writes no window. The windows left behind hold the old text and cover blocks the item may
// no longer have, and the tick would delete the whole-text row standing behind them. What the
// drift arm reads of such an item is the marker the trigger set on the write of its text, which
// that older build's index cannot clear because it does not name the column
// (refs.ItemsOwedWindows), and one tick rewrites its windows. Skipping the window path is how
// that older build is driven here: the item is re-indexed, and then its marker and the stamp of
// its windows are put back as that build leaves them.
func TestABuildThatWritesNoWindowLeavesWindowsTheTickRewrites(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	comment, _, _ := seedItemsWithBlocks(t, database)
	if tick := reconcileBlockIndex(t, service, ""); tick.ItemBlocks != 3 {
		t.Fatalf("first tick = %#v, want the three items indexed", tick)
	}
	owner := "comment/" + comment
	before := windowsOf(t, database, "comment_block", owner)
	if len(before) == 0 {
		t.Fatalf("the comment holds no window after its own index")
	}
	var stamped string
	if err := database.Pool.QueryRow(ctx, `
		select block_state from embedding_window_states where kind = 'comment_block' and owner_key = $1
	`, owner).Scan(&stamped); err != nil {
		t.Fatalf("read the block state the comment's windows were written from: %v", err)
	}

	// The edit the older build makes: a new body, its blocks indexed again, and its windows and
	// their stamp left as they were.
	if _, err := database.Pool.Exec(ctx, `
		update comments set body = E'One paragraph only, rewritten.', edited_at = now() where id = $1::uuid
	`, comment); err != nil {
		t.Fatalf("edit the comment: %v", err)
	}
	if tick := reconcileBlockIndex(t, service, ""); tick.ItemBlocks != 1 {
		t.Fatalf("the tick that indexed the edit = %#v, want the comment's blocks indexed", tick)
	}
	if _, err := database.Pool.Exec(ctx, `
		update embedding_window_states set block_state = $2 where kind = 'comment_block' and owner_key = $1
	`, owner, stamped); err != nil {
		t.Fatalf("put the comment's window stamp back as the older build leaves it: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		update comment_block_state set windows_owed_at = now() where comment_id = $1::uuid
	`, comment); err != nil {
		t.Fatalf("leave the comment owed its windows as the older build leaves it: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		update embeddings set text_snapshot = $2, content_hash = encode(sha256(convert_to($2, 'UTF8')), 'hex')
		where kind = 'comment_block' and split_part(id, '#', 1) = $1
	`, owner, "The first paragraph.\n\nThe second paragraph."); err != nil {
		t.Fatalf("leave the comment's windows holding its old text: %v", err)
	}

	tick := reconcileBlockIndex(t, service, "")
	if tick.Windows != 1 {
		t.Fatalf("the window arm gave windows to %d owners (%#v), want the comment whose blocks moved on", tick.Windows, tick)
	}
	var text string
	if err := database.Pool.QueryRow(ctx, `
		select string_agg(text_snapshot, '\n' order by id) from embeddings
		where kind = 'comment_block' and split_part(id, '#', 1) = $1
	`, owner).Scan(&text); err != nil {
		t.Fatalf("read the comment's windows: %v", err)
	}
	if !strings.Contains(text, "One paragraph only, rewritten.") || strings.Contains(text, "The second paragraph.") {
		t.Fatalf("the comment's windows hold %q, want the text the edit left", text)
	}
	if next := reconcileBlockIndex(t, service, ""); next.Windows != 0 {
		t.Fatalf("the next tick windowed %d owners (%#v), want none left", next.Windows, next)
	}
}

// A whole-text row stays while its owner's window holds no vector of its current text, and is
// deleted once it holds one: the release before this one keeps finding the item until this one
// can. A window the queue settled as nothing to embed (embedqueue's settledNothingToEmbed: its
// embedded_hash set, its embedding null) is current and stands for nothing, so it retires no
// row either. An issue's row is never retired, its trigger and its kind both standing.
func TestRetirementWaitsForACurrentWindow(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	comment, _, _ := seedItemsWithBlocks(t, database)
	if tick := reconcileBlockIndex(t, service, ""); tick.ItemBlocks != 3 {
		t.Fatalf("first tick = %#v, want the three items indexed", tick)
	}
	// The row the release before this one's trigger wrote for the comment, which it still reads.
	// The two issues' rows are their own trigger's, which this release keeps.
	if _, err := database.Pool.Exec(ctx, `select embeddings_enqueue('comment', $1, 'The first paragraph.')`, comment); err != nil {
		t.Fatalf("write the whole-text row of the comment: %v", err)
	}
	countWholeText := func(stage string, want int) {
		t.Helper()
		var left int
		if err := database.Pool.QueryRow(ctx, `select count(*) from embeddings where kind = 'comment'`).Scan(&left); err != nil || left != want {
			t.Fatalf("%s: whole-text comment rows = %d (%v), want %d", stage, left, err, want)
		}
	}

	if tick := reconcileBlockIndex(t, service, ""); tick.Retired != 0 {
		t.Fatalf("tick with no embedded window retired %d rows (%#v), want none", tick.Retired, tick)
	}
	countWholeText("no embedded window", 1)

	// The window settled with no vector, as the queue settles one whose text has nothing to
	// embed: embedded at its current text, and standing for nothing in a meaning search.
	if _, err := database.Pool.Exec(ctx, `
		update embeddings set embedding = null, embedded_hash = content_hash, model = 'dev:words-v1', embedded_at = now()
		where kind = 'comment_block'
	`); err != nil {
		t.Fatalf("settle the comment's windows with no vector: %v", err)
	}
	if tick := reconcileBlockIndex(t, service, ""); tick.Retired != 0 {
		t.Fatalf("tick behind a window settled with no vector retired %d rows (%#v), want none", tick.Retired, tick)
	}
	countWholeText("a window settled with no vector", 1)

	if _, err := database.Pool.Exec(ctx, `
		update embeddings set embedding = array_fill(0.1::real, array[1536])::vector,
		       embedded_hash = content_hash, model = 'dev:words-v1', embedded_at = now()
		where kind = 'comment_block'
	`); err != nil {
		t.Fatalf("embed the comment's windows: %v", err)
	}
	if tick := reconcileBlockIndex(t, service, ""); tick.Retired != 1 {
		t.Fatalf("tick after the window was embedded retired %d rows (%#v), want the comment's one", tick.Retired, tick)
	}
	countWholeText("a window embedded at its text", 0)
	var issues int
	if err := database.Pool.QueryRow(ctx, `select count(*) from embeddings where kind = 'issue'`).Scan(&issues); err != nil || issues != 2 {
		t.Fatalf("issue rows = %d (%v), want CORE-1's and CORE-2's, which no window replaces", issues, err)
	}
}

// retireVector is a window's vector, as the seeding below writes one.
const retireVector = "array_fill(0.1::real, array[1536])::vector"

// One pass of the retirement deletes exactly the whole-text rows whose owner holds a window
// carrying a vector of its current text, and leaves the rest standing: a row whose window is
// pending, stale or settled with no vector, a row with no window at all, and a row whose
// window sits under an owner key that does not carry this kind's prefix, which names no row of
// this kind. The walk reads the owner keys in order, an owner's windows costing it one place
// however many it holds; a pass stops at the limit it is given and reads on from the cursor it
// left, and passes of the production limit drain the kind.
//
// Both of the pass's statements are held to their plans as well: the delete reaches each row
// through embeddings_pkey keyed on the row's own id, and the walk reads the owner keys through
// embeddings_window_owner. A plan that reads every whole-text row of the kind instead is the
// one INFRA-988 measured at 25 minutes a pass. Every kind of wholeTextOwners is checked, since
// each reads an owner key prefix of its own.
func TestRetirementReachesEachWholeTextRowThroughItsPrimaryKey(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	// Each group is a set of whole-text rows named by what their owner's windows are: the ones
	// marked retiring go, the rest stand. ownerPrefix is the text the group's windows are held
	// under before the row's own id, which is the kind's own prefix for every group but the
	// last.
	type retireGroup struct {
		name         string
		rows         int
		blocks       []string
		embeddedHash string
		embedding    string
		ownerPrefix  string
		retires      bool
	}
	for _, owner := range wholeTextOwners {
		t.Run(owner.kind, func(t *testing.T) {
			exec := func(statement string, args ...any) {
				t.Helper()
				if _, err := database.Pool.Exec(ctx, statement, args...); err != nil {
					t.Fatalf("seed the embeddings: %v", err)
				}
			}
			// "twice" holds two windows of one owner, the shape that must cost one place in
			// the walk and one deletion.
			groups := []retireGroup{
				{"ready", 30, []string{"b1"}, "md5(e.id)", retireVector, owner.ownerPrefix, true},
				{"twice", 10, []string{"b1", "b2"}, "md5(e.id)", retireVector, owner.ownerPrefix, true},
				{"pending", 7, []string{"b1"}, "null", "null", owner.ownerPrefix, false},
				{"stale", 5, []string{"b1"}, "md5('an older text')", retireVector, owner.ownerPrefix, false},
				{"novector", 3, []string{"b1"}, "md5(e.id)", "null", owner.ownerPrefix, false},
				{"naked", 11, nil, "", "", owner.ownerPrefix, false},
			}
			if owner.ownerPrefix != "" {
				// A pass reads the id of a whole-text row out of its window's owner key, by
				// taking this kind's prefix off the front of that key. A key of another shape
				// holds no id of this kind, so it must retire nothing rather than the row of
				// whatever text is left: "otherprefix" holds current windows under another
				// leading text, and its rows stand. The document entry's prefix is empty, so
				// every key of that kind carries it and the group is seeded for the items
				// alone.
				groups = append(groups, retireGroup{
					"otherprefix", 4, []string{"b1"}, "md5(e.id)", retireVector, "elsewhere/", false,
				})
			}
			owed, windowed := 0, 0
			want := map[string]int{}
			for _, group := range groups {
				exec(`
					insert into embeddings (kind, id, text_snapshot, content_hash)
					select $1, $2 || '-' || to_char(n, 'FM000'), 'the whole text', md5($2 || n::text)
					from generate_series(1, $3) n
				`, owner.kind, group.name, group.rows)
				for _, block := range group.blocks {
					exec(fmt.Sprintf(`
						insert into embeddings (kind, id, text_snapshot, content_hash,
						                        embedded_hash, model, embedding, embedded_at, covers)
						select $1, $2 || e.id || '#' || $3, 'a window of ' || e.id, md5(e.id),
						       %s, 'dev:words-v1', %s, now(), array[$3]
						from embeddings e where e.kind = $4 and e.id like $5
					`, group.embeddedHash, group.embedding),
						owner.windowKind, group.ownerPrefix, block, owner.kind, group.name+"-%")
				}
				if len(group.blocks) > 0 {
					windowed += group.rows
				}
				if group.retires {
					owed += group.rows
					continue
				}
				want[group.name] = group.rows
			}
			exec(`analyze embeddings`)

			// The walk's own read, asked for more keys than the fixture can answer with. An
			// owner holding two windows takes one place in it, so the keys are the rows that
			// hold a window, each once.
			keys := ownerKeys(t, database, owner, windowed+1)
			if len(keys) != windowed {
				t.Fatalf("the walk reads %d owner keys of the %s windows, want %d: an owner's windows cost it one place in the walk however many it holds",
					len(keys), owner.windowKind, windowed)
			}
			// The ids a pass hands the delete: the row behind each owner key that carries this
			// kind's prefix (wholeTextOwner.pass).
			ids := make([]string, 0, len(keys))
			for _, key := range keys {
				if id, ok := strings.CutPrefix(key, owner.ownerPrefix); ok {
					ids = append(ids, id)
				}
			}
			requireRetireByPrimaryKey(t, database, owner, ids)
			requireOwnersReadTheWindowIndex(t, database, owner)

			// A limit bounds the pass, and the cursor it leaves is where the next one reads on
			// from: a pass of ten from the start stops at the tenth owner key, and the pass
			// after it stops at the twentieth rather than at the tenth over again.
			const small = 10
			if len(keys) < 2*small {
				t.Fatalf("the fixture holds %d owner keys of the %s windows, too few for two passes of %d",
					len(keys), owner.windowKind, small)
			}
			firstRetired, firstNext, err := owner.pass(ctx, database.Pool, "", small)
			if err != nil {
				t.Fatalf("the first pass of %d over the whole-text %s rows: %v", small, owner.kind, err)
			}
			if firstRetired > small || firstNext != keys[small-1] {
				t.Fatalf("a pass of %d from the start retired %d whole-text %s rows and left the cursor %q, want at most %d rows and the cursor %q, its %dth owner key",
					small, firstRetired, owner.kind, firstNext, small, keys[small-1], small)
			}
			secondRetired, secondNext, err := owner.pass(ctx, database.Pool, firstNext, small)
			if err != nil {
				t.Fatalf("the pass of %d after the cursor %q: %v", small, firstNext, err)
			}
			if secondRetired > small || secondNext != keys[2*small-1] {
				t.Fatalf("a pass of %d from the cursor %q retired %d whole-text %s rows and left the cursor %q, want at most %d rows and the cursor %q, the %dth owner key past the one it started after",
					small, firstNext, secondRetired, owner.kind, secondNext, small, keys[2*small-1], small)
			}

			// Passes of the production limit from the start, each reading on from the cursor
			// the one before it left, until one retires nothing and sends the walk back to the
			// start. The turns are bounded, so a walk that never comes to rest fails here
			// rather than running for ever.
			const turns = 8
			retired, after, rested := firstRetired+secondRetired, "", false
			passes := 0
			for ; passes < turns; passes++ {
				gone, next, err := owner.pass(ctx, database.Pool, after, blockIndexRetireBatch)
				if err != nil {
					t.Fatalf("pass %d of %d over the whole-text %s rows: %v", passes+1, blockIndexRetireBatch, owner.kind, err)
				}
				if passes == 0 && next != "" {
					t.Fatalf("the first pass of %d over the %s owners left the cursor %q, want the empty string: the fixture holds %d owner keys, fewer than the %d this pass asked for, and a slice that comes back short is the end of this kind's owners, which sends the next pass back to the first of them",
						blockIndexRetireBatch, owner.windowKind, next, len(keys), blockIndexRetireBatch)
				}
				retired += gone
				after = next
				if gone == 0 && next == "" {
					rested = true
					break
				}
			}
			if !rested {
				t.Fatalf("%d passes of %d retired %d whole-text %s rows and never came to rest, the last leaving the cursor %q",
					passes, blockIndexRetireBatch, retired, owner.kind, after)
			}
			if retired != owed {
				t.Fatalf("the walk retired %d whole-text %s rows, want the %d the fixture owes: every owner holding a window embedded at its current text, and only those",
					retired, owner.kind, owed)
			}

			left := map[string]int{}
			rows, err := database.Pool.Query(ctx, `
				select split_part(id, '-', 1), count(*) from embeddings where kind = $1 group by 1
			`, owner.kind)
			if err != nil {
				t.Fatalf("read the whole-text %s rows left: %v", owner.kind, err)
			}
			defer rows.Close()
			for rows.Next() {
				var group string
				var count int
				if err := rows.Scan(&group, &count); err != nil {
					t.Fatalf("scan the whole-text %s rows left: %v", owner.kind, err)
				}
				left[group] = count
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("read the whole-text %s rows left: %v", owner.kind, err)
			}
			if !reflect.DeepEqual(left, want) {
				t.Fatalf("the whole-text %s rows left are %v, want %v: every owner holding a window embedded at its current text, and only those, retires", owner.kind, left, want)
			}
		})
	}
}

// The kinds of the retirement share one budget of rows deleted, and a kind the budget leaves
// nothing for keeps the cursor it had. Before the fix this round made, retireWholeText wrote
// tick.Cursor.Retire inside its loop alone, so a kind the budget skipped came out of the tick
// with no cursor and walked its window owners from the start again on the next one. That costs
// the kind a whole lap over its owners, one slice of them every BlockIndexReconcileInterval:
// this walk reads every window owner of a kind, not only the owners that owe a delete.
//
// Three things are stated here: one tick deletes at most blockIndexRetireBatch rows, a kind the
// budget skips comes back with the cursor it was handed, and the cursor a kind that did run
// leaves is where the next tick reads that kind on from.
func TestRetirementSpendsOneBudgetAcrossTheKindsAndKeepsEveryKindsCursor(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	documents, comments := wholeTextOwners[0], wholeTextOwners[1]
	if documents.kind != "document" || comments.kind != "comment" {
		t.Fatalf("the retirement walks %q then %q, want the documents then the comments: this test spends the budget on the first kind and reads the second kind's cursor back",
			documents.kind, comments.kind)
	}

	// The fixture, by kind: a "ready" row's owner holds a window embedded at its current text,
	// so the row retires, and a "stale" row's owner holds a window embedded at an older text,
	// so it stands. "ready" sorts before "stale", so the ready owners come first in the walk.
	// The documents hold more ready rows than one tick's budget, so the first tick spends the
	// whole budget on them and never reaches the comments.
	const (
		documentTail  = 30 // ready document owners past the first tick's budget
		documentStale = 10
		commentReady  = 24
		commentStale  = 6
		commentAhead  = 10 // comment owner keys the cursor handed in has already passed
	)
	documentReady := blockIndexRetireBatch + documentTail
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("seed the embeddings: %v", err)
		}
	}
	// seed writes whole-text rows of this kind named "<group>-NNNN", each with one window of
	// the kind's window kind under the kind's owner prefix, embedded at embeddedHash.
	seed := func(owner wholeTextOwner, group string, rows int, embeddedHash string) {
		t.Helper()
		exec(`
			insert into embeddings (kind, id, text_snapshot, content_hash)
			select $1, $2 || '-' || to_char(n, 'FM0000'), 'the whole text', md5($2 || n::text)
			from generate_series(1, $3) n
		`, owner.kind, group, rows)
		exec(fmt.Sprintf(`
			insert into embeddings (kind, id, text_snapshot, content_hash,
			                        embedded_hash, model, embedding, embedded_at, covers)
			select $1, $2 || e.id || '#b1', 'a window of ' || e.id, md5(e.id),
			       %s, 'dev:words-v1', %s, now(), array['b1']
			from embeddings e where e.kind = $3 and e.id like $4
		`, embeddedHash, retireVector), owner.windowKind, owner.ownerPrefix, owner.kind, group+"-%")
	}
	seed(documents, "ready", documentReady, "md5(e.id)")
	seed(documents, "stale", documentStale, "md5('an older text')")
	seed(comments, "ready", commentReady, "md5(e.id)")
	seed(comments, "stale", commentStale, "md5('an older text')")
	exec(`analyze embeddings`)

	// left is how many whole-text rows of this kind each group has left.
	left := func(owner wholeTextOwner) map[string]int {
		t.Helper()
		rows, err := database.Pool.Query(ctx, `
			select split_part(id, '-', 1), count(*) from embeddings where kind = $1 group by 1
		`, owner.kind)
		if err != nil {
			t.Fatalf("read the whole-text %s rows left: %v", owner.kind, err)
		}
		defer rows.Close()
		counts := map[string]int{}
		for rows.Next() {
			var group string
			var count int
			if err := rows.Scan(&group, &count); err != nil {
				t.Fatalf("scan the whole-text %s rows left: %v", owner.kind, err)
			}
			counts[group] = count
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the whole-text %s rows left: %v", owner.kind, err)
		}
		return counts
	}

	// The cursor the first tick is given for the comments, read from that kind's own owner
	// keys, so it is a key its walk could have left.
	ahead := ownerKeys(t, database, comments, commentAhead)
	if len(ahead) != commentAhead {
		t.Fatalf("the fixture holds %d owner keys of the %s windows, want at least %d to take a cursor from",
			len(ahead), comments.windowKind, commentAhead)
	}
	commentCursor := ahead[commentAhead-1]

	first := BlockIndexTick{}
	if err := service.retireWholeText(ctx, map[string]string{comments.kind: commentCursor}, &first); err != nil {
		t.Fatalf("the first tick of the retirement: %v", err)
	}
	if first.Retired != blockIndexRetireBatch {
		t.Fatalf("the first tick retired %d whole-text rows, want the %d of the budget: the documents alone owe more than one tick may delete",
			first.Retired, blockIndexRetireBatch)
	}
	if got := first.Cursor.Retire[comments.kind]; got != commentCursor {
		t.Fatalf("the first tick left the %s cursor %q, want the %q it was handed: the documents spent the budget, so this kind read nothing and keeps where its walk had reached. An empty cursor sends it back to the first of its window owners, a whole lap over owners that owe no delete.",
			comments.kind, got, commentCursor)
	}
	// The documents did run, so their cursor is the last owner key their pass read.
	read := ownerKeys(t, database, documents, blockIndexRetireBatch)
	if got, want := first.Cursor.Retire[documents.kind], read[len(read)-1]; got != want {
		t.Fatalf("the first tick left the %s cursor %q, want %q, the last of the %d owner keys its pass read",
			documents.kind, got, want, blockIndexRetireBatch)
	}
	if got, want := left(documents), (map[string]int{"ready": documentTail, "stale": documentStale}); !reflect.DeepEqual(got, want) {
		t.Fatalf("the whole-text document rows left after the first tick are %v, want %v: the budget bounds what a tick deletes", got, want)
	}
	if got, want := left(comments), (map[string]int{"ready": commentReady, "stale": commentStale}); !reflect.DeepEqual(got, want) {
		t.Fatalf("the whole-text comment rows left after the first tick are %v, want %v: the documents spent the budget, so this kind read nothing", got, want)
	}

	second := BlockIndexTick{}
	if err := service.retireWholeText(ctx, first.Cursor.Retire, &second); err != nil {
		t.Fatalf("the second tick of the retirement: %v", err)
	}
	// What says each kind read on from its cursor rather than from the start is what the
	// second tick deleted. A window stays when the whole-text row behind it goes, so a walk
	// over the documents starting over would read the same first blockIndexRetireBatch owner
	// keys again, whose rows the first tick deleted, and retire none of them; one reading on
	// from the cursor retires the documentTail rows past them. The comments are the same: a
	// walk from the start would retire the commentAhead rows the handed-in cursor is past as
	// well, and this one leaves them.
	if want := documentTail + commentReady - commentAhead; second.Retired != want {
		t.Fatalf("the second tick retired %d whole-text rows, want %d: the %d document rows past the cursor the first tick left, and the %d comment rows past the cursor that kind kept",
			second.Retired, want, documentTail, commentReady-commentAhead)
	}
	if got, want := left(documents), (map[string]int{"stale": documentStale}); !reflect.DeepEqual(got, want) {
		t.Fatalf("the whole-text document rows left after the second tick are %v, want %v: the tick read on from the cursor the first left and drained the rest", got, want)
	}
	if got, want := left(comments), (map[string]int{"ready": commentAhead, "stale": commentStale}); !reflect.DeepEqual(got, want) {
		t.Fatalf("the whole-text comment rows left after the second tick are %v, want %v: the %d rows the kept cursor is past are the ones this walk has not reached again",
			got, want, commentAhead)
	}
}

// ownerKeys is the owner keys the walk over this kind's windows reads from the start, at most
// limit of them, as one pass reads them (wholeTextOwner.owners).
func ownerKeys(t *testing.T, database *store.Store, owner wholeTextOwner, limit int) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), owner.owners(), "", limit)
	if err != nil {
		t.Fatalf("read the owners of the %s windows: %v", owner.windowKind, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the owners of the %s windows: %v", owner.windowKind, err)
	}
	return keys
}

// explainPlan is the plan of statement under arguments, read in a transaction that is rolled
// back, with sequential scans off. A fixture of a few hundred rows sits in a page or two, so
// what a plan costs here says nothing about what it costs on a production table, and the
// planner's choice between one read of that page and another says as little; the index a read
// can be served through, and the column it is keyed on, are the same either way, and they are
// what these tests pin (explaintest).
func explainPlan(t *testing.T, database *store.Store, statement string, arguments ...any) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("set local enable_seqscan = off: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+statement, arguments...).Scan(&raw); err != nil {
		t.Fatalf("explain the statement: %v", err)
	}
	return explaintest.OnePlan(t, raw)
}

// requireRetireByPrimaryKey fails unless the delete reaches the whole-text rows of this kind
// through embeddings_pkey under a condition naming the row's own id, given the ids one pass
// hands it. An index scan narrowed on the kind alone, which is the read INFRA-988 measured at
// 25 minutes a pass, fails here, and so does a sequential scan, which is no index scan at all
// and so leaves no scan to keep.
//
// What is required of a kept scan is its index and its condition, not the node it is: on a
// fixture whose every row of a kind sits in one page the planner reads them through a bitmap
// of embeddings_pkey, and on a table where a pass's ids are a thousandth of the kind's rows it
// reads them one by one, and both are the same read of the same index under the same
// condition. Holding the node type as well would say nothing more and would need bitmap scans
// turned off to pass.
//
// The statement reads embeddings three times over - the rows it picks and locks, each owner's
// windows, and the rows the delete takes by the key picked holds - so the kind a scan is
// narrowed by, written quoted, is what tells the first from the other two. The quotes are what
// make that safe: 'comment' is no substring of 'comment_block' nor of 'comment/', the window
// kind and the owner prefix the window read writes, and the delete's own scan is keyed on
// picked's columns and names no kind at all.
func requireRetireByPrimaryKey(t *testing.T, database *store.Store, owner wholeTextOwner, ids []string) {
	t.Helper()
	plan := explainPlan(t, database, owner.retire(), ids)
	kept := 0
	for _, scan := range explaintest.IndexScans(t, plan) {
		if !strings.Contains(scan.Cond, "'"+owner.kind+"'") {
			continue
		}
		kept++
		if scan.Index != "embeddings_pkey" || !indexCondOnID.MatchString(scan.Cond) {
			t.Fatalf("the delete reads the whole-text %s rows by a %s under %q, want a read of embeddings_pkey keyed on the row's own id:\n%s",
				owner.kind, scan, scan.Cond, plan)
		}
	}
	if kept == 0 {
		t.Fatalf("the plan reads the whole-text %s rows through no index; the delete picks them:\n%s", owner.kind, plan)
	}
}

// requireOwnersReadTheWindowIndex fails unless the walk over this kind's window owners reads
// them through embeddings_window_owner, keyed on that index's own first key column, and reads
// the embeddings table through no sequential scan. What that catches is a walk the index cannot
// serve at all, such as one naming its window kind as a parameter, which the index's partial
// predicate then refuses. It does not catch the 195 ms plan owners records, a parallel
// sequential scan and a sort, because sequential scans are off here and the planner takes the
// index whenever it can; check k4 of the issue's Verification document measures that plan.
//
// Whether the walk takes the order the index already holds, or reads the kind's windows and
// sorts them, is not stated here: a fixture of a few dozen windows is one page, which the
// planner reads whole through a bitmap of this index and sorts whatever the statement asks
// for, so a plan of it answers that question for the fixture and not for the statement.
func requireOwnersReadTheWindowIndex(t *testing.T, database *store.Store, owner wholeTextOwner) {
	t.Helper()
	plan := explainPlan(t, database, owner.owners(), "", blockIndexRetireBatch)
	subject := "the walk over the " + owner.windowKind + " owners"
	explaintest.NewIndexKeys(database.Pool).RequireIndexKeyed(t, plan,
		"embeddings_window_owner", subject, explaintest.CondRequired)
	if explaintest.SeqScansRelation(t, plan, "embeddings") {
		t.Errorf("%s reads embeddings whole:\n%s", subject, plan)
	}
}

// TestNoDocumentKeepsItsWindowMarkerForEver: windows_owed_at is read through a partial index
// (20261107194001, 20261108070000), so a row that keeps the marker for ever is an entry every
// later pass of the window arm pays for and that nothing ever removes - the cost the marker was
// added to take away, growing with the deployment.
//
// A document that holds no block at a version of its own is the case that keeps it. IndexBlocks
// writes its document_block_versions row with the marker set, and then no arm lists it: the
// block arm does not, since the document is indexed at its version, and
// refs.DocumentsOwedWindows does not, since its semi-join asks for a row of document_blocks,
// which such a document has none of. WriteDocumentWindows is the only statement that clears the
// marker and it runs only for a document the arm listed, so the row stays marked.
//
// A document with no block is ordinary rather than contrived: an issue created with no spec
// gets a spec.md whose markdown is empty. Driven against a Dispatch built from this head, two
// such rows were left marked, the older for 57 minutes across the three ticks its log recorded
// (INFRA-988).
//
// The property this holds the index to is the one a marker must have, whichever way it is kept:
// after the arm has run, no owner is left marked that the arm will not list. Either such a
// document is listed and windowed (its windows being none), or IndexBlocks leaves it unmarked.
func TestNoDocumentKeepsItsWindowMarkerForEver(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	empty := createOwedDocument(t, database, service, "CORE-1", "", "spec", "", "")
	withBlocks := createOwedDocument(t, database, service, "CORE-1", "", "notes", "# Notes\n\nA paragraph.\n", "")

	// Two passes: the first indexes the blocks the documents have, the second has every window
	// the arm will ever write for them behind it.
	reconcileBlockIndex(t, service, "")
	reconcileBlockIndex(t, service, "")

	var marked []string
	rows, err := database.Pool.Query(ctx, `
		select v.artifact_id::text || ' (version ' || v.version || ', ' ||
		       (select count(*) from document_blocks b where b.artifact_id = v.artifact_id) || ' blocks)'
		from document_block_versions v where v.windows_owed_at is not null order by v.artifact_id
	`)
	if err != nil {
		t.Fatalf("read the documents left marked: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("read the documents left marked: %v", err)
		}
		marked = append(marked, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the documents left marked: %v", err)
	}
	if len(marked) > 0 {
		t.Fatalf("the window arm left %d document(s) marked that it will never list: %v\n"+
			"(the document with no block is %s; the one with blocks is %s)",
			len(marked), marked, empty, withBlocks)
	}
}

// indexCondOnID matches an "Index Cond" that keys the id column of the relation the node scans,
// which the plan writes unqualified; a condition naming another relation's id writes it with
// that relation's alias.
var indexCondOnID = regexp.MustCompile(`(^|[^.\w])id =`)
