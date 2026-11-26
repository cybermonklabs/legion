package refs

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// indexRoomCopies indexes the room whose tree artifact is tree over versions as version, with no
// typed facts, and states each of copies, a block of the room by id, copied from the ask its value
// names, as settlement's claim leaves the copy's node (docs' claimCopies).
func indexRoomCopies(t *testing.T, tx pgx.Tx, tree, treeRef string, versions Versions, version typedVersion, copies map[string]string) {
	t.Helper()
	ctx := context.Background()
	if _, won, _, _, err := IndexBlocks(ctx, tx, tree, versions, version.markdown, version.blocks, version.located, version.pointers, "", TypedFacts{}); err != nil || !won {
		t.Fatalf("index the room: won %t, %v", won, err)
	}
	_, newest := versions.Newest(tree)
	var claims graphstore.Uncaused
	for block, ask := range copies {
		claims.Tuples = append(claims.Tuples, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
			FromKind: BlockNodeKind, FromID: treeRef + "#" + block, Kind: graphstore.TupleCopiedFrom,
			ToKind: graphstore.KindAsk, ToID: ask, At: newest.At, Who: roomAuthor, Asserted: true,
		}})
	}
	if err := graphstore.Caused(ctx, tx, claims, graphstore.NoEvent); err != nil {
		t.Fatalf("state the copies' copied_from: %v", err)
	}
}

// The question map of a document of an issue room reads a copy's source off its block node's
// copied_from tuple before its opening line, as a document that is its own room's does
// (TestAQueuedVersionsCopyResolvesToTheSourceItsLineNames): the block rows of the room's every
// fragment are keyed by the room's tree artifact, and not by the document whose fragment holds
// them (DocumentRoomSQL). The Verification document's stored version holds the copy c, whose line
// names the second source, and the copy d, whose line names none; both nodes state the first
// source, so the map answers the first source for each. A read keyed by the document itself
// finds no row, and answers c's line and nothing of d.
func TestTheQuestionMapOfARoomsDocumentReadsItsCopiesNodes(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	_, _, verification, tree, treeRef := roomFixture(t, tx)
	first := insertScopeAsk(t, tx, "WIND-1", "", nil)
	second := insertScopeAsk(t, tx, "WIND-1", "", nil)
	markdown := `:::ask{#c copied_from="` + second + `"}` + "\nWhich?\n:::\n\n:::ask{#d}\nWhich too?\n:::\n"
	treeAt := writeStoredVersion(t, tx, tree, 1, "")
	verificationAt := writeStoredVersion(t, tx, verification, 1, markdown)
	indexRoomCopies(t, tx, tree, treeRef, Versions{tree: {Number: 1, At: treeAt}, verification: {Number: 1, At: verificationAt}},
		renderRoomVersion(t, tree, roomFragment{verification, markdown}), map[string]string{"c": first, "d": first})

	got, err := questionMap(ctx, tx, nil, verification)
	if err != nil {
		t.Fatalf("the question map of the Verification document: %v", err)
	}
	ref := text.Extract("dispatch://WIND-1/ask/"+first, "")[0]
	if want := map[string]text.Ref{"c": ref, "d": ref}; !reflect.DeepEqual(got, want) {
		t.Errorf("the question map of the room's Verification document = %#v; want %#v, the source both copies' nodes state", got, want)
	}
}

// A settlement of an issue room retracts the copied_from of every block of the room the read of
// its standing copies names that the settlement does not claim (docs' claimCopies). A retired
// document's fragment left the room, so no settlement claims its copy, and every pass over the
// room keeps its blocks as the retirement left them (BlockIndexing.retained): the read leaves the
// retired Verification document's copy v out, and names the spec's copy c.
func TestTheStandingCopiesOfARoomLeaveARetiredFragmentsCopyOut(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	spec, _, verification, tree, treeRef := roomFixture(t, tx)
	source := insertScopeAsk(t, tx, "WIND-1", "", nil)
	specCopy := ":::ask{#c copied_from=\"" + source + "\"}\nWhich?\n:::\n"
	verificationCopy := ":::ask{#v copied_from=\"" + source + "\"}\nWhich?\n:::\n"
	treeAt := writeStoredVersion(t, tx, tree, 1, "")
	specAt := writeStoredVersion(t, tx, spec, 1, specCopy)
	verificationAt := writeStoredVersion(t, tx, verification, 1, verificationCopy)
	indexRoomCopies(t, tx, tree, treeRef, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 1, At: specAt}, verification: {Number: 1, At: verificationAt},
	}, renderRoomVersion(t, tree, roomFragment{spec, specCopy}, roomFragment{verification, verificationCopy}),
		map[string]string{"c": source, "v": source})
	standing, err := DocumentCopiedFrom(ctx, tx, tree)
	if err != nil {
		t.Fatalf("read the room's standing copies: %v", err)
	}
	if len(standing) != 2 {
		t.Fatalf("the room's standing copies before the retirement are %#v, want c and v, or this test proves nothing", standing)
	}
	if _, err := tx.Exec(ctx, `update artifacts set retired_at = now(), role = null where id = $1::uuid`, verification); err != nil {
		t.Fatalf("retire the Verification document: %v", err)
	}

	standing, err = DocumentCopiedFrom(ctx, tx, tree)
	if err != nil {
		t.Fatalf("read the room's standing copies: %v", err)
	}
	if want := map[string]BlockCopiedFrom{"c": {Node: treeRef + "#c", Ask: source}}; !reflect.DeepEqual(standing, want) {
		t.Errorf("the room's standing copies after the Verification document's retirement are %#v, want %#v alone", standing, want)
	}
}
