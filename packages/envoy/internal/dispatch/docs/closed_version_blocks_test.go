package docs

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// A block pasted in the browser keeps its source's id until a settlement stamps a new one, so the
// live document can hold one id twice. Version 1 holds "Foo." twice, indexed as rows p1 and p2.
// The live document holds p1, a paste of it that kept the id p1, and p2; in the edited case the
// paste's text is "Bar.". refs.IndexBlocks indexes only the first holder of an id, so the
// version's two "Foo." blocks take p1 and p2, each once.
func TestClosedVersionBlocksCountsOnlyTheFirstHolderOfAnID(t *testing.T) {
	for name, pasted := range map[string]string{"paste": "Foo.", "edited paste": "Bar."} {
		t.Run(name, func(t *testing.T) {
			live, liveBlocks, err := documentBlocks(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{
				paragraphNode("p1", "Foo."), paragraphNode("p1", pasted), paragraphNode("p2", "Foo."),
			}})
			if err != nil {
				t.Fatalf("render the live document: %v", err)
			}
			sum := refs.BlockTextSum(live[liveBlocks[0].From:liveBlocks[0].To])
			indexed := []indexedBlock{{id: "p1", typ: "paragraph", sum: sum}, {id: "p2", typ: "paragraph", sum: sum}}
			markdown, blocks, err := closedVersionBlocks("Foo.\n\nFoo.\n", live, liveBlocks, indexed)
			if err != nil {
				t.Fatalf("match the version's blocks: %v", err)
			}
			var ids []string
			for _, block := range blocks {
				if markdown[block.From:block.To] != live[liveBlocks[0].From:liveBlocks[0].To] {
					t.Fatalf("block %s holds %q, want \"Foo.\"", block.ID, markdown[block.From:block.To])
				}
				ids = append(ids, block.ID)
			}
			if want := []string{"p1", "p2"}; !slices.Equal(ids, want) {
				t.Fatalf("the version's blocks take %v, want %v", ids, want)
			}
		})
	}
}

// heldBlocksOf is the rendering the window arm writes document artifactID's windows from while
// its live text is ahead of its latest version (heldVersionBlocks), read as windowDocument reads
// it: the live document as the server serves it, then, under the document's owner row, the latest
// version's stored markdown and the index's rows.
func heldBlocksOf(t *testing.T, service *Service, artifactID string) (string, []model.ArtifactBlock) {
	t.Helper()
	ctx := context.Background()
	live, liveBlocks, err := service.TextWithBlocks(ctx, artifactID)
	if err != nil {
		t.Fatalf("read the live document %s: %v", artifactID, err)
	}
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read of %s's version: %v", artifactID, err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := lockArtifactOwner(ctx, tx, artifactID); err != nil {
		t.Fatalf("lock the owner of %s: %v", artifactID, err)
	}
	var stored string
	if err := tx.QueryRow(ctx, `
		select markdown from artifact_versions
		where artifact_id = $1 and markdown is not null order by number desc limit 1
	`, artifactID).Scan(&stored); err != nil {
		t.Fatalf("read the latest version of %s: %v", artifactID, err)
	}
	if live == stored {
		t.Fatalf("the live text of %s is its latest version's, want it ahead: %q", artifactID, live)
	}
	markdown, blocks, err := heldVersionBlocks(ctx, tx, artifactID, stored, live, liveBlocks,
		"test: cannot read back a document version")
	if err != nil {
		t.Fatalf("render the latest version of %s: %v", artifactID, err)
	}
	return markdown, blocks
}

// heldParagraphs is the id and the text, as markdown holds it, of each paragraph of blocks, in
// order.
func heldParagraphs(markdown string, blocks []model.ArtifactBlock) (ids, texts []string) {
	for _, block := range blocks {
		if block.Type == "paragraph" {
			ids = append(ids, block.ID)
			texts = append(texts, markdown[block.From:block.To])
		}
	}
	return ids, texts
}

// paragraphNode is a paragraph of a document tree, with block id id and text body.
func paragraphNode(id, body string) *pmdoc.Node {
	return &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: id}, Children: []*pmdoc.Node{{Type: "text", Text: body}}}
}

// closedSpec is a spec whose paragraph cites CORE-2, beside an ask block no settlement has indexed.
const closedSpec = "Ship it after dispatch://CORE-2.\n\n:::ask{#region urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"

// An issue's close, its final sweep and the repair write ask blocks' states into the issue's
// documents and write no version, so such a document is live ahead of its latest version until
// the settlement that saves those states writes the next one. Meanwhile its windows are written
// from that version's own rendering (windowDocument), and a version behind only in ask states is
// matched whole: every block of it takes the id the live document gives it, an untyped block
// included, and the ask block holds the version's text of it, not the state the sweep wrote.
func TestHeldVersionBlocksMatchesEveryBlockOfAVersionBehindOnlyInAskStates(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	spec := createOwedDocument(t, database, service, "CORE-1", "", "spec", closedSpec, "")
	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'CORE-1'`); err != nil {
		t.Fatalf("close CORE-1: %v", err)
	}
	// The close's final sweep, as the repair runs it, under the issue's row: it writes resolved
	// the ask block no settlement has indexed.
	var marked []BlockRef
	if err := joinedWrite(service, func(ctx context.Context) (err error) {
		tx, _ := txFromContext(ctx)
		if _, err := tx.Exec(ctx, `select 1 from issues where key = 'CORE-1' for no key update`); err != nil {
			return fmt.Errorf("lock the issue: %w", err)
		}
		marked, err = service.WriteIssueAskStates(ctx, "CORE-1", nil, model.Actor{Kind: "user", ID: "carol"}, true)
		return err
	}); err != nil {
		t.Fatalf("write the closed issue's ask states: %v", err)
	}
	if want := []BlockRef{{ArtifactID: spec, BlockID: "region"}}; !reflect.DeepEqual(marked, want) {
		t.Fatalf("the sweep marked %+v, want %+v", marked, want)
	}
	closeDocumentRoom(t, service, spec)
	live, liveBlocks, err := service.TextWithBlocks(ctx, spec)
	if err != nil {
		t.Fatalf("read the live document: %v", err)
	}
	if !strings.Contains(live, `state="resolved"`) {
		t.Fatalf("the sweep left live text %q, want the ask block resolved", live)
	}

	markdown, blocks := heldBlocksOf(t, service, spec)
	if len(blocks) == 0 || len(blocks) != len(liveBlocks) {
		t.Fatalf("the version's blocks %#v, want one under each of the live document's %#v", blocks, liveBlocks)
	}
	liveIDs := make(map[string]bool, len(liveBlocks))
	for _, block := range liveBlocks {
		liveIDs[block.ID] = true
	}
	var region string
	for _, block := range blocks {
		if !liveIDs[block.ID] {
			t.Fatalf("the version's block %s is under no live block's id: %#v", block.ID, liveBlocks)
		}
		if block.ID == "region" {
			region = markdown[block.From:block.To]
		}
	}
	if !strings.Contains(region, "Which region?") || strings.Contains(region, `state="resolved"`) {
		t.Fatalf("the ask block holds %q, want version 1's text of it, with no resolved state", region)
	}
}

// A block an edit changed keeps its row, with the version's text. Version 1 holds two untyped
// paragraphs, indexed at version 1. An edit no settlement has versioned yet then changes the
// plain paragraph's text: the same block, with new text. The version's rendering gives each
// paragraph the id of the row that holds version 1's text of it, so the edited paragraph keeps
// its id with version 1's text, and no block holds the edit, which no version holds.
func TestHeldVersionBlocksGivesAnEditedUntypedBlockItsRowWithTheVersionsText(t *testing.T) {
	database, service := blockIndexService(t)
	spec := createOwedDocument(t, database, service, "CORE-1", "", "spec", "Ship it after dispatch://CORE-2.\n\nPlain paragraph.\n", "")
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 || tick.OwedDocuments != 0 {
		t.Fatalf("indexing version 1: tick = %#v", tick)
	}
	cited := holderOf(t, service, spec, "dispatch://CORE-2")
	edited := holderOf(t, service, spec, "Plain paragraph.")

	replaced := false
	var replace func(node *pmdoc.Node)
	replace = func(node *pmdoc.Node) {
		if node.Type == "text" && strings.Contains(node.Text, "Plain paragraph.") {
			node.Text, replaced = strings.Replace(node.Text, "Plain paragraph.", "Edited paragraph.", 1), true
		}
		for _, child := range node.Children {
			replace(child)
		}
	}
	editLiveTree(t, service, spec, func(tree *pmdoc.Node) *pmdoc.Node {
		replace(tree)
		return tree
	})
	if !replaced {
		t.Fatalf("no text of %s holds the plain paragraph", spec)
	}
	waitForLandedAppends(t, service, spec)
	closeDocumentRoom(t, service, spec)
	if holder := holderOf(t, service, spec, "Edited paragraph."); holder != edited {
		t.Fatalf("the edit is in block %s, want the plain paragraph's block %s edited", holder, edited)
	}

	markdown, blocks := heldBlocksOf(t, service, spec)
	ids, texts := heldParagraphs(markdown, blocks)
	if want := []string{cited, edited}; !slices.Equal(ids, want) {
		t.Fatalf("the version's paragraphs take %v, want %v", ids, want)
	}
	if !strings.Contains(texts[1], "Plain paragraph.") {
		t.Fatalf("the edited paragraph %s holds %q, want version 1's text of it", edited, texts[1])
	}
	for _, block := range blocks {
		if strings.Contains(markdown[block.From:block.To], "Edited paragraph.") {
			t.Fatalf("block %s holds the edit, which no version holds: %q", block.ID, markdown[block.From:block.To])
		}
	}
}

// Identical paragraphs keep their own rows. Version 1 holds three "Same." paragraphs and an
// "Other." one, indexed at version 1. An edit no settlement has versioned yet then inserts a new
// "Same." paragraph after "Other.", before the third "Same.". Each paragraph of the version takes
// the id of its own row, in version 1's order, and the inserted paragraph, which no version holds,
// takes none.
func TestHeldVersionBlocksGivesIdenticalParagraphsTheirOwnRows(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	spec := createOwedDocument(t, database, service, "CORE-1", "", "spec", "Same.\n\nSame.\n\nOther.\n\nSame.\n", "")
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 || tick.OwedDocuments != 0 {
		t.Fatalf("indexing version 1: tick = %#v", tick)
	}
	_, firstBlocks, err := service.TextWithBlocks(ctx, spec)
	if err != nil {
		t.Fatalf("read the live document: %v", err)
	}
	var held []string
	for _, block := range firstBlocks {
		if block.Type == "paragraph" {
			held = append(held, block.ID)
		}
	}
	if len(held) != 4 {
		t.Fatalf("version 1's paragraphs %#v, want 4", firstBlocks)
	}
	const inserted = "5a3e0c1d-7b2f-4e6a-9c8d-1f2e3d4c5b6a"
	editLiveTree(t, service, spec, func(tree *pmdoc.Node) *pmdoc.Node {
		for index, child := range tree.Children {
			if id, _ := child.Attrs[pmdoc.BlockIDAttr].(string); id == held[2] {
				tree.Children = slices.Insert(tree.Children, index+1, paragraphNode(inserted, "Same."))
				return tree
			}
		}
		t.Fatalf("no paragraph %s in the live tree", held[2])
		return tree
	})
	waitForLandedAppends(t, service, spec)
	closeDocumentRoom(t, service, spec)
	_, liveBlocks, err := service.TextWithBlocks(ctx, spec)
	if err != nil {
		t.Fatalf("read the live document: %v", err)
	}
	var order []string
	for _, block := range liveBlocks {
		if block.Type == "paragraph" {
			order = append(order, block.ID)
		}
	}
	if want := []string{held[0], held[1], held[2], inserted, held[3]}; !slices.Equal(order, want) {
		t.Fatalf("live paragraphs %v, want %v", order, want)
	}

	markdown, blocks := heldBlocksOf(t, service, spec)
	ids, texts := heldParagraphs(markdown, blocks)
	if !slices.Equal(ids, held) {
		t.Fatalf("the version's paragraphs take %v, want their own rows %v", ids, held)
	}
	if want := []string{"Same.", "Same.", "Other.", "Same."}; !slices.Equal(texts, want) {
		t.Fatalf("the version's paragraphs hold %q, want %q", texts, want)
	}
}

// With no row to name a block - a document none of whose blocks the index holds yet, as the
// block index's deploy leaves one - a text held more than once is matched in order only when the
// version and the live document hold the same number of copies of it. Otherwise no copy of it is
// matched until a settlement versions them, so a copy typed since the version never takes an
// original's id. Version 1 holds "Same." three times and "Other." once. In "counts differ" the
// live document holds a fourth "Same.", typed before the version's third, and only "Other." is
// matched. In "counts agree" a paragraph of new text is all that sets the live document apart,
// and each paragraph of the version takes the id of its live copy, in order.
func TestClosedVersionBlocksMatchesRepeatedTextWithNoRowOnlyWhenItsCountsAgree(t *testing.T) {
	const stored = "Same.\n\nSame.\n\nOther.\n\nSame.\n"
	for _, test := range []struct {
		name       string
		live       []*pmdoc.Node
		ids, texts []string
	}{{
		name: "counts differ",
		live: []*pmdoc.Node{
			paragraphNode("s1", "Same."), paragraphNode("s2", "Same."), paragraphNode("o", "Other."),
			paragraphNode("typed", "Same."), paragraphNode("s3", "Same."),
		},
		ids:   []string{"o"},
		texts: []string{"Other."},
	}, {
		name: "counts agree",
		live: []*pmdoc.Node{
			paragraphNode("s1", "Same."), paragraphNode("s2", "Same."), paragraphNode("o", "Other."),
			paragraphNode("s3", "Same."), paragraphNode("typed", "Typed since the version."),
		},
		ids:   []string{"s1", "s2", "o", "s3"},
		texts: []string{"Same.", "Same.", "Other.", "Same."},
	}} {
		t.Run(test.name, func(t *testing.T) {
			live, liveBlocks, err := documentBlocks(&pmdoc.Node{Type: "doc", Children: test.live})
			if err != nil {
				t.Fatalf("render the live document: %v", err)
			}
			if len(liveBlocks) != len(test.live) {
				t.Fatalf("the live document renders blocks %#v, want %d", liveBlocks, len(test.live))
			}
			markdown, blocks, err := closedVersionBlocks(stored, live, liveBlocks, nil)
			if err != nil {
				t.Fatalf("match the version's blocks: %v", err)
			}
			ids, texts := heldParagraphs(markdown, blocks)
			if !slices.Equal(ids, test.ids) || !slices.Equal(texts, test.texts) {
				t.Fatalf("the version's paragraphs take %v holding %q, want %v holding %q", ids, texts, test.ids, test.texts)
			}
		})
	}
}

// A version's blocks keep their rows when edits cross. Version 1 holds the paragraphs "Intro.",
// "Moved.", "Same.", "Same.", "Retyped.", "Middle.", "Same." and "End.", indexed at version 1.
// Edits no settlement has versioned yet then move "Moved." to the end, delete "Retyped." and type
// in its place a paragraph of the same text under a new id, and type "Typed since the version."
// under a new id after "Middle.". In mode rows the index holds version 1's rows; in mode deploy
// it holds none, as migration 0121 leaves a database.
//
// In mode rows each paragraph of the version takes its own row's id with that row's text - the
// moved paragraph, each "Same." and the deleted "Retyped." among them - and the retyped
// paragraph, which no version holds under its id, takes none. In mode deploy text identity is all
// there is: each paragraph of the version takes the id of the live paragraph holding its text,
// the retyped one among them, and the moved paragraph, whose text is held once on each side, is
// matched by that text across the blocks it moved past. In neither does the typed paragraph take
// a block of the version.
func TestHeldVersionBlocksMatchesAVersionAcrossCrossedEdits(t *testing.T) {
	for _, mode := range []string{"rows", "deploy"} {
		t.Run(mode, func(t *testing.T) {
			database, service := blockIndexService(t)
			ctx := context.Background()
			spec := createOwedDocument(t, database, service, "CORE-1", "", "spec",
				"Intro.\n\nMoved.\n\nSame.\n\nSame.\n\nRetyped.\n\nMiddle.\n\nSame.\n\nEnd.\n", "")
			if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 || tick.OwedDocuments != 0 {
				t.Fatalf("indexing version 1: tick = %#v", tick)
			}
			_, firstBlocks, err := service.TextWithBlocks(ctx, spec)
			if err != nil {
				t.Fatalf("read the live document: %v", err)
			}
			var held []string
			for _, block := range firstBlocks {
				if block.Type == "paragraph" {
					held = append(held, block.ID)
				}
			}
			if len(held) != 8 {
				t.Fatalf("version 1's paragraphs %#v, want 8", firstBlocks)
			}
			firstTexts := map[string]string{}
			for _, id := range held {
				var firstText string
				if err := database.Pool.QueryRow(ctx, `
					select text from document_block_texts where artifact_id = $1 and block_id = $2 and version = 1
				`, spec, id).Scan(&firstText); err != nil || firstText == "" {
					t.Fatalf("read block %s's text at version 1: %q (%v)", id, firstText, err)
				}
				firstTexts[id] = firstText
			}
			moved, retyped, middle := held[1], held[4], held[5]
			const retypedAgain = "0b6f2c4e-8a1d-4f3b-9e7c-2d5a6b8c9e01"
			const typedAfter = "7c9d1e2f-3a4b-4c5d-8e6f-9a0b1c2d3e4f"
			editLiveTree(t, service, spec, func(tree *pmdoc.Node) *pmdoc.Node {
				var movedNode *pmdoc.Node
				children := make([]*pmdoc.Node, 0, len(tree.Children)+1)
				for _, child := range tree.Children {
					id, _ := child.Attrs[pmdoc.BlockIDAttr].(string)
					switch id {
					case moved:
						movedNode = child
						continue
					case retyped:
						child = paragraphNode(retypedAgain, "Retyped.")
					}
					children = append(children, child)
					if id == middle {
						children = append(children, paragraphNode(typedAfter, "Typed since the version."))
					}
				}
				if movedNode == nil {
					t.Fatalf("no paragraph %s in the live tree", moved)
				}
				tree.Children = append(children, movedNode)
				return tree
			})
			waitForLandedAppends(t, service, spec)
			closeDocumentRoom(t, service, spec)
			if mode == "deploy" {
				// The document has no index at all, as migration 0121 leaves a database, and is owed.
				for _, statement := range []string{
					`delete from block_refs where artifact_id = $1`,
					`delete from document_block_texts where artifact_id = $1`,
					`delete from document_blocks where artifact_id = $1`,
					`delete from document_indexed_versions where artifact_id = $1`,
					`update document_block_versions set version = null where artifact_id = $1`,
				} {
					if _, err := database.Pool.Exec(ctx, statement, spec); err != nil {
						t.Fatalf("clear the document's index (%s): %v", statement, err)
					}
				}
			}
			live, liveBlocks, err := service.TextWithBlocks(ctx, spec)
			if err != nil {
				t.Fatalf("read the live document: %v", err)
			}
			var order []string
			liveTexts := map[string]string{}
			for _, block := range liveBlocks {
				if block.Type == "paragraph" {
					order = append(order, block.ID)
					liveTexts[block.ID] = live[block.From:block.To]
				}
			}
			if want := []string{held[0], held[2], held[3], retypedAgain, middle, typedAfter, held[6], held[7], moved}; !slices.Equal(order, want) {
				t.Fatalf("live paragraphs %v, want %v", order, want)
			}

			markdown, blocks := heldBlocksOf(t, service, spec)
			ids, texts := heldParagraphs(markdown, blocks)
			if mode == "rows" {
				if !slices.Equal(ids, held) {
					t.Fatalf("the version's paragraphs take %v, want their own rows %v", ids, held)
				}
				for index, id := range ids {
					if texts[index] != firstTexts[id] {
						t.Errorf("the version's paragraph %s holds %q, want its row's text %q", id, texts[index], firstTexts[id])
					}
				}
				return
			}
			if want := []string{held[0], moved, held[2], held[3], retypedAgain, middle, held[6], held[7]}; !slices.Equal(ids, want) {
				t.Fatalf("the version's paragraphs take %v, want the live ids holding their texts %v", ids, want)
			}
			for index, id := range ids {
				if texts[index] == "" || texts[index] != liveTexts[id] {
					t.Errorf("the version's paragraph %q is under live id %s, which holds %q", texts[index], id, liveTexts[id])
				}
			}
		})
	}
}
