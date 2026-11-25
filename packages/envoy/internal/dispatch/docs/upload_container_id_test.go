package docs

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// containerListItems is how many items the flat list of this test holds, the count c-container-id
// names: a list minted afresh on an edit of one of them would reparent every other item.
const containerListItems = 1980

// The store half of the untyped container rule (pmdoc's test of the same name holds the four rules
// of the id carry itself): a one-item edit of a flat list of 1,980 items, uploaded through the
// document upload path, leaves the list's id as it was, every unchanged sibling keeps its parent,
// and the edit appends no parent tuple for one, where the commit before this child minted the list
// afresh and appended 3,968 tuples for that edit. A block pointer naming the list still resolves
// after the edit.
//
// The upload is the function the upload route calls: api's uploadArtifactFor hands a document to
// storeArtifact, which writes a new version of an existing document with Docs.ReplaceText, then
// the version row (Ledger.QueueVersion), then Docs.IndexDocumentReferences, in one transaction
// (uploadDocumentVersion). The test drains the block-index reconciliation after each version as
// well, so the edit's blocks are read once indexed, whether the write indexed them or left them to
// the reconciliation (deferBlockIndexBlocks).
func TestAnUploadKeepsAnUntypedContainersIDWhenItsContentsChange(t *testing.T) {
	// A claim whose serves points at any block, so a pointer may name the list.
	pmdoctest.UseBlockSchema(t, pmdoctest.BlockPointerSchema(pmdoc.BlockAttributes{{
		Name: "serves", BlockAttributeSchema: pmdoc.BlockAttributeSchema{Kind: pmdoc.BlockAttributeBlockList},
	}}))
	database, service := blockIndexService(t)
	ctx := context.Background()
	seedPointerAttribute(t, database, "claim", "serves", nil)

	var markdown strings.Builder
	markdown.WriteString("Opening paragraph.\n\n")
	for item := 1; item <= containerListItems; item++ {
		fmt.Fprintf(&markdown, "- Item %04d\n", item)
	}
	markdown.WriteString("\nClosing paragraph.\n")
	list := writeIndexedDocument(t, database, service, "list", markdown.String())
	drainBlockIndex(t, service, "the list's first version")
	before := containerBlocks(t, database, list)
	listID := before.only(t, "bullet_list")
	if items := len(before.children(listID)); items != containerListItems {
		t.Fatalf("the list holds %d items after its first index, want %d", items, containerListItems)
	}
	listAddress := "CORE-1/list#" + listID
	writeIndexedDocument(t, database, service, "checks", claimPointing("c1", "dispatch://CORE-1/artifact/list#b-"+listID, "It holds."))
	requireLinksTo(t, database, "the pointer naming the list, before the edit", listAddress, "serves CORE-1/checks#c1")

	var highest int64
	if err := database.Pool.QueryRow(ctx, `select coalesce(max(seq), 0) from graph_tuples`).Scan(&highest); err != nil {
		t.Fatalf("read the newest tuple: %v", err)
	}
	live, _, err := service.TextWithBlocks(ctx, list)
	if err != nil {
		t.Fatalf("read the list's live text: %v", err)
	}
	edited := strings.Replace(live, "- Item 1000\n", "- Item 1000, revised\n", 1)
	if edited == live {
		t.Fatalf("the list's live text holds no item 1000:\n%.400s", live)
	}
	uploadDocumentVersion(t, database, service, list, edited)
	drainBlockIndex(t, service, "the one-item edit")
	after := containerBlocks(t, database, list)

	if got := after.only(t, "bullet_list"); got != listID {
		t.Errorf("the list took the id %q on a one-item edit, want %q, the id it had: the upload minted the list afresh", got, listID)
	}
	changedBefore, changedAfter := before.paragraph(t, "Item 1000"), after.paragraph(t, "Item 1000, revised")
	if changedAfter == changedBefore {
		t.Errorf("the edited item's paragraph kept the id %q though its text changed, want a fresh one", changedBefore)
	}
	// Every block that stood before the edit but the edited paragraph is kept in its place: the
	// paragraphs around the list, the 1,979 unchanged items and their paragraphs, and the edited
	// item, a container that stays.
	moved := 0
	for id, parent := range before.parents {
		if id == changedBefore {
			continue
		}
		if now, held := after.parents[id]; !held || now != parent {
			moved++
			if moved <= 5 {
				t.Errorf("the block %s, which the edit leaves in place, is held by %q after it, want %q", id, now, parent)
			}
		}
	}
	if moved > 0 {
		t.Errorf("%d blocks the edit leaves in place have another parent, or none, after it", moved)
	}

	rows, err := database.Pool.Query(ctx, `
		select from_id, kind, asserted from graph_tuples
		where seq > $1 and from_kind = $2 and to_kind <> $3
	`, highest, refs.BlockNodeKind, graphstore.KindValue)
	if err != nil {
		t.Fatalf("read the tuples the edit appended: %v", err)
	}
	type appended struct {
		FromID, Kind string
		Asserted     bool
	}
	stated, err := pgx.CollectRows(rows, pgx.RowToStructByPos[appended])
	if err != nil {
		t.Fatalf("scan the tuples the edit appended: %v", err)
	}
	var parents, siblings int
	for _, tuple := range stated {
		if tuple.Kind != graphstore.TuplePartOf {
			continue
		}
		parents++
		id := strings.TrimPrefix(tuple.FromID, "CORE-1/list#")
		// A block the edit made, the changed paragraph's fresh one or a list minted afresh, is
		// no sibling that stood before it.
		if _, stood := before.parents[id]; !stood || id == changedBefore {
			continue
		}
		siblings++
		if siblings <= 5 {
			t.Errorf("the edit appended a parent tuple for the block %s, which it leaves in place (asserted %t)", tuple.FromID, tuple.Asserted)
		}
	}
	t.Logf("the one-item edit of a %d-item list appended %d relation tuples, %d of them parents, %d for a block it leaves in place; the commit before this child appended 3,968 tuples for it",
		containerListItems, len(stated), parents, siblings)
	if siblings > 0 {
		t.Errorf("the edit appended %d parent tuples for blocks it leaves in place, want none", siblings)
	}
	requireLinksTo(t, database, "the pointer naming the list, after the edit", listAddress, "serves CORE-1/checks#c1")
	// The link is listed out of its citing block's own row, whatever its far end holds, so what
	// the pointer names is read as well: the document still holds a block of that address, and it
	// is the list.
	var named string
	if err := database.Pool.QueryRow(ctx, `
		select coalesce((select d.type from document_blocks d
		                 join block_refs r on r.to_kind = $1 and r.to_id = d.node_id
		                 where r.from_node = 'CORE-1/checks#c1' and r.kind = 'serves'), '')
	`, refs.BlockNodeKind).Scan(&named); err != nil {
		t.Fatalf("read the block the pointer names: %v", err)
	}
	if named != "bullet_list" {
		t.Errorf("the pointer at %s names a block the document holds as %q after the edit, want the list", listAddress, named)
	}
}

// uploadDocumentVersion writes markdown as the document's next version as the upload route writes
// one (api's storeArtifact): the document's text replaced by Docs.ReplaceText under the token of
// the version it replaces, the version row, and the version's references indexed by
// Docs.IndexDocumentReferences, whose store write the route appends under the event it records,
// all in one transaction. This helper records no event, so the store write carries the store's
// sentinel, as writeIndexedDocument's does. The document's settlement is cleared after it, as the
// version it wrote settles it.
func uploadDocumentVersion(t *testing.T, database *store.Store, service *Service, artifactID, markdown string) {
	t.Helper()
	alice := model.Actor{Kind: "user", ID: "alice"}
	if err := joinedWrite(service, func(ctx context.Context) error {
		precondition, err := replacementPrecondition(ctx, service, artifactID)
		if err != nil {
			return err
		}
		written, err := service.ReplaceText(ctx, artifactID, markdown, alice, precondition)
		if err != nil {
			return err
		}
		tx, _ := txFromContext(ctx)
		var number int
		if err := tx.QueryRow(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select $1, max(number) + 1, $2, '[{"kind":"user","id":"alice"}]' from artifact_versions where artifact_id = $1
			returning number
		`, artifactID, written).Scan(&number); err != nil {
			return err
		}
		_, write, err := service.IndexDocumentReferences(ctx, artifactID, number, written)
		if err != nil {
			return err
		}
		return causeInWritePhase(ctx, tx, write, graphstore.NoEvent)
	}); err != nil {
		t.Fatalf("upload a version of %s: %v", artifactID, err)
	}
	settle(t, database, artifactID)
}

// containerBlockRows is the document_blocks rows of one document: each block's parent block id,
// the empty string for a block the document holds directly, its type and its excerpt.
type containerBlockRows struct {
	parents  map[string]string
	types    map[string]string
	excerpts map[string]string
}

func containerBlocks(t *testing.T, database *store.Store, artifactID string) containerBlockRows {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, coalesce(parent_block_id, ''), type, excerpt from document_blocks where artifact_id = $1
	`, artifactID)
	if err != nil {
		t.Fatalf("read the blocks of %s: %v", artifactID, err)
	}
	held := containerBlockRows{parents: map[string]string{}, types: map[string]string{}, excerpts: map[string]string{}}
	var id, parent, typ, excerpt string
	if _, err := pgx.ForEachRow(rows, []any{&id, &parent, &typ, &excerpt}, func() error {
		held.parents[id], held.types[id], held.excerpts[id] = parent, typ, excerpt
		return nil
	}); err != nil {
		t.Fatalf("scan the blocks of %s: %v", artifactID, err)
	}
	return held
}

// only is the id of the document's one block of typ.
func (r containerBlockRows) only(t *testing.T, typ string) string {
	t.Helper()
	var found []string
	for id, held := range r.types {
		if held == typ {
			found = append(found, id)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the document holds %d blocks of type %s (%v), want one", len(found), typ, found)
	}
	return found[0]
}

// children is the ids of the blocks parent holds.
func (r containerBlockRows) children(parent string) []string {
	var held []string
	for id, of := range r.parents {
		if of == parent {
			held = append(held, id)
		}
	}
	return held
}

// paragraph is the id of the one paragraph whose excerpt is text.
func (r containerBlockRows) paragraph(t *testing.T, text string) string {
	t.Helper()
	var found []string
	for id, excerpt := range r.excerpts {
		if r.types[id] == "paragraph" && excerpt == text {
			found = append(found, id)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the document holds %d paragraphs reading %q, want one", len(found), text)
	}
	return found[0]
}
