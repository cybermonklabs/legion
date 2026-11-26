package graphcensus

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// unresolvedOf is the counter of unresolved citations' read of the database, each citation as the
// node that cites and the address it cites, sorted.
func unresolvedOf(t *testing.T, database *store.Store) []string {
	t.Helper()
	report, err := UnresolvedCitations(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("count the unresolved citations: %v", err)
	}
	cited := make([]string, len(report.Citations))
	for index, citation := range report.Citations {
		cited[index] = citation.SourceKind + ":" + citation.SourceID + " -> " + citation.Target
	}
	slices.Sort(cited)
	return cited
}

// requireUnresolved fails t unless the counter reads the citations want, in any order; when says
// what the store holds then.
func requireUnresolved(t *testing.T, database *store.Store, when string, want ...string) {
	t.Helper()
	sorted := slices.Sorted(slices.Values(want))
	if got := unresolvedOf(t, database); !slices.Equal(got, sorted) {
		t.Fatalf("%s the counter reads %d unresolved citation(s) %v, want %d: %v", when, len(got), got, len(sorted), sorted)
	}
}

// The store census's `unresolved_citations n=<n>` line counts every block citation whose address
// resolves to no node, directly or through a moved_to tuple (this issue's spec R13): a node that
// cites an address from its text and from one of its blocks counts once, a citation of a block a
// document holds counts not at all, and a prose citation of a block no version holds counts. A
// citation of a block of a document that owes its block index, by store_owed_at or by a null
// version, is left out until the index stands, and so is one of a block of an issue room one of
// whose documents owes it, since the reconciliation indexes the room whole.
//
// A move adds nothing to it: once the decisions document's ask block stands in the spec with a
// moved_to from its old address, the citation of that address resolves through the tuple. With
// the tuple retracted, that citation resolves to no node and counts (the control), and with it
// asserted again it resolves. A block of the spec's fragment of an issue room, whose row the room
// holds under the tree's address, resolves through the room.
func TestTheUnresolvedCitationsLineCountsEveryCitationThatResolvesToNoNode(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	fixture := seedRestoreFixture(t, database)
	first, second, third := "55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666", "77777777-7777-4777-8777-777777777777"
	neverHeld := fixture.spec.refKey + "#never-held"
	oldAddress := fixture.decisions.refKey + "#ask-b"
	absent := fixture.notes.refKey + "#absent"
	citeFromComment(t, database, first, fixture.spec.refKey+"#held-x")
	citeFromComment(t, database, first, neverHeld)
	citeFromNotes(t, database, fixture.notes, neverHeld)
	citeFromComment(t, database, second, oldAddress)
	citeFromComment(t, database, second, absent)
	every := []string{
		"artifact:" + fixture.notes.id + " -> " + neverHeld,
		"comment:" + first + " -> " + neverHeld,
		"comment:" + second + " -> " + absent,
	}
	withoutNotes := every[:2]
	requireUnresolved(t, database, "before any move", every...)

	report, err := StoreCensus(ctx, database.Pool.Config().ConnString(), false, declining())
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	var printed strings.Builder
	report.WriteVerbose(&printed)
	for _, line := range []string{
		"census: unresolved_citations n=3\n",
		"census: unresolved_citation from=comment:" + second + " to=" + absent + "\n",
	} {
		if !strings.Contains(printed.String(), line) {
			t.Fatalf("the census prints no line %q:\n%s", line, printed.String())
		}
	}

	exec("mark notes.md owed by store_owed_at", `update document_block_versions set store_owed_at = now() where artifact_id = $1::uuid`, fixture.notes.id)
	requireUnresolved(t, database, "with notes.md owing its block index by store_owed_at", withoutNotes...)
	exec("mark notes.md owed by a null version", `update document_block_versions set store_owed_at = null, version = null where artifact_id = $1::uuid`, fixture.notes.id)
	requireUnresolved(t, database, "with notes.md owing its block index by a null version", withoutNotes...)
	exec("stand notes.md's block index again", `update document_block_versions set version = 1 where artifact_id = $1::uuid`, fixture.notes.id)
	requireUnresolved(t, database, "with notes.md's block index standing again", every...)

	// The move of the decisions document's ask block into the spec, as the restore makes it: the
	// block leaves the decisions document and stands in the spec, and the old address takes its
	// moved_to to the new one.
	exec("move the ask block out of the decisions document", `delete from document_blocks where artifact_id = $1::uuid and block_id = 'ask-b'`, fixture.decisions.id)
	insertTestBlock(t, database, fixture.spec, "ask-b", "ask")
	move := graphstore.Tuple{
		FromKind: refs.BlockNodeKind, FromID: oldAddress, Kind: graphstore.TupleMovedTo,
		ToKind: refs.BlockNodeKind, ToID: fixture.spec.refKey + "#ask-b", Asserted: true,
	}
	appendTuple(t, database, move)
	requireUnresolved(t, database, "after a move with its moved_to", every...)
	move.Asserted = false
	appendTuple(t, database, move)
	requireUnresolved(t, database, "after the move's moved_to is retracted", append(slices.Clone(every), "comment:"+second+" -> "+oldAddress)...)
	move.Asserted = true
	appendTuple(t, database, move)
	requireUnresolved(t, database, "with the move's moved_to asserted again", every...)

	// The spec and the decisions document stand in the issue's room, whose rows are keyed by its
	// tree artifact.
	var tree testDocument
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'tree', 'tree', 'tree', false, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`).Scan(&tree.id, &tree.refKey); err != nil {
		t.Fatalf("create the issue's tree: %v", err)
	}
	exec("move the spec and the decisions document into the issue room", `
		with held as (insert into issue_trees (issue_key, artifact_id) values ('CORE-1', $1::uuid))
		insert into document_rooms (artifact_id, room) values ($2::uuid, $1::uuid), ($3::uuid, $1::uuid)
	`, tree.id, fixture.spec.id, fixture.decisions.id)
	insertTestBlock(t, database, tree, "room-only", "paragraph")
	citeFromComment(t, database, third, fixture.spec.refKey+"#room-only")
	requireUnresolved(t, database, "with the spec in its issue room", every...)
	exec("mark the decisions document owed", `update document_block_versions set store_owed_at = now() where artifact_id = $1::uuid`, fixture.decisions.id)
	requireUnresolved(t, database, "with the decisions document, a fragment of the spec's room, owing its block index",
		"comment:"+second+" -> "+absent)
}
