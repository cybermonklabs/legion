package grapherase

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// An erase takes the erased block's words out of the text value of every block that holds it
// (R5 of dispatch://INFRA-1149; D9 of dispatch://INFRA-1253's plan: a block's text value is its
// whole slice of its version, the blocks inside it included). A document of a block outer holding
// a block inner is indexed through the block index (refs.IndexBlocks), which states each block's
// text value, and inner is erased as an operator erases it. Before the erase the listing names the
// outer block's text value tuple as one the run rewrites, and the outer value holds the words;
// after it no value tuple of the store holds them, the outer block's text value holds the erase's
// marker where they stood and still its own words, its document_block_texts row agrees with it,
// and the store census reads zero.
func TestTheEraseTakesABlocksWordsOutOfTheTextValueOfEveryBlockHoldingIt(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	const (
		secret   = "The secret words."
		markdown = "Outer words.\n" + secret + "\n"
	)
	document := seedIssueDocument(t, database, "CORE", coreDocumentID, markdown)
	// The issue, the document and its version are written by SQL; the backfill gives each its
	// node, as it does for rows a build before the store wrote, so the census over the store
	// after the erase reads what the erase did and nothing the fixture left.
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the fixture's nodes: %v", err)
	}
	at := len("Outer words.\n")
	blocks := []model.ArtifactBlock{
		{ID: "outer", Type: "paragraph", From: 0, To: len(markdown)},
		{ID: "inner", Type: "paragraph", From: at, To: at + len(secret)},
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		var versionAt time.Time
		if err := tx.QueryRow(ctx, `select created_at from artifact_versions where artifact_id = $1::uuid and number = 1`,
			document.artifactID).Scan(&versionAt); err != nil {
			return err
		}
		_, won, write, err := refs.IndexBlocks(ctx, tx, document.artifactID, refs.OneVersion(document.artifactID, 1, versionAt), markdown, blocks, nil, nil, refs.TypedFacts{})
		if err != nil {
			return err
		}
		if !won {
			t.Fatalf("the index of version 1 did not stand")
		}
		return graphstore.Caused(ctx, tx, write, graphstore.NoEvent)
	}); err != nil {
		t.Fatalf("index the document through the block index: %v", err)
	}
	outer, inner := document.refKey+"#outer", document.refKey+"#inner"
	holding := func() []string {
		t.Helper()
		rows, err := database.Pool.Query(ctx, `
			select from_kind || ':' || from_id || ' ' || kind from graph_tuples
			where to_kind = $1 and position($2 in to_id) > 0 order by seq`, graphstore.KindValue, secret)
		if err != nil {
			t.Fatalf("read the value tuples holding the words: %v", err)
		}
		held, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the value tuples holding the words: %v", err)
		}
		return held
	}
	if held := holding(); !strings.Contains(strings.Join(held, "\n"), "block:"+outer+" text") {
		t.Fatalf("before the erase the value tuples holding the words are %v, want the outer block's text among them: the case proves nothing", held)
	}

	listing := dryRun(t, database, refs.BlockNodeKind, inner)
	if !listed(listing, "redact graph_tuples", "block:"+outer+" text") {
		t.Errorf("the erase's listing names no rewrite of the outer block's text value:\n%s", strings.Join(listing.Listing, "\n"))
	}
	for _, line := range listing.Listing {
		if strings.HasPrefix(line, "leave ") && strings.Contains(line, "graph_tuples") {
			t.Errorf("the erase's listing leaves a tuple holding the words, which derives from the erased block: %s", line)
		}
	}
	requireErased(t, database, refs.BlockNodeKind, inner)

	if held := holding(); len(held) != 0 {
		t.Errorf("after the erase the value tuples %v still hold the erased words", held)
	}
	text, found, err := graphstore.CurrentValue(ctx, database.Pool, refs.BlockNodeKind, outer, graphstore.AttributeText)
	if err != nil || !found {
		t.Fatalf("read the outer block's text value after the erase: found %t, %v", found, err)
	}
	var row string
	if err := database.Pool.QueryRow(ctx, `
		select text from document_block_texts where artifact_id = $1::uuid and block_id = 'outer' order by version desc limit 1
	`, document.artifactID).Scan(&row); err != nil {
		t.Fatalf("read the outer block's document_block_texts row: %v", err)
	}
	if !strings.HasPrefix(text, "Outer words.\n") || strings.Contains(text, secret) || text != row {
		t.Errorf("after the erase the outer block's text value is %q and its document_block_texts row %q, want one text, its own words kept and the erased ones gone",
			text, row)
	}
	requireCensusZero(t, database, "after the erase of a block another holds")
}

// The erase of a block of an issue room (T12a of dispatch://INFRA-1253's plan) reaches what derives
// from it: the block is addressed under the issue's tree artifact, whose rows hold its texts and
// whose stored history is the room's, and its own document - the spec, whose fragment held it - is
// the one whose versions it stood in. Block gone, a version of the room dropped, has no
// issue_room_blocks row left, and its document is the one its last in_document names. Each erase
// lists the spec's version holding its text, the tree artifact's text rows and the room's history,
// and the confirmed run seeds the room's history with every other fragment as it stood: the
// Verification document's words stay in the room.
func TestTheEraseOfARoomBlockReachesItsDocumentsVersionsItsTextsAndTheRoomsHistory(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	var spec, verification, tree, specRef, treeRef string
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'Issue room', '{"kind":"user","id":"alice"}', 'U')`); err != nil {
			return err
		}
		create := func(slug, kind string, primary bool, id, refKey *string) error {
			return tx.QueryRow(ctx, `
				insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
				values ('CORE-1', 'CORE', $1, $1, $2, $3, '{"kind":"user","id":"alice"}')
				returning id::text, ref_key`, slug, kind, primary).Scan(id, refKey)
		}
		var verificationRef string
		if err := create("spec", "doc", true, &spec, &specRef); err != nil {
			return err
		}
		if err := create("verification-md", "doc", false, &verification, &verificationRef); err != nil {
			return err
		}
		if err := create("tree", "tree", false, &tree, &treeRef); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			with held as (insert into issue_trees (issue_key, artifact_id) values ('CORE-1', $1::uuid)),
			moved as (insert into document_rooms (artifact_id, room) values ($2::uuid, $1::uuid), ($3::uuid, $1::uuid))
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select v.id, v.number, v.markdown, '[{"kind":"user","id":"alice"}]'
			from unnest($4::uuid[], $5::int[], $6::text[]) as v (id, number, markdown)`,
			tree, spec, verification,
			[]string{tree, spec, spec, verification}, []int32{1, 1, 2, 1},
			[]string{"", "Room secret words.\n\nGone words.\n", "Room secret words.\n", "Check words.\n"})
		return err
	}); err != nil {
		t.Fatalf("seed the issue room: %v", err)
	}
	// The room's stored history, as the room holds it after block gone was taken out: the
	// Verification document's fragment and the spec's, each holding one paragraph.
	paragraph := func(id, text string) *pmdoc.Node {
		t.Helper()
		held, err := pmdoc.Parse(text + "\n")
		if err != nil {
			t.Fatalf("parse %q: %v", text, err)
		}
		if held.Children[0].Attrs == nil {
			held.Children[0].Attrs = pmdoc.Attrs{}
		}
		held.Children[0].Attrs[pmdoc.BlockIDAttr] = id
		return held
	}
	history := crdt.New()
	checkFragment, secretFragment := history.GetXmlFragment(verification), history.GetXmlFragment(spec)
	checkTree, secretTree := paragraph("check", "Check words."), paragraph("secret", "Room secret words.")
	var seeded error
	history.Transact(func(txn *crdt.Transaction) {
		if seeded = pmdoc.Update(txn, checkFragment, checkTree); seeded == nil {
			seeded = pmdoc.Update(txn, secretFragment, secretTree)
		}
	})
	if seeded != nil {
		t.Fatalf("write the room's history: %v", seeded)
	}
	if _, err := database.Pool.Exec(ctx, `insert into doc_updates (artifact_id, version, update, content_changed) values ($1, 1, $2, true)`,
		tree, crdt.EncodeStateAsUpdateV1(history, nil)); err != nil {
		t.Fatalf("store the room's history: %v", err)
	}
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the fixture's nodes: %v", err)
	}
	// Two passes of the room's index under the tree artifact: the first holds block gone in the
	// spec's fragment, the second, at the spec's version 2, has dropped it.
	index := func(specVersion int, parts [][3]string) {
		t.Helper()
		var markdown strings.Builder
		var blocks []model.ArtifactBlock
		for _, part := range parts {
			if markdown.Len() > 0 {
				markdown.WriteString("\n\n")
			}
			from := markdown.Len()
			markdown.WriteString(part[1])
			blocks = append(blocks, model.ArtifactBlock{ID: part[0], Type: "paragraph", From: from, To: markdown.Len(), Document: part[2]})
		}
		if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			versions := refs.Versions{}
			for document, number := range map[string]int{tree: 1, spec: specVersion, verification: 1} {
				var at time.Time
				if err := tx.QueryRow(ctx, `select created_at from artifact_versions where artifact_id = $1::uuid and number = $2`,
					document, number).Scan(&at); err != nil {
					return err
				}
				versions[document] = refs.Version{Number: number, At: at}
			}
			_, won, write, err := refs.IndexBlocks(ctx, tx, tree, versions, markdown.String(), blocks, nil, nil, refs.TypedFacts{})
			if err != nil {
				return err
			}
			if !won {
				t.Fatalf("the room's index at the spec's version %d did not stand", specVersion)
			}
			return graphstore.Caused(ctx, tx, write, graphstore.NoEvent)
		}); err != nil {
			t.Fatalf("index the room at the spec's version %d: %v", specVersion, err)
		}
	}
	index(1, [][3]string{{"check", "Check words.", verification}, {"secret", "Room secret words.", spec}, {"gone", "Gone words.", spec}})
	index(2, [][3]string{{"check", "Check words.", verification}, {"secret", "Room secret words.", spec}})

	// erased runs the erase of block id of the room as an operator does and holds it to the spec's
	// version it lists, the tree artifact's text rows of the block and the room's history.
	erased := func(id string, version int) {
		t.Helper()
		address := treeRef + "#" + id
		listing := dryRun(t, database, refs.BlockNodeKind, address)
		if want := []ErasedVersion{{Document: specRef, Issue: "CORE-1", Project: "CORE", Version: version}}; !slices.Equal(listing.Versions, want) {
			t.Errorf("the erase of %s lists the versions %+v, want the spec's version %d: %+v", address, listing.Versions, version, want)
		}
		if !listed(listing, "delete document_block_texts", address) {
			t.Errorf("the erase of %s lists no text row of the block under the tree artifact:\n%s", address, strings.Join(listing.Listing, "\n"))
		}
		if len(listing.Documents) != 1 || listing.Documents[0].Document != treeRef {
			t.Errorf("the erase of %s replaces the histories %+v, want the room's, %s", address, listing.Documents, treeRef)
		}
		requireErased(t, database, refs.BlockNodeKind, address)
	}
	erased("gone", 1)
	erased("secret", 2)

	loaded, err := docs.NewPgVersioned(database).Load(ctx, tree)
	if err != nil {
		t.Fatalf("load the room's history after the erases: %v", err)
	}
	room := crdt.New()
	if err := crdt.ApplyUpdateV1(room, loaded.Update, nil); err != nil {
		t.Fatalf("decode the room's history after the erases: %v", err)
	}
	text := func(fragment string) string {
		t.Helper()
		held, err := pmdoc.Read(room.GetXmlFragment(fragment))
		if err != nil {
			t.Fatalf("read the fragment %s: %v", fragment, err)
		}
		rendered, err := pmdoc.Render(held)
		if err != nil {
			t.Fatalf("render the fragment %s: %v", fragment, err)
		}
		return rendered
	}
	if got := text(verification); got != "Check words.\n" {
		t.Errorf("the Verification document's fragment of the room reads %q after the erases, want its words as they stood", got)
	}
	if got := text(spec); strings.Contains(got, "Room secret words.") {
		t.Errorf("the spec's fragment of the room still reads %q after the erase of its block", got)
	}
}
